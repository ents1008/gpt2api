package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"strings"
	"time"

	"github.com/432539/gpt2api/internal/upstream/chatgpt"
)

type probeAccount struct {
	ID             int    `json:"id"`
	AuthToken      string `json:"auth_token"`
	DeviceID       string `json:"device_id"`
	SessionID      string `json:"session_id"`
	Cookies        string `json:"cookies"`
	ProxyURL       string `json:"proxy_url"`
	BrowserProfile string `json:"browser_profile"`
}

type probeResult struct {
	ID                int    `json:"id"`
	OK                bool   `json:"ok"`
	Stage             string `json:"stage"`
	Status            int    `json:"status,omitempty"`
	ErrorClass        string `json:"error_class,omitempty"`
	Persona           string `json:"persona,omitempty"`
	TokenPresent      bool   `json:"token_present,omitempty"`
	TurnstileRequired bool   `json:"turnstile_required,omitempty"`
	POWRequired       bool   `json:"pow_required,omitempty"`
}

const probeTimeout = 25 * time.Second

func main() {
	raw := os.Getenv("GPT2API_LIVE_PROBES")
	if strings.TrimSpace(raw) == "" {
		writeProbeResult(probeResult{Stage: "input", ErrorClass: "missing_input"})
		os.Exit(2)
	}

	var accounts []probeAccount
	if err := json.Unmarshal([]byte(raw), &accounts); err != nil {
		writeProbeResult(probeResult{Stage: "input", ErrorClass: "invalid_json"})
		os.Exit(2)
	}

	for _, acc := range accounts {
		if errorClass := validateProbeAccount(acc); errorClass != "" {
			writeProbeResult(probeResult{
				ID: acc.ID, Stage: "input", ErrorClass: errorClass,
			})
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), probeTimeout)
		client, err := chatgpt.New(optionsForProbe(acc))
		if err != nil {
			cancel()
			writeProbeResult(probeResult{
				ID: acc.ID, Stage: "init", ErrorClass: "invalid_account_identity",
			})
			continue
		}
		resp, err := client.ChatRequirementsV2(ctx)
		cancel()
		if err != nil {
			status, errorClass := classifyProbeError(err)
			writeProbeResult(probeResult{
				ID: acc.ID, Stage: classifyProbeStage(err), Status: status, ErrorClass: errorClass,
			})
			continue
		}
		writeProbeResult(probeResult{
			ID:                acc.ID,
			OK:                true,
			Stage:             "requirements",
			Persona:           resp.Persona,
			TokenPresent:      resp.Token != "",
			TurnstileRequired: resp.Turnstile.Required,
			POWRequired:       resp.Proofofwork.Required,
		})
	}
}

func classifyProbeStage(err error) string {
	var upstream *chatgpt.UpstreamError
	if !errors.As(err, &upstream) {
		return "requirements"
	}
	switch upstream.Message {
	case "chat-requirements/prepare failed":
		return "requirements_prepare"
	case "chat-requirements/finalize failed":
		return "requirements_finalize"
	default:
		return "requirements"
	}
}

func validateProbeAccount(acc probeAccount) string {
	switch {
	case strings.TrimSpace(acc.AuthToken) == "":
		return "missing_auth_token"
	case strings.TrimSpace(acc.DeviceID) == "":
		return "missing_device_id"
	case strings.TrimSpace(acc.SessionID) == "":
		return "missing_session_id"
	case strings.TrimSpace(acc.Cookies) == "":
		return "missing_cookies"
	case strings.TrimSpace(acc.ProxyURL) == "":
		return "missing_proxy"
	case strings.TrimSpace(acc.BrowserProfile) == "":
		return "missing_browser_profile"
	default:
		return ""
	}
}

func optionsForProbe(acc probeAccount) chatgpt.Options {
	return chatgpt.Options{
		AuthToken:      acc.AuthToken,
		DeviceID:       acc.DeviceID,
		SessionID:      acc.SessionID,
		Cookies:        acc.Cookies,
		ProxyURL:       acc.ProxyURL,
		BrowserProfile: acc.BrowserProfile,
		Timeout:        probeTimeout,
	}
}

func classifyProbeError(err error) (int, string) {
	var upstream *chatgpt.UpstreamError
	if errors.As(err, &upstream) {
		switch upstream.Status {
		case 401:
			return upstream.Status, "unauthorized"
		case 403:
			return upstream.Status, "forbidden"
		case 429:
			return upstream.Status, "rate_limited"
		default:
			return upstream.Status, "upstream_error"
		}
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return 0, "timeout"
	}
	var networkError net.Error
	if errors.As(err, &networkError) {
		return 0, "transport_error"
	}
	return 0, "requirements_error"
}

func writeProbeResult(result probeResult) {
	raw, err := json.Marshal(result)
	if err != nil {
		fmt.Println(`{"ok":false,"stage":"output","error_class":"encode_failed"}`)
		return
	}
	fmt.Println(string(raw))
}
