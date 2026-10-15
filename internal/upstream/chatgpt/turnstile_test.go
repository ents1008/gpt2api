package chatgpt

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestSolveTurnstileTokenExecutesEncodedProgram verifies the Go solver can run
// the same XOR/base64 encoded instruction stream used by the Python reference
// implementation, including concat and conditional function dispatch.
func TestSolveTurnstileTokenExecutesEncodedProgram(t *testing.T) {
	p := "test-source-p"
	dx := encodeTurnstileProgram(t, []any{
		[]any{2, 100, "hello"},
		[]any{2, 101, "-"},
		[]any{5, 100, 101},
		[]any{2, 102, "world"},
		[]any{5, 100, 102},
		[]any{20, 100, 100, 3, 100},
	}, p)

	got := SolveTurnstileToken(dx, p)
	want := base64.StdEncoding.EncodeToString([]byte("hello-world"))
	if got != want {
		t.Fatalf("SolveTurnstileToken() = %q, want %q", got, want)
	}
}

// TestSolveTurnstileTokenAcceptsRawBase64DX verifies real challenges that omit
// base64 padding are accepted. Go's StdEncoding is stricter than Python's helper
// path, so the solver must try raw encodings before treating dx as malformed.
func TestSolveTurnstileTokenAcceptsRawBase64DX(t *testing.T) {
	p := "raw-source-p"
	raw := encodeTurnstileProgramRaw(t, []any{
		[]any{2, 100, "raw-ok"},
		[]any{20, 100, 100, 3, 100},
	}, p, base64.RawStdEncoding)

	want := base64.StdEncoding.EncodeToString([]byte("raw-ok"))
	if got := SolveTurnstileToken(raw, p); got != want {
		t.Fatalf("SolveTurnstileToken() = %q, want %q", got, want)
	}
}

// TestSolveTurnstileTokenAcceptsURLSafeDX verifies the solver accepts url-safe
// base64 challenges as well as standard base64. This keeps the local Go port
// tolerant of upstream dx alphabet changes without leaking the challenge body.
func TestSolveTurnstileTokenAcceptsURLSafeDX(t *testing.T) {
	p := "url-source-p"
	raw := encodeTurnstileProgramRaw(t, []any{
		[]any{2, 100, "url-ok"},
		[]any{20, 100, 100, 3, 100},
	}, p, base64.RawURLEncoding)

	want := base64.StdEncoding.EncodeToString([]byte("url-ok"))
	if got := SolveTurnstileToken(raw, p); got != want {
		t.Fatalf("SolveTurnstileToken() = %q, want %q", got, want)
	}
}

// TestSolveTurnstileTokenPreservesFractionalDynamicOpcodes verifies process_map
// keys keep Python's numeric-key semantics. Real dx programs can copy handler
// functions onto fractional opcode keys such as 96.3; truncating those keys to
// int aliases unrelated opcodes and prevents result generation.
func TestSolveTurnstileTokenPreservesFractionalDynamicOpcodes(t *testing.T) {
	p := "fractional-source-p"
	dx := encodeTurnstileProgram(t, []any{
		[]any{8, 96.3, 20},
		[]any{8, 96.7, 21},
		[]any{2, 100, "fractional-ok"},
		[]any{96.3, 100, 100, 3, 100},
	}, p)

	want := base64.StdEncoding.EncodeToString([]byte("fractional-ok"))
	if got := SolveTurnstileToken(dx, p); got != want {
		t.Fatalf("SolveTurnstileToken() = %q, want %q", got, want)
	}
}

// TestSolveTurnstileTokenContinuesDecodedNextStage verifies newer Turnstile
// programs that write a decoded instruction list back into process_map[9] keep
// executing in the same interpreter state. The captured production challenge
// first built the next program with func23 -> func14, then expected a following
// pass to reach the result function.
func TestSolveTurnstileTokenContinuesDecodedNextStage(t *testing.T) {
	p := "next-stage-source-p"
	nextStage := []any{
		[]any{2, 100, "next-stage-ok"},
		[]any{20, 100, 100, 3, 100},
	}
	nextStageJSON, err := json.Marshal(nextStage)
	if err != nil {
		t.Fatalf("marshal next stage: %v", err)
	}
	dx := encodeTurnstileProgram(t, []any{
		[]any{2, 200, string(nextStageJSON)},
		[]any{23, 200, 14, 9, 200},
	}, p)

	want := base64.StdEncoding.EncodeToString([]byte("next-stage-ok"))
	if got := SolveTurnstileToken(dx, p); got != want {
		t.Fatalf("SolveTurnstileToken() = %q, want %q", got, want)
	}
}

