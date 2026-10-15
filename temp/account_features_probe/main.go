package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"strings"
	"time"

	"github.com/432539/gpt2api/internal/upstream/chatgpt"
)

type probeInput struct {
	AuthToken      string `json:"auth_token"`
	DeviceID       string `json:"device_id"`
	SessionID      string `json:"session_id"`
	Cookies        string `json:"cookies"`
	ProxyURL       string `json:"proxy_url"`
	BrowserProfile string `json:"browser_profile"`
}

type probeResult struct {
	OK                     bool     `json:"ok"`
	Stage                  string   `json:"stage"`
	Status                 int      `json:"status,omitempty"`
	ErrorClass             string   `json:"error_class,omitempty"`
	ElapsedMS              int64    `json:"elapsed_ms"`
	FeatureCount           int      `json:"feature_count,omitempty"`
	ImageFeatures          []string `json:"image_features,omitempty"`
	IsDeactivated          bool     `json:"is_deactivated,omitempty"`
	CanAccessWithSession   bool     `json:"can_access_with_session,omitempty"`
	ImageGenerationEnabled bool     `json:"image_generation_enabled,omitempty"`
}

func main() {
	input, err := readProbeInput(os.Stdin)
	if err != nil {
		writeProbeResult(probeResult{Stage: "input", ErrorClass: err.Error()})
		os.Exit(2)
	}
	client, err := chatgpt.New(chatgpt.Options{
		AuthToken: input.AuthToken, DeviceID: input.DeviceID,
		SessionID: input.SessionID, Cookies: input.Cookies,
		ProxyURL: input.ProxyURL, BrowserProfile: input.BrowserProfile,
		Timeout: 25 * time.Second,
	})
	if err != nil {
		writeProbeResult(probeResult{Stage: "client_init", ErrorClass: "invalid_identity"})
		os.Exit(2)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	started := time.Now()
	features, err := client.GetAccountFeatures(ctx)
	if err != nil {
		status, class := classifyProbeError(err)
		writeProbeResult(probeResult{
			Stage: "account_features", Status: status, ErrorClass: class,
			ElapsedMS: time.Since(started).Milliseconds(),
		})
		os.Exit(1)
	}
	writeProbeResult(probeResult{
		OK: true, Stage: "account_features",
		ElapsedMS:    time.Since(started).Milliseconds(),
		FeatureCount: features.FeatureCount, ImageFeatures: features.ImageFeatures,
		IsDeactivated:          features.IsDeactivated,
		CanAccessWithSession:   features.CanAccessWithSession,
		ImageGenerationEnabled: features.ImageGenerationEnabled,
	})
}

func readProbeInput(reader io.Reader) (probeInput, error) {
	decoder := json.NewDecoder(io.LimitReader(reader, 2*1024*1024))
	decoder.DisallowUnknownFields()
	var input probeInput
	if err := decoder.Decode(&input); err != nil {
		return probeInput{}, errors.New("invalid_json")
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return probeInput{}, errors.New("multiple_values")
	}
	fields := []struct{ name, value string }{
		{"auth_token", input.AuthToken}, {"device_id", input.DeviceID},
		{"session_id", input.SessionID}, {"cookies", input.Cookies},
		{"proxy_url", input.ProxyURL}, {"browser_profile", input.BrowserProfile},
	}
	for _, field := range fields {
		if strings.TrimSpace(field.value) == "" {
			return probeInput{}, errors.New("missing_" + field.name)
		}
	}
	return input, nil
}

func classifyProbeError(err error) (int, string) {
	var upstream *chatgpt.UpstreamError
	if errors.As(err, &upstream) {
		switch upstream.Status {
		case 401:
			return 401, "unauthorized"
		case 403:
			return 403, "forbidden"
		case 429:
			return 429, "rate_limited"
		default:
			return upstream.Status, "upstream_error"
		}
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return 0, "timeout"
	}
	return 0, "transport_or_decode_error"
}

func writeProbeResult(result probeResult) {
	_ = json.NewEncoder(os.Stdout).Encode(result)
}
