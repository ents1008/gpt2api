package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"strings"
	"testing"
	"time"

	"github.com/432539/gpt2api/internal/upstream/chatgpt"
)

type fakeClient struct {
	requirementsCalls    int
	prepareCalls         int
	streamCalls          int
	pollCalls            int
	downloadURLCalls     int
	fetchCalls           int
	uploadCalls          int
	imageChatToken       string
	imageModel           string
	prepareOptions       chatgpt.ImageConvOpts
	streamOptions        chatgpt.ImageConvOpts
	features             chatgpt.AccountFeatures
	stream               []chatgpt.SSEEvent
	image                []byte
	pollStatus           chatgpt.PollStatus
	pollFileIDs          []string
	recentConversations  []chatgpt.ConversationSummary
	conversationMappings map[string]map[string]interface{}
	listCalls            int
	mappingCalls         int
	listErr              error
	fetchErrors          []error
	uploadedReference    *chatgpt.UploadedFile
}

func (client *fakeClient) UploadFile(_ context.Context, data []byte, fileName string) (*chatgpt.UploadedFile, error) {
	client.uploadCalls++
	if client.uploadedReference != nil {
		return client.uploadedReference, nil
	}
	return &chatgpt.UploadedFile{
		FileID: "uploaded-reference", FileName: fileName, FileSize: len(data), MimeType: "image/png",
	}, nil
}

func (client *fakeClient) ListRecentConversations(context.Context, int) ([]chatgpt.ConversationSummary, error) {
	client.listCalls++
	return client.recentConversations, client.listErr
}

func (client *fakeClient) GetConversationMapping(_ context.Context, conversationID string) (map[string]interface{}, error) {
	client.mappingCalls++
	return client.conversationMappings[conversationID], nil
}

func (client *fakeClient) GetAccountFeatures(context.Context) (chatgpt.AccountFeatures, error) {
	if client.features.FeatureCount == 0 {
		return chatgpt.AccountFeatures{CanAccessWithSession: true, FeatureCount: 1, ImageFeatures: []string{"gpt_image_1"}, ImageGenerationEnabled: true}, nil
	}
	return client.features, nil
}

func (client *fakeClient) ChatRequirementsV2(context.Context) (*chatgpt.ChatRequirementsResp, error) {
	client.requirementsCalls++
	return &chatgpt.ChatRequirementsResp{
		Token:   fmt.Sprintf("chat-token-%d", client.requirementsCalls),
		SOToken: fmt.Sprintf("so-token-%d", client.requirementsCalls),
		Persona: "chatgpt-freeaccount",
	}, nil
}

func (client *fakeClient) PrepareFConversation(_ context.Context, options chatgpt.ImageConvOpts) (string, error) {
	client.prepareCalls++
	client.prepareOptions = options
	return "conduit-token", nil
}

func (client *fakeClient) StreamFConversation(_ context.Context, options chatgpt.ImageConvOpts) (<-chan chatgpt.SSEEvent, error) {
	client.streamCalls++
	client.imageChatToken = options.ChatToken
	client.imageModel = options.UpstreamModel
	client.streamOptions = options
	stream := make(chan chatgpt.SSEEvent, len(client.stream))
	for _, event := range client.stream {
		stream <- event
	}
	close(stream)
	return stream, nil
}

func (client *fakeClient) PollConversationForImages(context.Context, string, chatgpt.PollOpts) (chatgpt.PollStatus, []string, []string, chatgpt.PollDebug) {
	client.pollCalls++
	return client.pollStatus, client.pollFileIDs, nil, chatgpt.PollDebug{Attempts: 1}
}

func (client *fakeClient) ImageDownloadURL(context.Context, string, string) (string, error) {
	client.downloadURLCalls++
	return "https://signed.example/image", nil
}

func (client *fakeClient) FetchImage(context.Context, string, int64) ([]byte, string, error) {
	client.fetchCalls++
	if len(client.fetchErrors) > 0 {
		errorValue := client.fetchErrors[0]
		client.fetchErrors = client.fetchErrors[1:]
		if errorValue != nil {
			return nil, "", errorValue
		}
	}
	return client.image, "image/png", nil
}