// TestSolveTurnstileTokenSkipsInstructionsThatReadMissingKeys verifies missing
// process_map entries behave like the Python reference implementation. Python
// raises KeyError and skips the whole instruction; treating the missing value as
// nil can make later nil == nil branches call the result function incorrectly.
func TestSolveTurnstileTokenSkipsInstructionsThatReadMissingKeys(t *testing.T) {
	p := "missing-key-source-p"
	dx := encodeTurnstileProgram(t, []any{
		[]any{8, 100, 999},
		[]any{2, 101, nil},
		[]any{2, 102, "must-not-return"},
		[]any{20, 100, 101, 3, 102},
	}, p)

	if got := SolveTurnstileToken(dx, p); got != "" {
		t.Fatalf("SolveTurnstileToken() = %q, want empty", got)
	}
}

// TestSolveTurnstileTokenTreatsFractionalJSONNumbersAsPythonFloats verifies
// numeric JSON literals keep the Python distinction between ints and floats.
// func_5 concatenates floats but not ints; leaving values as json.Number makes
// two fractional values incorrectly fall through to NaN.
func TestSolveTurnstileTokenTreatsFractionalJSONNumbersAsPythonFloats(t *testing.T) {
	p := "float-value-source-p"
	dx := encodeTurnstileProgram(t, []any{
		[]any{2, 100, 1.25},
		[]any{2, 101, 2.5},
		[]any{5, 100, 101},
		[]any{20, 100, 100, 3, 100},
	}, p)

	want := base64.StdEncoding.EncodeToString([]byte("1.252.5"))
	if got := SolveTurnstileToken(dx, p); got != want {
		t.Fatalf("SolveTurnstileToken() = %q, want %q", got, want)
	}
}

// TestSolveTurnstileTokenSupportsBrowserPrimitives verifies the migrated
// interpreter handles the browser-like primitives used by real Turnstile
// payloads: property joins, Object.keys(localStorage), base64 conversion, and
// Reflect.set-style ordered object mutation.
func TestSolveTurnstileTokenSupportsBrowserPrimitives(t *testing.T) {
	p := "browser-source-p"
	dx := encodeTurnstileProgram(t, []any{
		[]any{2, 100, "window"},
		[]any{2, 101, "Object"},
		[]any{24, 102, 100, 101},
		[]any{2, 103, "keys"},
		[]any{24, 104, 102, 103},
		[]any{2, 105, "localStorage"},
		[]any{24, 106, 100, 105},
		[]any{17, 107, 104, 106},
		[]any{19, 107},
		[]any{20, 107, 107, 3, 107},
	}, p)

	localStorageKeys := strings.Join([]string{
		"STATSIG_LOCAL_STORAGE_INTERNAL_STORE_V4",
		"STATSIG_LOCAL_STORAGE_STABLE_ID",
		"client-correlated-secret",
		"oai/apps/capExpiresAt",
		"oai-did",
		"STATSIG_LOCAL_STORAGE_LOGGING_REQUEST",
		"UiState.isNavigationCollapsed.1",
	}, ",")
	want := base64.StdEncoding.EncodeToString([]byte(base64.StdEncoding.EncodeToString([]byte(localStorageKeys))))

	if got := SolveTurnstileToken(dx, p); got != want {
		t.Fatalf("SolveTurnstileToken() = %q, want %q", got, want)
	}
}

