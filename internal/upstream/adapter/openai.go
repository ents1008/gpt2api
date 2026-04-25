package adapter

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// openaiAdapter 兼容 OpenAI /v1/chat/completions、/v1/images/generations。
//
// 许多第三方中转/聚合站(one-api、new-api、deepseek 官方、moonshot 官方、
// kimi 兼容端点等)都遵循 OpenAI 接口规范,差别只在 BaseURL 和 APIKey。
// 因此这个适配器同时适用:BaseURL 允许带或不带 /v1 后缀,我们做一次规整。
type openaiAdapter struct {
	baseURL string
	apiKey  string
	client  *http.Client
}

// NewOpenAI 构造一个 OpenAI 兼容适配器。
func NewOpenAI(p Params) *openaiAdapter {
	base := strings.TrimRight(p.BaseURL, "/")
	// 自动去尾部的 /v1,底下拼接时再补;用户填 https://api.openai.com 和
	// https://api.openai.com/v1 都要能用。
	base = strings.TrimSuffix(base, "/v1")
	timeout := time.Duration(p.TimeoutS) * time.Second
	if timeout <= 0 {
		timeout = 120 * time.Second
	}
	return &openaiAdapter{
		baseURL: base,
		apiKey:  p.APIKey,
		client:  &http.Client{Timeout: timeout},
	}
}

func (a *openaiAdapter) Type() string { return "openai" }

func (a *openaiAdapter) endpoint(path string) string {
	return a.baseURL + "/v1" + path
}

// Chat 发起 OpenAI /v1/chat/completions。流式和非流式都转成统一的 ChatStream。
func (a *openaiAdapter) Chat(ctx context.Context, upstreamModel string, req *ChatRequest) (ChatStream, error) {
	payload := map[string]any{
		"model":    upstreamModel,
		"messages": req.Messages,
		"stream":   req.Stream,
	}
	if req.Temperature > 0 {
		payload["temperature"] = req.Temperature
	}
	if req.TopP > 0 {
		payload["top_p"] = req.TopP
	}
	if req.MaxTokens > 0 {
		payload["max_tokens"] = req.MaxTokens
	}
	body, _ := json.Marshal(payload)
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost,
		a.endpoint("/chat/completions"), bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+a.apiKey)
	if req.Stream {
		httpReq.Header.Set("Accept", "text/event-stream")
	}

	resp, err := a.client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("openai: request: %w", err)
	}
	if resp.StatusCode >= 400 {
		return nil, upstreamErr(resp)
	}

	ch := make(chan ChatChunk, 16)
	if req.Stream {
		go parseOpenAISSE(resp.Body, ch)
	} else {
		go parseOpenAINonStream(resp.Body, ch)
	}
	return ch, nil
}

// parseOpenAISSE 解析 text/event-stream 响应,每行 data: {...}。
func parseOpenAISSE(body io.ReadCloser, ch chan<- ChatChunk) {
	defer body.Close()
	defer close(ch)

	sc := bufio.NewScanner(body)
	// SSE 单行可能很长,扩大 buffer。
	buf := make([]byte, 0, 64*1024)
	sc.Buffer(buf, 4*1024*1024)

	var lastUsage *ChatUsage

	for sc.Scan() {
		line := sc.Text()
		if line == "" {
			continue
		}
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "[DONE]" {
			break
		}
		var obj struct {
			Choices []struct {
				Delta struct {
					Content string `json:"content"`
				} `json:"delta"`
				FinishReason *string `json:"finish_reason"`
			} `json:"choices"`
			Usage *struct {
				PromptTokens     int `json:"prompt_tokens"`
				CompletionTokens int `json:"completion_tokens"`
				TotalTokens      int `json:"total_tokens"`
			} `json:"usage"`
		}
		if err := json.Unmarshal([]byte(data), &obj); err != nil {
			continue
		}
		if obj.Usage != nil {
			lastUsage = &ChatUsage{
				PromptTokens:     obj.Usage.PromptTokens,
				CompletionTokens: obj.Usage.CompletionTokens,
				TotalTokens:      obj.Usage.TotalTokens,
			}
		}
		for _, c := range obj.Choices {
			chunk := ChatChunk{Delta: c.Delta.Content}
			if c.FinishReason != nil {
				chunk.FinishReason = *c.FinishReason
			}
			ch <- chunk
		}
	}

	if lastUsage != nil {
		ch <- ChatChunk{Usage: lastUsage}
	}
	if err := sc.Err(); err != nil && !errors.Is(err, io.EOF) {
		ch <- ChatChunk{Err: err}
	}
}

// parseOpenAINonStream 读整个 JSON 响应,一次吐成 delta + finish_reason。
func parseOpenAINonStream(body io.ReadCloser, ch chan<- ChatChunk) {
	defer body.Close()
	defer close(ch)

	var obj struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Usage struct {
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
			TotalTokens      int `json:"total_tokens"`
		} `json:"usage"`
	}
	if err := json.NewDecoder(body).Decode(&obj); err != nil {
		ch <- ChatChunk{Err: fmt.Errorf("openai: decode non-stream: %w", err)}
		return
	}
	if len(obj.Choices) == 0 {
		ch <- ChatChunk{FinishReason: "stop"}
		return
	}
	c := obj.Choices[0]
	ch <- ChatChunk{Delta: c.Message.Content, FinishReason: c.FinishReason}
	ch <- ChatChunk{Usage: &ChatUsage{
		PromptTokens:     obj.Usage.PromptTokens,
		CompletionTokens: obj.Usage.CompletionTokens,
		TotalTokens:      obj.Usage.TotalTokens,
	}}
}

// ImageGenerate 调用 /v1/images/generations(DALL·E 3 / gpt-image-1 等)。
func (a *openaiAdapter) ImageGenerate(ctx context.Context, upstreamModel string, req *ImageRequest) (*ImageResult, error) {
	n := req.N
	if n <= 0 {
		n = 1
	}
	size := req.Size
	if size == "" {
		size = "1024x1024"
	}
	payload := map[string]any{
		"model":  upstreamModel,
		"prompt": req.Prompt,
		"n":      n,
		"size":   size,
	}
	body, _ := json.Marshal(payload)
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost,
		a.endpoint("/images/generations"), bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+a.apiKey)

	resp, err := a.client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("openai: image request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return nil, upstreamErr(resp)
	}
	var obj struct {
		Data []struct {
			URL    string `json:"url"`
			B64    string `json:"b64_json"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&obj); err != nil {
		return nil, fmt.Errorf("openai: image decode: %w", err)
	}
	r := &ImageResult{}
	for _, d := range obj.Data {
		if d.URL != "" {
			r.URLs = append(r.URLs, d.URL)
		}
		if d.B64 != "" {
			r.B64s = append(r.B64s, d.B64)
		}
	}
	if len(r.URLs) == 0 && len(r.B64s) == 0 {
		return nil, errors.New("openai: empty image response")
	}
	return r, nil
}

// Ping 发一次 /v1/models 探活。大部分兼容站都实现了这个端点。
func (a *openaiAdapter) Ping(ctx context.Context) error {
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet,
		a.endpoint("/models"), nil)
	if err != nil {
		return err
	}
	httpReq.Header.Set("Authorization", "Bearer "+a.apiKey)
	resp, err := a.client.Do(httpReq)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return upstreamErr(resp)
	}
	return nil
}

// upstreamErr 读取响应 body 做简要错误归纳。
func upstreamErr(resp *http.Response) error {
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 8*1024))
	return fmt.Errorf("upstream %d: %s", resp.StatusCode, strings.TrimSpace(string(data)))
}