func validInputJSON() string {
	input := acceptanceInput{
		AuthToken: "auth", DeviceID: "device", SessionID: "session",
		Cookies:        "[]",
		BrowserProfile: chatgpt.DefaultBrowserProfileJSON(),
	}
	raw, _ := json.Marshal(input)
	return string(raw)
}

func pngBytes(t *testing.T) []byte {
	t.Helper()
	var buffer bytes.Buffer
	img := image.NewRGBA(image.Rect(0, 0, 2, 3))
	img.Set(0, 0, color.White)
	if err := png.Encode(&buffer, img); err != nil {
		t.Fatal(err)
	}
	return buffer.Bytes()
}

// Purpose: measure sentinel control-plane traffic without consuming an image
// generation. Expected: exactly one complete requirements exchange runs and
// no feature, upload, prepare, stream, poll, or download stage is entered.
func TestRunRequirementsOnlyStopsBeforeImageTurn(t *testing.T) {
	client := &fakeClient{}
	result := runRequirementsOnly(context.Background(), client)
	if !result.OK || len(result.Phases) != 1 || result.Phases[0].Stage != "pre_image_requirements" {
		t.Fatalf("requirements-only result = %+v", result)
	}
	if client.requirementsCalls != 1 || client.uploadCalls != 0 || client.prepareCalls != 0 ||
		client.streamCalls != 0 || client.pollCalls != 0 || client.downloadURLCalls != 0 || client.fetchCalls != 0 {
		t.Fatalf("requirements-only calls = %+v", client)
	}
}

// Purpose: ensure the acceptance boundary cannot receive multiple identities
// or inject a proxy credential. Expected: reject before creating a client or
// performing network I/O; runtime proxy ownership remains the server-side id.
func TestReadInputRequiresExactlyOneCompleteIdentity(t *testing.T) {
	if _, err := readInput(strings.NewReader("[" + validInputJSON() + "]")); err == nil {
		t.Fatal("array input was accepted")
	}
	var injected map[string]any
	_ = json.Unmarshal([]byte(validInputJSON()), &injected)
	injected["proxy_url"] = "http://secret.invalid:8080"
	raw, _ := json.Marshal(injected)
	if _, err := readInput(bytes.NewReader(raw)); err == nil || err.Error() != "invalid_json" {
		t.Fatalf("injected proxy error = %v", err)
	}
}

// Purpose: prevent resume mode from searching history without a bounded
// externally derived run window. Expected: missing and over-wide windows are
// rejected before client creation or network I/O.
func TestReadInputRequiresBoundedWindowForResume(t *testing.T) {
	var input map[string]interface{}
	if err := json.Unmarshal([]byte(validInputJSON()), &input); err != nil {
		t.Fatal(err)
	}
	input["mode"] = "resume_existing_image"
	input["prior_result"] = successfulPrefixWithDownloadTimeout()
	for _, window := range []struct {
		start float64
		end   float64
	}{
		{},
		{start: 100, end: 161},
		{start: 101, end: 100},
	} {
		input["window_start"] = window.start
		input["window_end"] = window.end
		raw, _ := json.Marshal(input)
		if _, err := readInput(bytes.NewReader(raw)); err == nil || err.Error() != "invalid_acceptance_window" {
			t.Fatalf("window (%v, %v) error = %v", window.start, window.end, err)
		}
	}
}