// TestSolveTurnstileTokenReturnsEmptyForMalformedDX verifies a broken challenge
// does not panic or produce a fake token; callers can treat the empty string as
// a real solver failure and move the account into cooldown.
func TestSolveTurnstileTokenReturnsEmptyForMalformedDX(t *testing.T) {
	if got := SolveTurnstileToken("not-base64", "source"); got != "" {
		t.Fatalf("SolveTurnstileToken() = %q, want empty", got)
	}
}

// TestChatRequirementsFinalizeUsesPythonPayloadKeys verifies finalize sends the
// same field names as the working Python path: proof_token and turnstile_token.
// The old proofofwork/turnstile names are rejected by this test server.
func TestChatRequirementsFinalizeUsesPythonPayloadKeys(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Fatalf("method = %s, want POST", r.Method)
		}
		if r.URL.Path != "/backend-api/sentinel/chat-requirements/finalize" {
			t.Fatalf("path = %s, want /backend-api/sentinel/chat-requirements/finalize", r.URL.Path)
		}
		var payload map[string]any
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Fatalf("decode body: %v", err)
		}
		if got := payload["prepare_token"]; got != "prepare-token" {
			t.Fatalf("prepare_token = %#v, want prepare-token", got)
		}
		if got := payload["proof_token"]; got != "proof-token" {
			t.Fatalf("proof_token = %#v, want proof-token", got)
		}
		if got := payload["turnstile_token"]; got != "turnstile-token" {
			t.Fatalf("turnstile_token = %#v, want turnstile-token", got)
		}
		if _, ok := payload["proofofwork"]; ok {
			t.Fatalf("payload contains stale proofofwork key: %#v", payload)
		}
		if _, ok := payload["turnstile"]; ok {
			t.Fatalf("payload contains stale turnstile key: %#v", payload)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"token":"chat-token","persona":"chatgpt-freeaccount","so_token":"so-token"}`))
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

	token, persona, soToken, err := cli.ChatRequirementsFinalize(context.Background(), "prepare-token", "proof-token", "turnstile-token")
	if err != nil {
		t.Fatalf("ChatRequirementsFinalize() error = %v", err)
	}
	if token != "chat-token" {
		t.Fatalf("token = %q, want chat-token", token)
	}
	if persona != "chatgpt-freeaccount" {
		t.Fatalf("persona = %q, want chatgpt-freeaccount", persona)
	}
	if soToken != "so-token" {
		t.Fatalf("so token = %q, want so-token", soToken)
	}
}

// TestChatRequirementsV2SolvesTurnstileWithPrepareSourceToken verifies the full
// two-step sentinel path keeps the request-local p token from prepare, solves
// turnstile.dx locally, and finalizes without falling back to the old single-step
// chat-requirements endpoint.
func TestChatRequirementsV2SolvesTurnstileWithPrepareSourceToken(t *testing.T) {
	const expectedTurnstile = "b2s="
	var singleStepCalls int
	var finalizeCalls int

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodHead && r.URL.Path == "/":
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			_, _ = w.Write([]byte(`<html><head></head><body>ok</body></html>`))

		case r.Method == http.MethodPost && r.URL.Path == "/backend-api/sentinel/chat-requirements/prepare":
			body, err := io.ReadAll(r.Body)
			if err != nil {
				t.Fatalf("read prepare body: %v", err)
			}
			var payload map[string]string
			if err := json.Unmarshal(body, &payload); err != nil {
				t.Fatalf("unmarshal prepare body: %v", err)
			}
			sourceP := payload["p"]
			if sourceP == "" {
				t.Fatalf("prepare p token is empty: %s", string(body))
			}
			dx := encodeTurnstileProgram(t, []any{
				[]any{2, 100, "ok"},
				[]any{20, 100, 100, 3, 100},
			}, sourceP)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"persona":"chatgpt-freeaccount","prepare_token":"prepare-token","turnstile":{"required":true,"dx":"` + dx + `"},"proofofwork":{"required":false}}`))

		case r.Method == http.MethodPost && r.URL.Path == "/backend-api/sentinel/chat-requirements/finalize":
			finalizeCalls++
			var payload map[string]any
			if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
				t.Fatalf("decode finalize body: %v", err)
			}
			if got := payload["prepare_token"]; got != "prepare-token" {
				t.Fatalf("prepare_token = %#v, want prepare-token", got)
			}
			if got := payload["turnstile_token"]; got != expectedTurnstile {
				t.Fatalf("turnstile_token = %#v, want %q", got, expectedTurnstile)
			}
			if _, ok := payload["turnstile"]; ok {
				t.Fatalf("payload contains stale turnstile key: %#v", payload)
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"token":"chat-token","persona":"chatgpt-freeaccount","so_token":"so-token"}`))

		case r.Method == http.MethodPost && r.URL.Path == "/backend-api/sentinel/chat-requirements":
			singleStepCalls++
			http.Error(w, "single-step fallback must not be called", http.StatusInternalServerError)

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

	got, err := cli.ChatRequirementsV2(context.Background())
	if err != nil {
		t.Fatalf("ChatRequirementsV2() error = %v", err)
	}
	if got.Token != "chat-token" {
		t.Fatalf("Token = %q, want chat-token", got.Token)
	}
	if !got.Turnstile.Required {
		t.Fatal("Turnstile.Required = false, want true")
	}
	if got.TurnstileToken != expectedTurnstile {
		t.Fatalf("TurnstileToken = %q, want %q", got.TurnstileToken, expectedTurnstile)
	}
	if got.SOToken != "so-token" {
		t.Fatalf("SOToken = %q, want so-token", got.SOToken)
	}
	if finalizeCalls != 1 {
		t.Fatalf("finalizeCalls = %d, want 1", finalizeCalls)
	}
	if singleStepCalls != 0 {
		t.Fatalf("singleStepCalls = %d, want 0", singleStepCalls)
	}
}

// TestChatRequirementsV2BootstrapsHeadersBeforePrepare verifies the two-step
// sentinel path performs the same-origin cookie/header exchange without
// downloading the unused homepage document before requirements prepare.
func TestChatRequirementsV2BootstrapsHeadersBeforePrepare(t *testing.T) {
	var calls []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls = append(calls, r.Method+" "+r.URL.Path)
		switch {
		case r.Method == http.MethodHead && r.URL.Path == "/":
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			_, _ = w.Write([]byte(`<html><head></head><body>ok</body></html>`))

		case r.Method == http.MethodPost && r.URL.Path == "/backend-api/sentinel/chat-requirements/prepare":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"persona":"chatgpt-freeaccount","prepare_token":"prepare-token","turnstile":{"required":false},"proofofwork":{"required":false}}`))

		case r.Method == http.MethodPost && r.URL.Path == "/backend-api/sentinel/chat-requirements/finalize":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"token":"chat-token","persona":"chatgpt-freeaccount"}`))

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

	got, err := cli.ChatRequirementsV2(context.Background())
	if err != nil {
		t.Fatalf("ChatRequirementsV2() error = %v", err)
	}
	if got.Token != "chat-token" {
		t.Fatalf("Token = %q, want chat-token", got.Token)
	}
	want := []string{
		"HEAD /",
		"POST /backend-api/sentinel/chat-requirements/prepare",
		"POST /backend-api/sentinel/chat-requirements/finalize",
	}
	if len(calls) != len(want) {
		t.Fatalf("calls = %#v, want %#v", calls, want)
	}
	for i := range want {
		if calls[i] != want[i] {
			t.Fatalf("calls = %#v, want %#v", calls, want)
		}
	}
}

func encodeTurnstileProgram(t *testing.T, program []any, p string) string {
	return encodeTurnstileProgramRaw(t, program, p, base64.StdEncoding)
}

func encodeTurnstileProgramRaw(t *testing.T, program []any, p string, enc *base64.Encoding) string {
	t.Helper()
	raw, err := json.Marshal(program)
	if err != nil {
		t.Fatalf("marshal program: %v", err)
	}
	return enc.EncodeToString([]byte(xorTurnstileString(string(raw), p)))
}

func xorTurnstileString(text, key string) string {
	if key == "" {
		return text
	}
	var b strings.Builder
	for i, r := range text {
		b.WriteRune(r ^ rune(key[i%len(key)]))
	}
	return b.String()
}
