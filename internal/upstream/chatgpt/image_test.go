package chatgpt

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestPrepareFConversationUsesAccountBrowserProfile exercises the real HTTP
// request boundary used immediately after reference upload. Expected: both the
// browser headers and timezone payload come from the persisted account profile
// rather than the process-wide default identity.
func TestPrepareFConversationUsesAccountBrowserProfile(t *testing.T) {
	profile := `{"version":1,"headers":{"Accept-Language":"en-US,en;q=0.9","Sec-Ch-Ua":"\"Chromium\";v=\"146\", \"Google Chrome\";v=\"146\"","Sec-Ch-Ua-Mobile":"?0","Sec-Ch-Ua-Platform":"\"Windows\"","User-Agent":"Mozilla/5.0 Chrome/146.0.0.0 Safari/537.36"},"language":"en-US","timezone":"America/Phoenix"}`
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/backend-api/f/conversation/prepare" {
			t.Fatalf("path = %q, want prepare endpoint", request.URL.Path)
		}
		if got := request.Header.Get("User-Agent"); got != "Mozilla/5.0 Chrome/146.0.0.0 Safari/537.36" {
			t.Fatalf("User-Agent = %q, want account profile", got)
		}
		if got := request.Header.Get("Openai-Sentinel-Turnstile-Token"); got != "turnstile-token" {
			t.Fatalf("turnstile token = %q, want turnstile-token", got)
		}
		if got := request.Header.Get("OpenAI-Sentinel-SO-Token"); got != "so-token" {
			t.Fatalf("SO token = %q, want so-token", got)
		}
		var body map[string]interface{}
		if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
			t.Fatalf("decode request body: %v", err)
		}
		if got := body["timezone"]; got != "America/Phoenix" {
			t.Fatalf("timezone = %v, want America/Phoenix", got)
		}
		if got := int(body["timezone_offset_min"].(float64)); got != 420 {
			t.Fatalf("timezone offset = %d, want 420", got)
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"conduit_token": "conduit"})
	}))
	defer server.Close()

	client, err := New(Options{
		BaseURL:        server.URL,
		AuthToken:      "test-auth",
		DeviceID:       "test-device",
		BrowserProfile: profile,
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	conduit, err := client.PrepareFConversation(context.Background(), ImageConvOpts{
		Prompt:         "test prompt",
		ChatToken:      "chat-token",
		ParentMsgID:    "parent-message",
		TurnstileToken: "turnstile-token",
		SOToken:        "so-token",
	})
	if err != nil {
		t.Fatalf("PrepareFConversation() error = %v", err)
	}
	if conduit != "conduit" {
		t.Fatalf("conduit = %q, want conduit", conduit)
	}
}

// TestNormalizePollOptsDefaultsUseTwoMinuteWait verifies a conversation that
// never exposes generated image refs exits early enough for the account cooldown
// and retry path to run, instead of leaving the caller in an empty running state.
func TestNormalizePollOptsDefaultsUseTwoMinuteWait(t *testing.T) {
	got := normalizePollOpts(PollOpts{})

	if got.ExpectedN != 1 {
		t.Fatalf("ExpectedN = %d, want 1", got.ExpectedN)
	}
	if got.MaxWait != 2*time.Minute {
		t.Fatalf("MaxWait = %s, want 2m", got.MaxWait)
	}
	if got.Interval != 3*time.Second {
		t.Fatalf("Interval = %s, want 3s", got.Interval)
	}
	if got.ImageTaskStarted {
		t.Fatal("ImageTaskStarted = true, want false")
	}
}

func TestNormalizePollOptsPreservesImageTaskStarted(t *testing.T) {
	got := normalizePollOpts(PollOpts{ImageTaskStarted: true})
	if !got.ImageTaskStarted {
		t.Fatal("ImageTaskStarted = false, want true")
	}
}

// TestListRecentConversationsUsesBoundedUpdatedOrder verifies recovery can find
// an already-created image conversation without submitting another turn.
// Expected: the client uses the authenticated list endpoint with a bounded
// limit and decodes only the stable identity/time summary fields.
func TestListRecentConversationsUsesBoundedUpdatedOrder(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/backend-api/conversations" {
			t.Fatalf("path = %q, want conversations endpoint", request.URL.Path)
		}
		query := request.URL.Query()
		if query.Get("offset") != "0" || query.Get("limit") != "10" ||
			query.Get("order") != "updated" || query.Get("conversation_filter") != "all" {
			t.Fatalf("query = %q", request.URL.RawQuery)
		}
		if request.Header.Get("Authorization") != "Bearer test-auth" {
			t.Fatalf("Authorization = %q", request.Header.Get("Authorization"))
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"items": []map[string]any{{
			"id": "conv-1", "title": "image", "create_time": "2026-08-12T23:00:00Z", "update_time": nil,
		}}})
	}))
	defer server.Close()

	client, err := New(Options{BaseURL: server.URL, AuthToken: "test-auth", DeviceID: "test-device"})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	items, err := client.ListRecentConversations(context.Background(), 10)
	if err != nil {
		t.Fatalf("ListRecentConversations() error = %v", err)
	}
	if len(items) != 1 || items[0].ID != "conv-1" {
		t.Fatalf("items = %+v", items)
	}
}