// Purpose: encode the only successful trajectory: one fresh pre-image
// requirements transaction, one image turn, one signed-URL request, one byte
// download/decode, and one post-image requirements check. Expected: each
// production-equivalent costly operation runs once and prepare/stream share
// one explicit turn identity.
func TestRunAcceptanceUsesOneImageTurnAndOneDownload(t *testing.T) {
	client := &fakeClient{
		stream: []chatgpt.SSEEvent{{Data: []byte(`{"conversation_id":"conv","message":{"author":{"role":"tool"},"content":{"parts":["file-service://file1"]}}}`)}},
		image:  pngBytes(t),
	}
	result := runAcceptance(context.Background(), client)
	if !result.OK {
		t.Fatalf("acceptance failed: %+v", result)
	}
	if client.requirementsCalls != 2 || client.prepareCalls != 1 || client.streamCalls != 1 ||
		client.pollCalls != 0 || client.downloadURLCalls != 1 || client.fetchCalls != 1 {
		t.Fatalf("unexpected calls: %+v", client)
	}
	if client.imageChatToken != "chat-token-1" {
		t.Fatalf("image turn token = %q, want only pre-image token", client.imageChatToken)
	}
	if client.prepareOptions.SOToken != "so-token-1" || client.streamOptions.SOToken != "so-token-1" {
		t.Fatalf("image turn SO token drifted: prepare=%q stream=%q", client.prepareOptions.SOToken, client.streamOptions.SOToken)
	}
	if preImagePhase := result.Phases[1]; !preImagePhase.SOTokenPresent {
		t.Fatalf("pre-image requirements omitted SO token presence: %+v", preImagePhase)
	}
	if client.imageModel != upstreamModel {
		t.Fatalf("image turn model = %q, want %q", client.imageModel, upstreamModel)
	}
	if client.prepareOptions.ParentMsgID == "" || client.prepareOptions.MessageID == "" {
		t.Fatalf("prepare turn identity is incomplete: %+v", client.prepareOptions)
	}
	if client.prepareOptions.ParentMsgID != client.streamOptions.ParentMsgID ||
		client.prepareOptions.MessageID != client.streamOptions.MessageID {
		t.Fatalf("prepare/stream turn identity drifted: prepare=%+v stream=%+v", client.prepareOptions, client.streamOptions)
	}
	decoded := result.Phases[len(result.Phases)-2]
	if decoded.Stage != "image_decode" || decoded.Format != "png" || decoded.Width != 2 || decoded.Height != 3 {
		t.Fatalf("decode phase = %+v", decoded)
	}
}

// Purpose: exercise the production image-edit shape through the same
// acceptance boundary. Expected: one reference upload is attached to the one
// prepared/streamed image turn, followed by the normal single image download.
func TestRunAcceptanceWithReferenceUploadsOnceAndAttachesToTurn(t *testing.T) {
	client := &fakeClient{
		stream: []chatgpt.SSEEvent{{Data: []byte(`{"conversation_id":"conv","message":{"author":{"role":"tool"},"content":{"parts":["file-service://file1"]}}}`)}},
		image:  pngBytes(t),
	}
	reference := &acceptanceReference{Data: pngBytes(t), FileName: "reference.png"}
	result := runAcceptanceWithReference(context.Background(), client, reference)
	if !result.OK {
		t.Fatalf("image-edit acceptance failed: %+v", result)
	}
	if client.uploadCalls != 1 || client.prepareCalls != 1 || client.streamCalls != 1 || client.fetchCalls != 1 {
		t.Fatalf("unexpected image-edit calls: %+v", client)
	}
	if len(client.prepareOptions.References) != 1 || len(client.streamOptions.References) != 1 ||
		client.prepareOptions.References[0].FileID != "uploaded-reference" ||
		client.streamOptions.References[0].FileID != "uploaded-reference" {
		t.Fatalf("reference attachment drifted: prepare=%+v stream=%+v", client.prepareOptions, client.streamOptions)
	}
	if result.Phases[2].Stage != "reference_upload" || !result.Phases[2].OK {
		t.Fatalf("reference upload phase = %+v", result.Phases[2])
	}
}

// Purpose: prevent empty or text-only SSE output from entering a long poll.
// Expected: polling is allowed only after a real image-task signal.
func TestRunAcceptanceDoesNotPollWithoutImageIntent(t *testing.T) {
	client := &fakeClient{stream: []chatgpt.SSEEvent{{Data: []byte(`{"conversation_id":"conv","message":{"author":{"role":"assistant"},"content":{"content_type":"text","parts":["hello"]}}}`)}}}
	result := runAcceptance(context.Background(), client)
	if result.OK || client.pollCalls != 0 || client.downloadURLCalls != 0 || client.fetchCalls != 0 {
		t.Fatalf("unexpected acceptance state: result=%+v client=%+v", result, client)
	}
	phase := result.Phases[len(result.Phases)-1]
	if phase.Stage != "image_turn" || phase.SSETermination != "stream_closed" ||
		!phase.ConversationIDPresent || !phase.AssistantTextPresent ||
		phase.AssistantTextLength != len("hello") || phase.ToolInvoked {
		t.Fatalf("image turn evidence = %+v", phase)
	}
}

