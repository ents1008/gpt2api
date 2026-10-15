package chatgpt

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
)

// AccountFeatures describes the authenticated browser account state needed to
// explain image-tool eligibility without creating an image task.
type AccountFeatures struct {
	IsDeactivated          bool
	CanAccessWithSession   bool
	FeatureCount           int
	ImageFeatures          []string
	ImageGenerationEnabled bool
}

// GetAccountFeatures reads ChatGPT's authenticated account feature projection.
// It uses the client's existing proxy, cookies, browser profile, device, and
// session identity and does not mutate account state or consume image quota.
func (c *Client) GetAccountFeatures(ctx context.Context) (AccountFeatures, error) {
	query := url.Values{}
	query.Set("timezone_offset_min", fmt.Sprintf("%d", c.profile.TimezoneOffsetMinutes(time.Now())))
	endpoint := c.opts.BaseURL + "/backend-api/accounts/check/v4-2023-04-27?" + query.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return AccountFeatures{}, err
	}
	c.commonHeaders(req)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("X-Oai-Is-Client-Observation", "true")

	res, err := c.hc.Do(req)
	if err != nil {
		return AccountFeatures{}, err
	}
	defer res.Body.Close()
	body, err := io.ReadAll(io.LimitReader(res.Body, 2*1024*1024))
	if err != nil {
		return AccountFeatures{}, err
	}
	if res.StatusCode >= 400 {
		return AccountFeatures{}, &UpstreamError{
			Status: res.StatusCode, Message: "accounts/check failed", Body: string(body),
		}
	}

	var payload struct {
		Accounts map[string]struct {
			Account struct {
				IsDeactivated bool `json:"is_deactivated"`
			} `json:"account"`
			CanAccessWithSession bool              `json:"can_access_with_session"`
			Features             []json.RawMessage `json:"features"`
		} `json:"accounts"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return AccountFeatures{}, fmt.Errorf("decode accounts/check: %w", err)
	}
	account, ok := payload.Accounts["default"]
	if !ok {
		return AccountFeatures{}, fmt.Errorf("accounts/check missing default account")
	}
	features := make([]string, 0, len(account.Features))
	imageGenerationEnabled := false
	for _, raw := range account.Features {
		name := featureName(raw)
		if name != "" && isImageFeatureName(name) {
			features = append(features, name)
		}
		if isImageGenerationFeatureName(name) {
			imageGenerationEnabled = true
		}
	}
	sort.Strings(features)
	features = compactStrings(features)
	return AccountFeatures{
		IsDeactivated:          account.Account.IsDeactivated,
		CanAccessWithSession:   account.CanAccessWithSession,
		FeatureCount:           len(account.Features),
		ImageFeatures:          features,
		ImageGenerationEnabled: imageGenerationEnabled,
	}, nil
}

func featureName(raw json.RawMessage) string {
	var name string
	if err := json.Unmarshal(raw, &name); err == nil {
		return strings.TrimSpace(name)
	}
	var object map[string]interface{}
	if err := json.Unmarshal(raw, &object); err != nil {
		return ""
	}
	for _, key := range []string{"feature_name", "name", "id", "key"} {
		if value, ok := object[key].(string); ok && strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func isImageFeatureName(name string) bool {
	normalized := strings.ToLower(name)
	return strings.Contains(normalized, "image") ||
		strings.Contains(normalized, "img") ||
		strings.Contains(normalized, "picture")
}

func isImageGenerationFeatureName(name string) bool {
	normalized := strings.ToLower(strings.TrimSpace(name))
	return strings.Contains(normalized, "image_gen") ||
		strings.Contains(normalized, "gpt_image") ||
		strings.Contains(normalized, "picture_gen")
}

func compactStrings(values []string) []string {
	if len(values) < 2 {
		return values
	}
	out := values[:1]
	for _, value := range values[1:] {
		if value != out[len(out)-1] {
			out = append(out, value)
		}
	}
	return out
}