// TestListRecentConversationsAcceptsConversationIDAlias verifies the response
// projection tolerates the alternate stable identity key used by older list
// responses while ignoring unrelated schema drift.
func TestListRecentConversationsAcceptsConversationIDAlias(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"conversations":[{"conversation_id":"conv-alias","create_time":{"unexpected":true}}]}`))
	}))
	defer server.Close()
	client, err := New(Options{BaseURL: server.URL, AuthToken: "test-auth", DeviceID: "test-device"})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	items, err := client.ListRecentConversations(context.Background(), 10)
	if err != nil {
		t.Fatalf("ListRecentConversations() error = %v", err)
	}
	if len(items) != 1 || items[0].ID != "conv-alias" {
		t.Fatalf("items = %+v", items)
	}
}

// TestNormalizePollOptsCapsStartedImageTaskWait verifies an image task that has
// started but has not emitted any tool/image messages gets a bounded two-minute
// window. Complex image edits can legitimately take longer than the SSE startup
// budget, but should still leave time for account cooldown/retry before the bot
// HTTP timeout.
func TestNormalizePollOptsCapsStartedImageTaskWait(t *testing.T) {
	got := normalizePollOpts(PollOpts{
		MaxWait:          3 * time.Minute,
		ImageTaskStarted: true,
	})
	if got.MaxWait != 2*time.Minute {
		t.Fatalf("MaxWait = %s, want 2m for started empty image task", got.MaxWait)
	}
}

// TestPollConversationForImagesFailsFastAfterConsecutiveConversationErrors
// verifies an account that cannot read the conversation mapping is rejected
// quickly so the image runner can mark that account on cooldown and try another
// account, instead of holding the caller until the full poll MaxWait expires.
func TestPollConversationForImagesFailsFastAfterConsecutiveConversationErrors(t *testing.T) {
	var conversationRequests int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Fatalf("method = %s, want GET", r.Method)
		}
		switch r.URL.Path {
		case "/backend-api/tasks":
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"tasks": []interface{}{},
			})
		case "/backend-api/conversation/conv_forbidden":
			conversationRequests++
			http.Error(w, `{"detail":"conversation get failed"}`, http.StatusForbidden)
		default:
			t.Fatalf("path = %s, want /backend-api/tasks or /backend-api/conversation/conv_forbidden", r.URL.Path)
		}
	}))
	defer srv.Close()

	cli, err := New(Options{
		BaseURL:    srv.URL,
		AuthToken:  "test-auth",
		DeviceID:   "test-device",
		SessionID:  "test-session",
		Timeout:    5 * time.Second,
		SSETimeout: 5 * time.Second,
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	start := time.Now()
	status, fids, sids, dbg := cli.PollConversationForImages(context.Background(), "conv_forbidden", PollOpts{
		ExpectedN:        1,
		MaxWait:          30 * time.Second,
		Interval:         1 * time.Millisecond,
		ImageTaskStarted: true,
	})
	elapsed := time.Since(start)

	if status != PollStatusError {
		t.Fatalf("status = %s, want error", status)
	}
	if len(fids) != 0 || len(sids) != 0 {
		t.Fatalf("refs = (%v, %v), want none", fids, sids)
	}
	if dbg.ConversationErrors < 3 {
		t.Fatalf("ConversationErrors = %d, want at least 3", dbg.ConversationErrors)
	}
	if elapsed > time.Second {
		t.Fatalf("elapsed = %s, want fail-fast under 1s", elapsed)
	}
	if conversationRequests > 5 {
		t.Fatalf("conversation requests = %d, want fail-fast without polling until MaxWait", conversationRequests)
	}
}

// TestPollConversationForImagesReadsBackendTaskErrors verifies task-level
// image failures are visible even before the conversation mapping contains a
// final assistant error node. Expected: a structured /backend-api/tasks error
// stops polling immediately instead of waiting for the empty mapping budget.
func TestPollConversationForImagesReadsBackendTaskErrors(t *testing.T) {
	var taskRequests int
	var conversationRequests int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Fatalf("method = %s, want GET", r.Method)
		}
		switch r.URL.Path {
		case "/backend-api/tasks":
			taskRequests++
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"tasks": []interface{}{
					map[string]interface{}{
						"task_id":         "task_123",
						"conversation_id": "conv_task_error",
						"image_gen_message": map[string]interface{}{
							"id": "task_msg_123",
							"author": map[string]interface{}{
								"role": "assistant",
							},
							"content": map[string]interface{}{
								"content_type": "text",
								"parts": []interface{}{
									"非常抱歉，该提示可能违反了我们的内容政策。",
								},
							},
							"metadata": map[string]interface{}{
								"async_task_type": "image_gen",
								"async_task_id":   "imagegen_123",
								"is_error":        true,
							},
						},
					},
				},
			})
		case "/backend-api/conversation/conv_task_error":
			conversationRequests++
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"mapping": map[string]interface{}{},
			})
		default:
			t.Fatalf("unexpected path = %s", r.URL.Path)
		}
	}))
	defer srv.Close()

	cli, err := New(Options{
		BaseURL:    srv.URL,
		AuthToken:  "test-auth",
		DeviceID:   "test-device",
		SessionID:  "test-session",
		Timeout:    5 * time.Second,
		SSETimeout: 5 * time.Second,
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	start := time.Now()
	status, fids, sids, dbg := cli.PollConversationForImages(context.Background(), "conv_task_error", PollOpts{
		ExpectedN:        1,
		MaxWait:          50 * time.Millisecond,
		Interval:         1 * time.Millisecond,
		ImageTaskStarted: true,
	})
	elapsed := time.Since(start)

	if status != PollStatusRejected {
		t.Fatalf("status = %s, want rejected; dbg=%+v", status, dbg)
	}
	if len(fids) != 0 || len(sids) != 0 {
		t.Fatalf("refs = (%v, %v), want none", fids, sids)
	}
	if taskRequests == 0 {
		t.Fatal("tasks endpoint was not queried")
	}
	if conversationRequests != 0 {
		t.Fatalf("conversation requests = %d, want 0 after terminal task error", conversationRequests)
	}
	if dbg.AsyncErrorType != "content_policy" {
		t.Fatalf("AsyncErrorType = %q, want content_policy", dbg.AsyncErrorType)
	}
	if !strings.Contains(dbg.AsyncErrorText, "内容政策") {
		t.Fatalf("AsyncErrorText = %q, want content policy text", dbg.AsyncErrorText)
	}
	if dbg.AsyncErrorMessageID != "task_msg_123" {
		t.Fatalf("AsyncErrorMessageID = %q, want task_msg_123", dbg.AsyncErrorMessageID)
	}
	if elapsed > time.Second {
		t.Fatalf("elapsed = %s, want fail-fast under 1s", elapsed)
	}
}

// TestPollConversationForImagesDoesNotRejectToolArgumentTaskText verifies a
// ChatGPT task row that stores image tool arguments as an assistant
// metadata.is_error text message does not preempt the real image tool result.
// This mirrors the observed IMG2 shape where /backend-api/tasks can contain a
// JSON argument echo while the image is still delivered through conversation
// tool messages.
func TestPollConversationForImagesDoesNotRejectToolArgumentTaskText(t *testing.T) {
	var conversationRequests int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Fatalf("method = %s, want GET", r.Method)
		}
		switch r.URL.Path {
		case "/backend-api/tasks":
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"tasks": []interface{}{
					map[string]interface{}{
						"task_id":         "task_args",
						"conversation_id": "conv_task_args",
						"image_gen_message": map[string]interface{}{
							"id":     "task_msg_args",
							"author": map[string]interface{}{"role": "assistant"},
							"content": map[string]interface{}{
								"content_type": "text",
								"parts": []interface{}{
									`{"referenced_image_ids":["file_00000000d93871f8882b8b7bdd2a0611"],"size":"1024x1792","n":1,"prompt":"edit this image"}`,
								},
							},
							"metadata": map[string]interface{}{
								"async_task_type": "image_gen",
								"async_task_id":   "imagegen_args",
								"is_error":        true,
							},
						},
					},
				},
			})
		case "/backend-api/conversation/conv_task_args":
			conversationRequests++
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"mapping": map[string]interface{}{
					"tool_result": map[string]interface{}{
						"message": map[string]interface{}{
							"id":     "tool_result",
							"author": map[string]interface{}{"role": "tool"},
							"content": map[string]interface{}{
								"content_type": "multimodal_text",
								"parts": []interface{}{
									map[string]interface{}{"asset_pointer": "sediment://file_00000000c05071fdadf93e0894c5d43d"},
								},
							},
							"metadata": map[string]interface{}{
								"async_task_type": "image_gen",
							},
						},
					},
				},
			})
		default:
			t.Fatalf("unexpected path = %s", r.URL.Path)
		}
	}))
	defer srv.Close()

	cli, err := New(Options{
		BaseURL:    srv.URL,
		AuthToken:  "test-auth",
		DeviceID:   "test-device",
		SessionID:  "test-session",
		Timeout:    5 * time.Second,
		SSETimeout: 5 * time.Second,
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	status, fids, sids, dbg := cli.PollConversationForImages(context.Background(), "conv_task_args", PollOpts{
		ExpectedN:        1,
		MaxWait:          50 * time.Millisecond,
		Interval:         1 * time.Millisecond,
		ImageTaskStarted: true,
	})

	if status != PollStatusSuccess {
		t.Fatalf("status = %s, want success; dbg=%+v", status, dbg)
	}
	if len(fids) != 0 {
		t.Fatalf("fids = %v, want none", fids)
	}
	if len(sids) != 1 || sids[0] != "file_00000000c05071fdadf93e0894c5d43d" {
		t.Fatalf("sids = %v, want generated sediment id", sids)
	}
	if conversationRequests == 0 {
		t.Fatal("conversation endpoint was not queried after tool-argument task text")
	}
}

// TestPollConversationForImagesStopsAtFirstTechnicalTerminal verifies the
// production failure shape where an intent-only image turn never creates an
// image task and the conversation already contains the final technical error.
// Expected: the first conversation read returns immediately without consuming
// the remaining polling budget.
func TestPollConversationForImagesStopsAtFirstTechnicalTerminal(t *testing.T) {
	const terminalText = "由于我这边发生了错误，我未能生成图片。"
	var conversationRequests int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Fatalf("method = %s, want GET", r.Method)
		}
		switch r.URL.Path {
		case "/backend-api/tasks":
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"tasks": []interface{}{}})
		case "/backend-api/conversation/conv_technical_terminal":
			conversationRequests++
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"mapping": map[string]interface{}{
					"assistant_terminal": map[string]interface{}{
						"message": map[string]interface{}{
							"id":        "assistant_terminal",
							"author":    map[string]interface{}{"role": "assistant"},
							"content":   map[string]interface{}{"content_type": "text", "parts": []interface{}{terminalText}},
							"status":    "finished_successfully",
							"recipient": "all",
							"metadata":  map[string]interface{}{},
						},
					},
				},
			})
		default:
			t.Fatalf("unexpected path = %s", r.URL.Path)
		}
	}))
	defer srv.Close()

	cli, err := New(Options{
		BaseURL:    srv.URL,
		AuthToken:  "test-auth",
		DeviceID:   "test-device",
		SessionID:  "test-session",
		Timeout:    5 * time.Second,
		SSETimeout: 5 * time.Second,
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	start := time.Now()
	status, fids, sids, dbg := cli.PollConversationForImages(
		context.Background(),
		"conv_technical_terminal",
		PollOpts{
			ExpectedN:        1,
			MaxWait:          time.Second,
			Interval:         100 * time.Millisecond,
			ImageTaskStarted: false,
		},
	)
	elapsed := time.Since(start)

	if status != PollStatusRejected {
		t.Fatalf("status = %s, want rejected; dbg=%+v", status, dbg)
	}
	if len(fids) != 0 || len(sids) != 0 {
		t.Fatalf("refs = (%v, %v), want none", fids, sids)
	}
	if dbg.AssistantReplyText != terminalText {
		t.Fatalf("AssistantReplyText = %q, want exact technical terminal", dbg.AssistantReplyText)
	}
	if conversationRequests != 1 {
		t.Fatalf("conversation requests = %d, want exactly 1", conversationRequests)
	}
	if elapsed >= 500*time.Millisecond {
		t.Fatalf("elapsed = %s, want immediate return under 500ms", elapsed)
	}
}

func TestParseImageSSEIgnoresUserReferenceAssets(t *testing.T) {
	stream := make(chan SSEEvent, 4)
	stream <- SSEEvent{Data: mustJSON(t, map[string]interface{}{
		"v": map[string]interface{}{
			"conversation_id": "conv_123",
			"message": map[string]interface{}{
				"author": map[string]interface{}{"role": "user"},
				"content": map[string]interface{}{
					"content_type": "multimodal_text",
					"parts": []interface{}{
						map[string]interface{}{
							"content_type":  "image_asset_pointer",
							"asset_pointer": "file-service://input_ref_1",
						},
						"把这张图改成电影海报",
					},
				},
				"metadata": map[string]interface{}{},
			},
		},
	})}
	stream <- SSEEvent{Data: mustJSON(t, map[string]interface{}{
		"v": map[string]interface{}{
			"conversation_id": "conv_123",
			"message": map[string]interface{}{
				"author": map[string]interface{}{"role": "assistant"},
				"content": map[string]interface{}{
					"content_type": "text",
					"parts":        []interface{}{"正在生成图片"},
				},
				"metadata": map[string]interface{}{
					"image_gen_task_id": "task_123",
				},
			},
		},
	})}
	stream <- SSEEvent{Data: mustJSON(t, map[string]interface{}{
		"v": map[string]interface{}{
			"conversation_id": "conv_123",
			"message": map[string]interface{}{
				"author": map[string]interface{}{"role": "tool"},
				"content": map[string]interface{}{
					"content_type": "multimodal_text",
					"parts": []interface{}{
						map[string]interface{}{
							"asset_pointer": "file-service://generated_1",
						},
					},
				},
				"metadata": map[string]interface{}{
					"async_task_type": "image_gen",
				},
			},
		},
	})}
	stream <- SSEEvent{Data: []byte("[DONE]")}
	close(stream)

	got := ParseImageSSE(stream)
	if got.ConversationID != "conv_123" {
		t.Fatalf("ConversationID = %q, want conv_123", got.ConversationID)
	}
	if got.ImageGenTaskID != "task_123" {
		t.Fatalf("ImageGenTaskID = %q, want task_123", got.ImageGenTaskID)
	}
	if len(got.FileIDs) != 1 || got.FileIDs[0] != "generated_1" {
		t.Fatalf("FileIDs = %#v, want only generated output", got.FileIDs)
	}
}

func TestParseImageSSEUntilReadyReturnsWhenImageMetadataIsPollable(t *testing.T) {
	stream := make(chan SSEEvent, 4)
	stream <- SSEEvent{Data: mustJSON(t, map[string]interface{}{
		"type":            "resume_conversation_token",
		"conversation_id": "conv_pollable",
	})}
	stream <- SSEEvent{Data: mustJSON(t, map[string]interface{}{
		"type":            "server_ste_metadata",
		"conversation_id": "conv_pollable",
		"metadata": map[string]interface{}{
			"tool_invoked":  true,
			"turn_use_case": "image gen",
		},
	})}

	done := make(chan ImageSSEResult, 1)
	go func() {
		done <- ParseImageSSEUntilReady(stream, ImageSSEParseOpts{
			ExpectedN:          1,
			ReturnWhenPollable: true,
		})
	}()

	select {
	case got := <-done:
		if got.ConversationID != "conv_pollable" {
			t.Fatalf("ConversationID = %q, want conv_pollable", got.ConversationID)
		}
		if !got.ToolInvoked {
			t.Fatal("ToolInvoked = false, want true")
		}
		if got.TurnUseCase != "image gen" {
			t.Fatalf("TurnUseCase = %q, want image gen", got.TurnUseCase)
		}
		if !got.ShouldPollForImage() {
			t.Fatal("ShouldPollForImage() = false, want true")
		}
	case <-time.After(200 * time.Millisecond):
		t.Fatal("ParseImageSSEUntilReady did not return after pollable image metadata")
	}
}

// TestImageSSEResultSeparatesPollIntentFromStartedTask verifies that an image
// turn intent is enough to query the conversation, but is not evidence that an
// asynchronous image task actually started. Expected: intent-only metadata is
// pollable while HasStartedImageTask remains false until a task id is present.
func TestImageSSEResultSeparatesPollIntentFromStartedTask(t *testing.T) {
	intentOnly := ImageSSEResult{
		ConversationID: "conv_intent_only",
		TurnUseCase:    "image gen",
	}
	if !intentOnly.ShouldPollForImage() {
		t.Fatal("ShouldPollForImage() = false, want true for image turn intent")
	}
	if intentOnly.HasStartedImageTask() {
		t.Fatal("HasStartedImageTask() = true, want false without task id or image refs")
	}

	started := intentOnly
	started.ImageGenTaskID = "image_task_123"
	if !started.HasStartedImageTask() {
		t.Fatal("HasStartedImageTask() = false, want true with image task id")
	}
}

// TestParseImageSSEUntilReadyTimesOutWaitingForImageStartSignal verifies an
// image SSE stream that only exposes a conversation id but never starts an
// image task returns within the configured startup window instead of waiting
// for the transport read timeout.
func TestParseImageSSEUntilReadyTimesOutWaitingForImageStartSignal(t *testing.T) {
	stream := make(chan SSEEvent, 2)
	stream <- SSEEvent{Data: mustJSON(t, map[string]interface{}{
		"type":            "resume_conversation_token",
		"conversation_id": "conv_no_image_signal",
	})}

	done := make(chan ImageSSEResult, 1)
	go func() {
		done <- ParseImageSSEUntilReady(stream, ImageSSEParseOpts{
			ExpectedN:          1,
			ReturnWhenPollable: true,
			MaxWait:            20 * time.Millisecond,
		})
	}()

	select {
	case got := <-done:
		if got.ConversationID != "conv_no_image_signal" {
			t.Fatalf("ConversationID = %q, want conv_no_image_signal", got.ConversationID)
		}
		if !got.TimedOut {
			t.Fatal("TimedOut = false, want true")
		}
		if got.ShouldPollForImage() {
			t.Fatal("ShouldPollForImage() = true, want false")
		}
	case <-time.After(200 * time.Millisecond):
		t.Fatal("ParseImageSSEUntilReady kept waiting for a non-image SSE stream")
	}
}

// TestParseImageSSEUntilReadyPreservesSecretSafeReadFailureClass verifies the
// parser distinguishes transport failure mechanisms without retaining raw
// error text. Expected: each producer-owned class survives unchanged and the
// result remains safe to serialize in diagnostics.
func TestParseImageSSEUntilReadyPreservesSecretSafeReadFailureClass(t *testing.T) {
	classes := []SSEReadFailureClass{
		SSEReadFailureUnexpectedEOF,
		SSEReadFailureTimeout,
		SSEReadFailureContextCancelled,
		SSEReadFailureOtherIO,
	}
	for _, class := range classes {
		t.Run(string(class), func(t *testing.T) {
			stream := make(chan SSEEvent, 1)
			stream <- SSEEvent{Err: fmt.Errorf("secret %s detail", class), ReadFailure: class}
			close(stream)

			got := ParseImageSSE(stream)
			if got.Termination != ImageSSETerminationStreamError {
				t.Fatalf("Termination = %q, want stream_error", got.Termination)
			}
			if got.ReadFailure != class {
				t.Fatalf("ReadFailure = %q, want %q", got.ReadFailure, class)
			}
			raw, err := json.Marshal(got)
			if err != nil {
				t.Fatalf("json.Marshal() error = %v", err)
			}
			if strings.Contains(string(raw), "secret") {
				t.Fatalf("serialized result leaked raw error: %s", raw)
			}
		})
	}
}

// TestParseImageSSECapturesPlainAssistantText verifies plain assistant text is
// preserved as a terminal image request result, so the runner does not classify
// policy/refusal text as a retryable account startup failure.
func TestParseImageSSECapturesPlainAssistantText(t *testing.T) {
	stream := make(chan SSEEvent, 3)
	stream <- SSEEvent{Data: mustJSON(t, map[string]interface{}{
		"type":            "resume_conversation_token",
		"conversation_id": "conv_assistant_text",
	})}
	stream <- SSEEvent{Data: mustJSON(t, map[string]interface{}{
		"v": map[string]interface{}{
			"conversation_id": "conv_assistant_text",
			"message": map[string]interface{}{
				"author": map[string]interface{}{"role": "assistant"},
				"content": map[string]interface{}{
					"content_type": "text",
					"parts":        []interface{}{"I can't create that image."},
				},
				"metadata": map[string]interface{}{},
			},
		},
	})}
	stream <- SSEEvent{Data: []byte("[DONE]")}
	close(stream)

	got := ParseImageSSE(stream)
	if got.AssistantText != "I can't create that image." {
		t.Fatalf("AssistantText = %q, want refusal text", got.AssistantText)
	}
	if got.ShouldPollForImage() {
		t.Fatal("ShouldPollForImage() = true, want false for plain assistant text")
	}
}

func TestParseImageSSECapturesPatchAssetAfterImageToolMetadata(t *testing.T) {
	stream := make(chan SSEEvent, 5)
	stream <- SSEEvent{Data: mustJSON(t, map[string]interface{}{
		"type":            "resume_conversation_token",
		"conversation_id": "conv_patch",
	})}
	stream <- SSEEvent{Data: mustJSON(t, map[string]interface{}{
		"type":            "server_ste_metadata",
		"conversation_id": "conv_patch",
		"metadata": map[string]interface{}{
			"tool_invoked":  true,
			"turn_use_case": "image gen",
		},
	})}
	stream <- SSEEvent{Data: mustJSON(t, map[string]interface{}{
		"v": map[string]interface{}{
			"conversation_id": "conv_patch",
			"message": map[string]interface{}{
				"author": map[string]interface{}{"role": "user"},
				"content": map[string]interface{}{
					"content_type": "multimodal_text",
					"parts": []interface{}{
						map[string]interface{}{"asset_pointer": "file-service://input_ref_should_not_capture"},
					},
				},
			},
		},
	})}
	stream <- SSEEvent{Data: mustJSON(t, map[string]interface{}{
		"o": "patch",
		"v": []interface{}{
			map[string]interface{}{
				"p": "/message/content/parts/0/asset_pointer",
				"v": "file-service://generated_patch_1",
			},
		},
	})}
	stream <- SSEEvent{Data: []byte("[DONE]")}
	close(stream)

	got := ParseImageSSE(stream)
	if got.ConversationID != "conv_patch" {
		t.Fatalf("ConversationID = %q, want conv_patch", got.ConversationID)
	}
	if len(got.FileIDs) != 1 || got.FileIDs[0] != "generated_patch_1" {
		t.Fatalf("FileIDs = %#v, want only generated patch output", got.FileIDs)
	}
}

func TestStreamFConversationAndParseImageSSEIgnoresUserReferenceAssets(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Fatalf("method = %s, want POST", r.Method)
		}
		if r.URL.Path != "/backend-api/f/conversation" {
			t.Fatalf("path = %s, want /backend-api/f/conversation", r.URL.Path)
		}
		if got := r.Header.Get("Openai-Sentinel-Turnstile-Token"); got != "turnstile-token" {
			t.Fatalf("turnstile token header = %q, want finalized challenge token", got)
		}
		if got := r.Header.Get("OpenAI-Sentinel-SO-Token"); got != "so-token" {
			t.Fatalf("SO token header = %q, want finalized SO token", got)
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read request body: %v", err)
		}
		if !strings.Contains(string(body), "file-service://input_ref_1") {
			t.Fatalf("request body missing reference image asset pointer: %s", string(body))
		}

		w.Header().Set("Content-Type", "text/event-stream")
		flusher, _ := w.(http.Flusher)
		writeEvent := func(payload any) {
			_, _ = fmt.Fprintf(w, "data: %s\n\n", mustJSON(t, payload))
			if flusher != nil {
				flusher.Flush()
			}
		}
		writeEvent(map[string]interface{}{
			"v": map[string]interface{}{
				"conversation_id": "conv_http_123",
				"message": map[string]interface{}{
					"author": map[string]interface{}{"role": "user"},
					"content": map[string]interface{}{
						"content_type": "multimodal_text",
						"parts": []interface{}{
							map[string]interface{}{
								"content_type":  "image_asset_pointer",
								"asset_pointer": "file-service://input_ref_1",
							},
							"把这张图改成电影海报",
						},
					},
				},
			},
		})
		writeEvent(map[string]interface{}{
			"v": map[string]interface{}{
				"conversation_id": "conv_http_123",
				"message": map[string]interface{}{
					"author": map[string]interface{}{"role": "assistant"},
					"content": map[string]interface{}{
						"content_type": "text",
						"parts":        []interface{}{"正在生成图片"},
					},
					"metadata": map[string]interface{}{
						"image_gen_task_id": "task_http_123",
					},
				},
			},
		})
		writeEvent(map[string]interface{}{
			"v": map[string]interface{}{
				"conversation_id": "conv_http_123",
				"message": map[string]interface{}{
					"author": map[string]interface{}{"role": "tool"},
					"content": map[string]interface{}{
						"content_type": "multimodal_text",
						"parts": []interface{}{
							map[string]interface{}{
								"asset_pointer": "file-service://generated_http_1",
							},
						},
					},
					"metadata": map[string]interface{}{
						"async_task_type": "image_gen",
					},
				},
			},
		})
		_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
		if flusher != nil {
			flusher.Flush()
		}
	}))
	defer srv.Close()

	cli, err := New(Options{
		BaseURL:    srv.URL,
		AuthToken:  "test-auth",
		DeviceID:   "test-device",
		SessionID:  "test-session",
		Timeout:    5 * time.Second,
		SSETimeout: 5 * time.Second,
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	stream, err := cli.StreamFConversation(context.Background(), ImageConvOpts{
		Prompt:      "把这张图改成电影海报",
		ParentMsgID: "parent_123",
		MessageID:   "msg_123",
		ChatToken:   "chat_token",
		References: []*UploadedFile{
			{
				FileID:   "input_ref_1",
				FileName: "input.png",
				FileSize: 123,
				MimeType: "image/png",
				UseCase:  "multimodal",
				Width:    512,
				Height:   512,
			},
		},
		TurnstileToken: "turnstile-token",
		SOToken:        "so-token",
	})
	if err != nil {
		t.Fatalf("StreamFConversation() error = %v", err)
	}

	got := ParseImageSSE(stream)
	if got.ConversationID != "conv_http_123" {
		t.Fatalf("ConversationID = %q, want conv_http_123", got.ConversationID)
	}
	if got.ImageGenTaskID != "task_http_123" {
		t.Fatalf("ImageGenTaskID = %q, want task_http_123", got.ImageGenTaskID)
	}
	if len(got.FileIDs) != 1 || got.FileIDs[0] != "generated_http_1" {
		t.Fatalf("FileIDs = %#v, want only generated output", got.FileIDs)
	}
}

func TestExtractImageToolMsgsIncludesAssistantMetadataAssetPointers(t *testing.T) {
	mapping := map[string]interface{}{
		"assistant_image": map[string]interface{}{
			"message": map[string]interface{}{
				"author": map[string]interface{}{"role": "assistant"},
				"content": map[string]interface{}{
					"content_type": "text",
					"parts":        []interface{}{"正在生成图片"},
				},
				"metadata": map[string]interface{}{
					"async_task_type": "image_gen",
					"result": map[string]interface{}{
						"asset_pointer": "file-service://generated_metadata_1",
					},
				},
			},
		},
		"user_input": map[string]interface{}{
			"message": map[string]interface{}{
				"author": map[string]interface{}{"role": "user"},
				"content": map[string]interface{}{
					"content_type": "multimodal_text",
					"parts": []interface{}{
						map[string]interface{}{"asset_pointer": "file-service://input_ref_1"},
					},
				},
				"metadata": map[string]interface{}{},
			},
		},
	}

	msgs := ExtractImageToolMsgs(mapping)
	if len(msgs) != 1 {
		t.Fatalf("len(ExtractImageToolMsgs) = %d, want 1", len(msgs))
	}
	if len(msgs[0].FileIDs) != 1 || msgs[0].FileIDs[0] != "generated_metadata_1" {
		t.Fatalf("FileIDs = %#v, want assistant metadata output", msgs[0].FileIDs)
	}
}

func TestExtractImageErrorMsgsDetectsContentPolicyAsyncError(t *testing.T) {
	mapping := map[string]interface{}{
		"hidden": map[string]interface{}{
			"message": map[string]interface{}{
				"author": map[string]interface{}{"role": "assistant"},
				"content": map[string]interface{}{
					"content_type": "text",
					"parts":        []interface{}{"非常抱歉，该提示可能违反了我们的内容政策。如果你认为此判断有误，请重试或修改提示语。"},
				},
				"status":    "finished_successfully",
				"end_turn":  true,
				"recipient": "all",
				"metadata": map[string]interface{}{
					"is_visually_hidden_from_conversation": true,
				},
			},
		},
		"async_err": map[string]interface{}{
			"message": map[string]interface{}{
				"author": map[string]interface{}{"role": "assistant"},
				"content": map[string]interface{}{
					"content_type": "text",
					"parts":        []interface{}{"非常抱歉，该提示可能违反了我们的内容政策。如果你认为此判断有误，请重试或修改提示语。"},
				},
				"status":    "finished_successfully",
				"end_turn":  true,
				"recipient": "all",
				"metadata": map[string]interface{}{
					"async_task_type": "image_gen",
					"async_task_id":   "imagegen_123",
					"is_error":        true,
				},
			},
		},
	}

	msgs := ExtractImageErrorMsgs(mapping)
	if len(msgs) != 2 {
		t.Fatalf("len(ExtractImageErrorMsgs) = %d, want 2", len(msgs))
	}

	best := pickBestImageError(msgs)
	if best == nil {
		t.Fatal("pickBestImageError returned nil")
	}
	if best.AsyncTaskType != "image_gen" {
		t.Fatalf("best.AsyncTaskType = %q, want image_gen", best.AsyncTaskType)
	}
	if !best.IsError {
		t.Fatal("best.IsError = false, want true")
	}
	if best.ErrorType != "content_policy" {
		t.Fatalf("best.ErrorType = %q, want content_policy", best.ErrorType)
	}
	if !strings.Contains(best.Text, "内容政策") {
		t.Fatalf("best.Text = %q, want content policy refusal text", best.Text)
	}
}

func TestExtractAssistantTextRepliesDetectsPlainAssistantRefusal(t *testing.T) {
	mapping := map[string]interface{}{
		"assistant_reply": map[string]interface{}{
			"message": map[string]interface{}{
				"author": map[string]interface{}{"role": "assistant"},
				"content": map[string]interface{}{
					"content_type": "text",
					"parts":        []interface{}{"I can't create that image. If you have another request, let me know!"},
				},
				"status":    "finished_successfully",
				"recipient": "all",
				"metadata":  map[string]interface{}{},
			},
		},
		"context": map[string]interface{}{
			"message": map[string]interface{}{
				"author": map[string]interface{}{"role": "assistant"},
				"content": map[string]interface{}{
					"content_type": "model_editable_context",
				},
				"status":   "finished_successfully",
				"metadata": map[string]interface{}{},
			},
		},
	}

	msgs := ExtractAssistantTextReplies(mapping)
	if len(msgs) != 1 {
		t.Fatalf("len(ExtractAssistantTextReplies) = %d, want 1", len(msgs))
	}
	if !strings.Contains(msgs[0].Text, "can't create") {
		t.Fatalf("assistant reply text = %q, want refusal text", msgs[0].Text)
	}
}

func TestExtractImageErrorMsgsDetectsGuardrailsFraudScamRejection(t *testing.T) {
	const guardrailMsg = "We’re so sorry, but the image we created may violate our guardrails around potential fraudulent or scam activity. If you think we got it wrong, please retry or edit your prompt."
	mapping := map[string]interface{}{
		"err_msg_1": map[string]interface{}{
			"message": map[string]interface{}{
				"author": map[string]interface{}{"role": "assistant"},
				"content": map[string]interface{}{
					"content_type": "text",
					"parts":        []interface{}{guardrailMsg},
				},
				"metadata": map[string]interface{}{
					"async_task_type": "image_gen",
					"is_error":        true,
				},
				"status": "finished_successfully",
			},
		},
	}

	msgs := ExtractImageErrorMsgs(mapping)
	if len(msgs) != 1 {
		t.Fatalf("len(msgs) = %d, want 1", len(msgs))
	}
	if msgs[0].ErrorType != "content_policy" {
		t.Fatalf("msgs[0].ErrorType = %q, want content_policy", msgs[0].ErrorType)
	}
	if !IsContentPolicyText(guardrailMsg) {
		t.Fatalf("IsContentPolicyText(%q) = false, want true", guardrailMsg)
	}

	const policiesMsg = "We’re so sorry, but the prompt may violate our content policies. If you think we got it wrong, please retry or edit your prompt."
	if !IsContentPolicyText(policiesMsg) {
		t.Fatalf("IsContentPolicyText(%q) = false, want true", policiesMsg)
	}
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("json.Marshal() error = %v", err)
	}
	return b
}