// Purpose: distinguish a clean upstream terminal marker from a transport close
// when no image tool starts. Expected: [DONE] is reported without exposing any
// upstream assistant content or credentials.
func TestRunAcceptanceReportsDoneWithoutImageTask(t *testing.T) {
	client := &fakeClient{stream: []chatgpt.SSEEvent{{Data: []byte("[DONE]")}}}
	result := runAcceptance(context.Background(), client)
	if result.OK {
		t.Fatalf("unexpected success: %+v", result)
	}
	phase := result.Phases[len(result.Phases)-1]
	if phase.SSETermination != "done" || phase.AssistantTextPresent || phase.ConversationIDPresent {
		t.Fatalf("image turn evidence = %+v", phase)
	}
}

// Purpose: preserve the distinction between a parser/transport failure and a
// normal upstream completion. Expected: only a fixed stream_error marker is
// emitted; the underlying error text never enters the result.
func TestRunAcceptanceReportsStreamErrorWithoutLeakingMessage(t *testing.T) {
	client := &fakeClient{stream: []chatgpt.SSEEvent{{
		Err:         errors.New("secret stream detail"),
		ReadFailure: chatgpt.SSEReadFailureUnexpectedEOF,
	}}}
	result := runAcceptance(context.Background(), client)
	phase := result.Phases[len(result.Phases)-1]
	serialized, _ := json.Marshal(phase)
	if result.OK || phase.SSETermination != "stream_error" ||
		phase.SSEReadFailure != "unexpected_eof" ||
		strings.Contains(string(serialized), "secret stream detail") {
		t.Fatalf("image turn evidence = %s", serialized)
	}
}

// Purpose: cover the legitimate asynchronous image trajectory where SSE emits
// a task signal but no asset reference. Expected: exactly one bounded poll is
// used, followed by the same single download path.
func TestRunAcceptancePollsOnlyAfterTaskSignal(t *testing.T) {
	client := &fakeClient{
		stream:      []chatgpt.SSEEvent{{Data: []byte(`{"conversation_id":"conv","message":{"metadata":{"image_gen_task_id":"task"}}}`)}},
		pollStatus:  chatgpt.PollStatusSuccess,
		pollFileIDs: []string{"file1"},
		image:       pngBytes(t),
	}
	result := runAcceptance(context.Background(), client)
	if !result.OK || client.pollCalls != 1 || client.fetchCalls != 1 {
		t.Fatalf("unexpected acceptance state: result=%+v client=%+v", result, client)
	}
}

// Purpose: prove a signed URL and non-empty bytes are insufficient evidence.
// Expected: undecodable bytes fail before the post-image health check.
func TestRunAcceptanceRejectsUndecodableImageBytes(t *testing.T) {
	client := &fakeClient{
		stream: []chatgpt.SSEEvent{{Data: []byte(`{"conversation_id":"conv","message":{"author":{"role":"tool"},"content":{"parts":["file-service://file1"]}}}`)}},
		image:  []byte("not an image"),
	}
	result := runAcceptance(context.Background(), client)
	if result.OK || client.fetchCalls != 1 || client.requirementsCalls != 1 {
		t.Fatalf("unexpected acceptance state: result=%+v client=%+v", result, client)
	}
}

// Purpose: continue the exact partially successful acceptance after only the
// CDN byte fetch failed. Expected: recovery uniquely matches the existing
// prompt-bearing conversation, downloads and decodes that image, performs the
// post-image requirements check, and never prepares or streams another turn.
func TestResumeAcceptanceDownloadsExistingImageWithoutNewTurn(t *testing.T) {
	prior := successfulPrefixWithDownloadTimeout()
	client := &fakeClient{
		recentConversations: []chatgpt.ConversationSummary{{ID: "conv-existing"}},
		conversationMappings: map[string]map[string]interface{}{
			"conv-existing": existingAcceptanceConversation("file-existing"),
		},
		image: pngBytes(t),
	}
	result := resumeAcceptance(
		context.Background(),
		client,
		&prior,
		identityDigest("session"),
		acceptanceWindow{Start: 100, End: 130},
	)
	if !result.OK {
		t.Fatalf("resume acceptance failed: %+v", result)
	}
	if client.prepareCalls != 0 || client.streamCalls != 0 || client.pollCalls != 0 {
		t.Fatalf("resume submitted a new image turn: %+v", client)
	}
	if client.listCalls != 1 || client.mappingCalls != 1 ||
		client.downloadURLCalls != 1 || client.fetchCalls != 1 || client.requirementsCalls != 1 {
		t.Fatalf("resume calls = %+v", client)
	}
	stages := make([]string, 0, len(result.Phases))
	for _, phase := range result.Phases {
		stages = append(stages, phase.Stage)
	}
	want := []string{"account_features", "pre_image_requirements", "conversation_prepare", "image_turn", "download_url", "image_decode", "post_image_requirements"}
	if fmt.Sprint(stages) != fmt.Sprint(want) {
		t.Fatalf("stages = %v, want %v", stages, want)
	}
}

