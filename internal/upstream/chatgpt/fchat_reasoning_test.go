package chatgpt

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// TestPrepareFChatIncludesThinkingEffort verifies that the text-chat prepare request now carries the same
// thinking model slug, turn trace id, and thinking_effort field observed in real ChatGPT Web requests.
func TestPrepareFChatIncludesThinkingEffort(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Fatalf("method = %s, want POST", r.Method)
		}
		if r.URL.Path != "/backend-api/f/conversation/prepare" {
			t.Fatalf("path = %s, want /backend-api/f/conversation/prepare", r.URL.Path)
		}
		if got := r.Header.Get("Openai-Sentinel-Chat-Requirements-Token"); got != "chat-token" {
			t.Fatalf("chat requirements token = %q, want %q", got, "chat-token")
		}
		if got := r.Header.Get("Openai-Sentinel-Proof-Token"); got != "proof-token" {
			t.Fatalf("proof token = %q, want %q", got, "proof-token")
		}
		if got := r.Header.Get("Openai-Sentinel-Turnstile-Token"); got != "turnstile-token" {
			t.Fatalf("turnstile token = %q, want %q", got, "turnstile-token")
		}
		if got := r.Header.Get("X-Oai-Turn-Trace-Id"); got != "trace-prepare" {
			t.Fatalf("turn trace id = %q, want %q", got, "trace-prepare")
		}
		if got := r.Header.Get("X-Conduit-Token"); got != "no-token" {
			t.Fatalf("x-conduit-token = %q, want %q", got, "no-token")
		}

		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read body: %v", err)
		}

		var payload map[string]any
		if err := json.Unmarshal(body, &payload); err != nil {
			t.Fatalf("unmarshal body: %v", err)
		}

		if got := payload["model"]; got != "gpt-5-5-thinking" {
			t.Fatalf("model = %#v, want %q", got, "gpt-5-5-thinking")
		}
		if got := payload["client_prepare_state"]; got != "none" {
			t.Fatalf("client_prepare_state = %#v, want %q", got, "none")
		}
		if got := payload["thinking_effort"]; got != "extended" {
			t.Fatalf("thinking_effort = %#v, want %q", got, "extended")
		}

		partialQuery, ok := payload["partial_query"].(map[string]any)
		if !ok {
			t.Fatalf("partial_query missing or wrong type: %#v", payload["partial_query"])
		}
		content, ok := partialQuery["content"].(map[string]any)
		if !ok {
			t.Fatalf("partial_query.content missing or wrong type: %#v", partialQuery["content"])
		}
		parts, ok := content["parts"].([]any)
		if !ok || len(parts) != 1 || parts[0] != "请只回答 ok" {
			t.Fatalf("partial_query.content.parts = %#v, want [\"请只回答 ok\"]", content["parts"])
		}

		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"conduit_token":"conduit-123"}`))
	}))
	defer srv.Close()

	cli, err := New(Options{
		BaseURL:    srv.URL,
		AuthToken:  "auth-token",
		DeviceID:   "device-id",
		SessionID:  "session-id",
		Timeout:    5 * time.Second,
		SSETimeout: 5 * time.Second,
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	conduit, err := cli.PrepareFChat(context.Background(), FChatOpts{
		UpstreamModel:  "gpt-5-5-thinking",
		Messages:       []ChatMessage{{Role: "user", Content: "请只回答 ok"}},
		ChatToken:      "chat-token",
		ProofToken:     "proof-token",
		TurnstileToken: "turnstile-token",
		ParentMsgID:    "parent-123",
		TurnTraceID:    "trace-prepare",
		ThinkingEffort: "extended",
	})
	if err != nil {
		t.Fatalf("PrepareFChat() error = %v", err)
	}
	if conduit != "conduit-123" {
		t.Fatalf("conduit = %q, want %q", conduit, "conduit-123")
	}
}

// TestStreamFChatIncludesThinkingEffort verifies that the text-chat stream request keeps the thinking slug,
// reuses the caller-supplied turn trace id, and forwards thinking_effort into the main f/conversation payload.
func TestStreamFChatIncludesThinkingEffort(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Fatalf("method = %s, want POST", r.Method)
		}
		if r.URL.Path != "/backend-api/f/conversation" {
			t.Fatalf("path = %s, want /backend-api/f/conversation", r.URL.Path)
		}
		if got := r.Header.Get("X-Oai-Turn-Trace-Id"); got != "trace-stream" {
			t.Fatalf("turn trace id = %q, want %q", got, "trace-stream")
		}
		if got := r.Header.Get("X-Conduit-Token"); got != "conduit-123" {
			t.Fatalf("x-conduit-token = %q, want %q", got, "conduit-123")
		}
		if got := r.Header.Get("Openai-Sentinel-Turnstile-Token"); got != "turnstile-token" {
			t.Fatalf("turnstile token = %q, want %q", got, "turnstile-token")
		}

		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read body: %v", err)
		}

		var payload map[string]any
		if err := json.Unmarshal(body, &payload); err != nil {
			t.Fatalf("unmarshal body: %v", err)
		}
		if got := payload["model"]; got != "gpt-5-5-thinking" {
			t.Fatalf("model = %#v, want %q", got, "gpt-5-5-thinking")
		}
		if got := payload["thinking_effort"]; got != "extended" {
			t.Fatalf("thinking_effort = %#v, want %q", got, "extended")
		}

		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer srv.Close()

	cli, err := New(Options{
		BaseURL:    srv.URL,
		AuthToken:  "auth-token",
		DeviceID:   "device-id",
		SessionID:  "session-id",
		Timeout:    5 * time.Second,
		SSETimeout: 5 * time.Second,
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	stream, err := cli.StreamFChat(context.Background(), FChatOpts{
		UpstreamModel:  "gpt-5-5-thinking",
		Messages:       []ChatMessage{{Role: "user", Content: "请只回答 ok"}},
		ParentMsgID:    "parent-123",
		ChatToken:      "chat-token",
		ProofToken:     "proof-token",
		TurnstileToken: "turnstile-token",
		ConduitToken:   "conduit-123",
		TurnTraceID:    "trace-stream",
		ThinkingEffort: "extended",
		SSETimeout:     5 * time.Second,
	})
	if err != nil {
		t.Fatalf("StreamFChat() error = %v", err)
	}

	ev, ok := <-stream
	if !ok {
		t.Fatal("stream closed before yielding [DONE]")
	}
	if string(ev.Data) != "[DONE]" {
		t.Fatalf("first event data = %q, want %q", string(ev.Data), "[DONE]")
	}
}

// TestSetLastUsedModelConfigSendsThinkingEffort verifies that the client can mirror ChatGPT Web's preflight
// settings PATCH so reasoning-capable turns can persist the chosen thinking model and effort before sending chat traffic.
func TestSetLastUsedModelConfigSendsThinkingEffort(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPatch {
			t.Fatalf("method = %s, want PATCH", r.Method)
		}
		if r.URL.Path != "/backend-api/settings/user_last_used_model_config" {
			t.Fatalf("path = %s, want /backend-api/settings/user_last_used_model_config", r.URL.Path)
		}
		if got := r.URL.Query().Get("model_slug"); got != "gpt-5-5-thinking" {
			t.Fatalf("model_slug = %q, want %q", got, "gpt-5-5-thinking")
		}
		if got := r.URL.Query().Get("thinking_effort"); got != "extended" {
			t.Fatalf("thinking_effort = %q, want %q", got, "extended")
		}

		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"success":true}`))
	}))
	defer srv.Close()

	cli, err := New(Options{
		BaseURL:    srv.URL,
		AuthToken:  "auth-token",
		DeviceID:   "device-id",
		SessionID:  "session-id",
		Timeout:    5 * time.Second,
		SSETimeout: 5 * time.Second,
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	if err := cli.SetLastUsedModelConfig(context.Background(), "gpt-5-5-thinking", "extended"); err != nil {
		t.Fatalf("SetLastUsedModelConfig() error = %v", err)
	}
}

// TestGetConversationStreamStatusCallsExpectedEndpoint verifies that the client can query the same
// stream_status endpoint ChatGPT Web uses while waiting for async thinking completions to settle.
func TestGetConversationStreamStatusCallsExpectedEndpoint(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Fatalf("method = %s, want GET", r.Method)
		}
		if r.URL.Path != "/backend-api/conversation/conv-123/stream_status" {
			t.Fatalf("path = %s, want /backend-api/conversation/conv-123/stream_status", r.URL.Path)
		}

		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"COMPLETE"}`))
	}))
	defer srv.Close()

	cli, err := New(Options{
		BaseURL:    srv.URL,
		AuthToken:  "auth-token",
		DeviceID:   "device-id",
		SessionID:  "session-id",
		Timeout:    5 * time.Second,
		SSETimeout: 5 * time.Second,
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	status, err := cli.GetConversationStreamStatus(context.Background(), "conv-123")
	if err != nil {
		t.Fatalf("GetConversationStreamStatus() error = %v", err)
	}
	if status != "COMPLETE" {
		t.Fatalf("status = %q, want %q", status, "COMPLETE")
	}
}

// TestStreamFChatRecoversContentAfterStreamHandoff verifies that a thinking-model handoff can be recovered
// through stream_status plus conversation mapping, so callers still receive the final assistant text.
func TestStreamFChatRecoversContentAfterStreamHandoff(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/backend-api/f/conversation":
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = w.Write([]byte(
				"data: \"v1\"\n\n" +
					"data: {\"type\":\"resume_conversation_token\",\"conversation_id\":\"conv-123\",\"token\":\"resume-token\"}\n\n" +
					"data: {\"type\":\"stream_handoff\",\"conversation_id\":\"conv-123\",\"turn_exchange_id\":\"turn-123\",\"options\":[{\"type\":\"resume_sse_endpoint\",\"topic_id\":\"conversation-turn-turn-123\"}]}\n\n" +
					"data: [DONE]\n\n",
			))
		case r.Method == http.MethodGet && r.URL.Path == "/backend-api/conversation/conv-123/stream_status":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"status":"COMPLETE"}`))
		case r.Method == http.MethodGet && r.URL.Path == "/backend-api/conversation/conv-123":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{
				"current_node":"assistant-1",
				"mapping":{
					"assistant-1":{
						"message":{
							"id":"assistant-1",
							"author":{"role":"assistant"},
							"recipient":"all",
							"create_time":1,
							"status":"finished_successfully",
							"content":{"content_type":"text","parts":["ok"]}
						}
					}
				}
			}`))
		default:
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
	}))
	defer srv.Close()

	cli, err := New(Options{
		BaseURL:    srv.URL,
		AuthToken:  "auth-token",
		DeviceID:   "device-id",
		SessionID:  "session-id",
		Timeout:    5 * time.Second,
		SSETimeout: 5 * time.Second,
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	stream, err := cli.StreamFChat(context.Background(), FChatOpts{
		UpstreamModel: "gpt-5-5-thinking",
		Messages:      []ChatMessage{{Role: "user", Content: "请只回答 ok"}},
		ParentMsgID:   "parent-123",
		ChatToken:     "chat-token",
		ProofToken:    "proof-token",
		ConduitToken:  "conduit-123",
		TurnTraceID:   "trace-stream",
		SSETimeout:    5 * time.Second,
	})
	if err != nil {
		t.Fatalf("StreamFChat() error = %v", err)
	}

	var (
		gotRecovered bool
		gotDone      bool
	)
	for ev := range stream {
		if ev.Err != nil {
			t.Fatalf("unexpected stream error: %v", ev.Err)
		}
		if string(ev.Data) == "[DONE]" {
			gotDone = true
			break
		}
		var payload map[string]any
		if err := json.Unmarshal(ev.Data, &payload); err != nil {
			continue
		}
		msg, _ := payload["message"].(map[string]any)
		content, _ := msg["content"].(map[string]any)
		parts, _ := content["parts"].([]any)
		if len(parts) == 1 && parts[0] == "ok" {
			gotRecovered = true
		}
	}

	if !gotRecovered {
		t.Fatal("stream did not emit recovered assistant content")
	}
	if !gotDone {
		t.Fatal("stream did not reach a final state")
	}
}

// TestStreamFChatRetriesMappingAfterStreamStatusComplete verifies that recovery does not stop on the first
// empty COMPLETE-state mapping fetch, because GPT-5.5 can expose the assistant text a beat later.
func TestStreamFChatRetriesMappingAfterStreamStatusComplete(t *testing.T) {
	var mappingCalls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/backend-api/f/conversation":
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = w.Write([]byte(
				"data: \"v1\"\n\n" +
					"data: {\"type\":\"resume_conversation_token\",\"conversation_id\":\"conv-456\",\"token\":\"resume-token\"}\n\n" +
					"data: {\"type\":\"stream_handoff\",\"conversation_id\":\"conv-456\",\"turn_exchange_id\":\"turn-456\",\"options\":[{\"type\":\"resume_sse_endpoint\",\"topic_id\":\"conversation-turn-turn-456\"}]}\n\n" +
					"data: [DONE]\n\n",
			))
		case r.Method == http.MethodGet && r.URL.Path == "/backend-api/conversation/conv-456/stream_status":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"status":"COMPLETE"}`))
		case r.Method == http.MethodGet && r.URL.Path == "/backend-api/conversation/conv-456":
			mappingCalls++
			w.Header().Set("Content-Type", "application/json")
			if mappingCalls == 1 {
				_, _ = w.Write([]byte(`{
					"current_node":"assistant-empty",
					"mapping":{
						"assistant-empty":{
							"message":{
								"id":"assistant-empty",
								"author":{"role":"assistant"},
								"recipient":"all",
								"create_time":1,
								"status":"finished_successfully",
								"content":{"content_type":"text","parts":[""]}
							}
						}
					}
				}`))
				return
			}
			_, _ = w.Write([]byte(`{
				"current_node":"assistant-final",
				"mapping":{
					"assistant-final":{
						"message":{
							"id":"assistant-final",
							"author":{"role":"assistant"},
							"recipient":"all",
							"create_time":2,
							"status":"finished_successfully",
							"content":{"content_type":"text","parts":["ok"]}
						}
					}
				}
			}`))
		default:
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
	}))
	defer srv.Close()

	cli, err := New(Options{
		BaseURL:    srv.URL,
		AuthToken:  "auth-token",
		DeviceID:   "device-id",
		SessionID:  "session-id",
		Timeout:    5 * time.Second,
		SSETimeout: 5 * time.Second,
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	stream, err := cli.StreamFChat(context.Background(), FChatOpts{
		UpstreamModel: "gpt-5-5-thinking",
		Messages:      []ChatMessage{{Role: "user", Content: "请只回答 ok"}},
		ParentMsgID:   "parent-456",
		ChatToken:     "chat-token",
		ProofToken:    "proof-token",
		ConduitToken:  "conduit-456",
		TurnTraceID:   "trace-stream-456",
		SSETimeout:    5 * time.Second,
	})
	if err != nil {
		t.Fatalf("StreamFChat() error = %v", err)
	}

	var recovered bool
	for ev := range stream {
		if ev.Err != nil {
			t.Fatalf("unexpected stream error: %v", ev.Err)
		}
		if string(ev.Data) == "[DONE]" {
			break
		}
		var payload map[string]any
		if err := json.Unmarshal(ev.Data, &payload); err != nil {
			continue
		}
		msg, _ := payload["message"].(map[string]any)
		content, _ := msg["content"].(map[string]any)
		parts, _ := content["parts"].([]any)
		if len(parts) == 1 && parts[0] == "ok" {
			recovered = true
		}
	}

	if !recovered {
		t.Fatal("stream did not recover assistant text after retrying mapping")
	}
	if mappingCalls < 2 {
		t.Fatalf("mappingCalls = %d, want at least 2", mappingCalls)
	}
}
