package chatgpt

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
)

// GetConversationStreamStatus reads the same stream-status endpoint ChatGPT Web polls while waiting
// for async conversation work to finish after a handoff.
func (c *Client) GetConversationStreamStatus(ctx context.Context, convID string) (string, error) {
	if convID == "" {
		return "", fmt.Errorf("conv_id required")
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		c.opts.BaseURL+"/backend-api/conversation/"+url.PathEscape(convID)+"/stream_status", nil)
	if err != nil {
		return "", err
	}
	c.commonHeaders(req)
	req.Header.Set("Accept", "*/*")
	req.Header.Set("X-Openai-Target-Route", "/backend-api/conversation/{conversation_id}/stream_status")

	res, err := c.hc.Do(req)
	if err != nil {
		return "", err
	}
	defer res.Body.Close()

	buf, _ := io.ReadAll(res.Body)
	if res.StatusCode >= 400 {
		return "", &UpstreamError{Status: res.StatusCode, Message: "conversation stream_status failed", Body: string(buf)}
	}

	var out struct {
		Status string `json:"status"`
	}
	if err := json.Unmarshal(buf, &out); err != nil {
		return "", fmt.Errorf("decode stream_status: %w", err)
	}
	return out.Status, nil
}