// Purpose: automate only the safe continuation after a generated image's CDN
// download times out. Expected: one image stream is submitted, the existing
// conversation is uniquely recovered inside the captured turn window, and a
// second byte fetch completes without preparing or streaming another turn.
func TestRunAutomaticAcceptanceRecoversDownloadTimeoutWithoutSecondTurn(t *testing.T) {
	createdAt := float64(time.Now().UnixMilli()) / 1000
	client := &fakeClient{
		stream:              []chatgpt.SSEEvent{{Data: []byte(`{"conversation_id":"conv","message":{"author":{"role":"tool"},"content":{"parts":["file-service://file1"]}}}`)}},
		image:               pngBytes(t),
		fetchErrors:         []error{context.DeadlineExceeded, nil},
		recentConversations: []chatgpt.ConversationSummary{{ID: "conv"}},
		conversationMappings: map[string]map[string]interface{}{
			"conv": existingAcceptanceConversationAt("file1", createdAt),
		},
	}

	result := runAutomaticAcceptance(context.Background(), client, identityDigest("session"))
	if !result.OK {
		t.Fatalf("automatic acceptance failed: %+v", result)
	}
	if client.streamCalls != 1 || client.prepareCalls != 1 || client.fetchCalls != 2 {
		t.Fatalf("automatic calls = %+v", client)
	}
	if client.listCalls != 1 || client.mappingCalls != 1 || client.downloadURLCalls != 2 {
		t.Fatalf("recovery calls = %+v", client)
	}
}

// Purpose: prevent recovery from guessing among history or accepting a result
// from another browser identity. Expected: a digest mismatch or more than one
// exact prompt-bearing image conversation fails before URL lookup/download.
func TestResumeAcceptanceRejectsWrongIdentityAndAmbiguousConversation(t *testing.T) {
	prior := successfulPrefixWithDownloadTimeout()
	client := &fakeClient{
		recentConversations: []chatgpt.ConversationSummary{{ID: "conv-1"}, {ID: "conv-2"}},
		conversationMappings: map[string]map[string]interface{}{
			"conv-1": existingAcceptanceConversationAt("file-1", 110),
			"conv-2": existingAcceptanceConversationAt("file-2", 120),
		},
		image: pngBytes(t),
	}
	window := acceptanceWindow{Start: 100, End: 130}
	wrong := resumeAcceptance(context.Background(), client, &prior, identityDigest("another-session"), window)
	if wrong.OK || client.listCalls != 0 {
		t.Fatalf("wrong identity reached conversation history: result=%+v client=%+v", wrong, client)
	}
	ambiguous := resumeAcceptance(context.Background(), client, &prior, identityDigest("session"), window)
	if ambiguous.OK || client.downloadURLCalls != 0 || client.fetchCalls != 0 {
		t.Fatalf("ambiguous recovery downloaded an image: result=%+v client=%+v", ambiguous, client)
	}
}

// Purpose: keep a failed history lookup diagnosable without exposing an
// upstream response body. Expected: the existing fixed HTTP class/status is
// preserved and secret response text never enters the serialized phase.
func TestResumeAcceptanceClassifiesConversationListFailureWithoutLeakingBody(t *testing.T) {
	prior := successfulPrefixWithDownloadTimeout()
	client := &fakeClient{listErr: &chatgpt.UpstreamError{
		Status: 403, Message: "secret message", Body: "secret body",
	}}
	result := resumeAcceptance(
		context.Background(), client, &prior, identityDigest("session"),
		acceptanceWindow{Start: 100, End: 130},
	)
	phase := result.Phases[len(result.Phases)-1]
	raw, _ := json.Marshal(phase)
	if phase.Status != 403 || phase.ErrorClass != "forbidden" || strings.Contains(string(raw), "secret") {
		t.Fatalf("conversation recovery phase = %s", raw)
	}
}

func successfulPrefixWithDownloadTimeout() acceptanceResult {
	return acceptanceResult{
		IdentityDigest: identityDigest("session"),
		Phases: []phaseResult{
			{Stage: "account_features", OK: true, ImageGenerationEnabled: true, CanAccessWithSession: true},
			{Stage: "pre_image_requirements", OK: true},
			{Stage: "conversation_prepare", OK: true},
			{Stage: "image_turn", OK: true, SSETermination: "ready", ConversationIDPresent: true},
			{Stage: "download_url", OK: true},
			{Stage: "image_download", ErrorClass: "timeout"},
		},
	}
}

func existingAcceptanceConversation(fileID string) map[string]interface{} {
	return existingAcceptanceConversationAt(fileID, 110)
}

func existingAcceptanceConversationAt(fileID string, createdAt float64) map[string]interface{} {
	return map[string]interface{}{"mapping": map[string]interface{}{
		"user": map[string]interface{}{"message": map[string]interface{}{
			"author":      map[string]interface{}{"role": "user"},
			"content":     map[string]interface{}{"content_type": "text", "parts": []interface{}{acceptanceText}},
			"create_time": createdAt,
		}},
		"tool": map[string]interface{}{"message": map[string]interface{}{
			"author":   map[string]interface{}{"role": "tool"},
			"content":  map[string]interface{}{"content_type": "multimodal_text", "parts": []interface{}{"file-service://" + fileID}},
			"metadata": map[string]interface{}{"async_task_type": "image_gen"},
		}},
	}}
}

// Purpose: resolve repeated historical acceptance prompts only with the
// externally preserved run window. Expected: one in-window user message wins;
// an otherwise identical older conversation is ignored without list-order
// guessing.
func TestResumeAcceptanceUsesBoundedRunWindowToResolveRepeatedPrompt(t *testing.T) {
	prior := successfulPrefixWithDownloadTimeout()
	client := &fakeClient{
		recentConversations: []chatgpt.ConversationSummary{{ID: "old"}, {ID: "current"}},
		conversationMappings: map[string]map[string]interface{}{
			"old":     existingAcceptanceConversationAt("file-old", 50),
			"current": existingAcceptanceConversationAt("file-current", 110),
		},
		image: pngBytes(t),
	}
	result := resumeAcceptance(
		context.Background(), client, &prior, identityDigest("session"),
		acceptanceWindow{Start: 100, End: 130},
	)
	if !result.OK || client.downloadURLCalls != 1 || client.fetchCalls != 1 {
		t.Fatalf("windowed recovery = %+v, client=%+v", result, client)
	}
}

// Purpose: bind a secret-free acceptance result to one exact browser session
// without serializing the session UUID. Expected: the digest is deterministic,
// domain-separated, and contains no source identifier.
func TestIdentityDigestIsStableAndSecretFree(t *testing.T) {
	const sessionID = "9e3b5a1d-b995-4da2-8980-03b3c4b12d90"
	first := identityDigest(sessionID)
	second := identityDigest(sessionID)
	if first != second || len(first) != 64 || strings.Contains(first, sessionID) {
		t.Fatalf("identity digest = %q", first)
	}
}

// Purpose: ensure diagnostic classification cannot expose upstream bodies,
// proxy credentials, tokens, or prompt text. Expected: only fixed error types
// and HTTP status survive.
func TestClassifyErrorEmitsTypedMetadataOnly(t *testing.T) {
	secret := "secret-token-proxy-prompt"
	status, class := classifyError(&chatgpt.UpstreamError{Status: 403, Message: secret, Body: secret})
	if status != 403 || class != "forbidden" || strings.Contains(class, secret) {
		t.Fatalf("classification = (%d, %q)", status, class)
	}
	status, class = classifyError(errors.New(secret))
	if status != 0 || class != "local_error" {
		t.Fatalf("local classification = (%d, %q)", status, class)
	}
}
