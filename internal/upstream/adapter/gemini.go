package adapter

import (
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

// geminiAdapter 兼容 Google Generative Language API v1beta。
//
// 协议差异要点:
//   1. 鉴权用 query param ?key=xxx 或 header X-Goog-Api-Key: xxx。
//   2. Chat 接口路径:
//        POST /v1beta/models/{model}:generateContent        (非流式)
//        POST /v1beta/models/{model}:streamGenerateContent  (流式)
//      流式默认返回 JSON 数组(每个元素是一个 GenerateContentResponse),
//      配合 ?alt=sse 才能拿到 text/event-stream。
//   3. 消息体结构:{ contents: [{role, parts:[{text}]}] }。
//      role 只接受 "user" / "model"(把 OpenAI assistant → model,system 拼
//      到 systemInstruction 或第一条 user 里)。
//   4. 图片生成走 imagen-4:generateContent 或 gemini-2.5-flash-image-preview,
//      返回 inlineData base64。
//
// 本适配器把 Gemini 响应实时转换成 OpenAI 风格 ChatChunk 输出,调用方无感。
type geminiAdapter struct {
	baseURL string
	apiKey  string
	client  *http.Client
}

func NewGemini(p Params) *geminiAdapter {
	base := strings.TrimRight(p.BaseURL, "/")
	// 允许用户填 https://generativelanguage.googleapis.com 或已带 /v1beta。
	base = strings.TrimSuffix(base, "/v1beta")
	timeout := time.Duration(p.TimeoutS) * time.Second
	if timeout <= 0 {
		timeout = 120 * time.Second
	}
	return &geminiAdapter{
		baseURL: base,
		apiKey:  p.APIKey,
		client:  &http.Client{Timeout: timeout},
	}
}

func (a *geminiAdapter) Type() string { return "gemini" }

// geminiContent 对应 { contents: [{ role, parts: [{ text }] }] }。
type geminiContent struct {
	Role  string       `json:"role,omitempty"`
	Parts []geminiPart `json:"parts"`
}

type geminiPart struct {
	Text       string            `json:"text,omitempty"`
	InlineData *geminiInlineData `json:"inlineData,omitempty"`
}

type geminiInlineData struct {
	MimeType string `json:"mimeType"`
	Data     string `json:"data"`
}

// buildGeminiPayload 把 OpenAI 风格 messages 转成 Gemini contents。
// system 角色合并进 systemInstruction 字段。
func buildGeminiPayload(req *ChatRequest) map[string]any {
	var contents []geminiContent
	var systemParts []geminiPart

	for _, m := range req.Messages {
		switch m.Role {
		case "system":
			systemParts = append(systemParts, geminiPart{Text: m.Content})
		case "assistant":
			contents = append(contents, geminiContent{
				Role:  "model",
				Parts: []geminiPart{{Text: m.Content}},
			})
		default: // "user" / tool / function 统一当 user
			contents = append(contents, geminiContent{
				Role:  "user",
				Parts: []geminiPart{{Text: m.Content}},
			})
		}
	}

	payload := map[string]any{
		"contents": contents,
	}
	if len(systemParts) > 0 {
		payload["systemInstruction"] = geminiContent{Parts: systemParts}
	}
	// generationConfig
	genCfg := map[string]any{}
	if req.Temperature > 0 {
		genCfg["temperature"] = req.Temperature
	}
	if req.TopP > 0 {
		genCfg["topP"] = req.TopP
	}
	if req.MaxTokens > 0 {
		genCfg["maxOutputTokens"] = req.MaxTokens
	}
	if len(genCfg) > 0 {
		payload["generationConfig"] = genCfg
	}
	return payload
}

// Chat 请求 Gemini。
//
// 说明关于"接受这个延迟":Gemini 流式接口(streamGenerateContent)默认
// 返回 JSON 数组,而不是真正的 SSE。为把它转换成 OpenAI SSE chunk,本实现
// 会实时流式解析上游 JSON 数组元素并逐个吐出 ChatChunk。
//   - 若用户请求 stream=true:走 streamGenerateContent?alt=sse
//   - 若用户请求 stream=false:走 generateContent 一次性返回
func (a *geminiAdapter) Chat(ctx context.Context, upstreamModel string, req *ChatRequest) (ChatStream, error) {
	path := "/v1beta/models/" + upstreamModel
	if req.Stream {
		path += ":streamGenerateContent?alt=sse"
	} else {
		path += ":generateContent"
	}
	body, _ := json.Marshal(buildGeminiPayload(req))
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost,
		a.baseURL+path, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("X-Goog-Api-Key", a.apiKey)

	resp, err := a.client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("gemini: request: %w", err)
	}
	if resp.StatusCode >= 400 {
		return nil, upstreamErr(resp)
	}

	ch := make(chan ChatChunk, 16)
	if req.Stream {
		go parseGeminiSSE(resp.Body, ch)
	} else {
		go parseGeminiNonStream(resp.Body, ch)
	}
	return ch, nil
}

// geminiGenResp 是单次 generateContent 响应 / SSE 单条 data 的结构。
type geminiGenResp struct {
	Candidates []struct {
		Content struct {
			Parts []geminiPart `json:"parts"`
			Role  string       `json:"role"`
		} `json:"content"`
		FinishReason string `json:"finishReason"`
	} `json:"candidates"`
	UsageMetadata *struct {
		PromptTokenCount     int `json:"promptTokenCount"`
		CandidatesTokenCount int `json:"candidatesTokenCount"`
		TotalTokenCount      int `json:"totalTokenCount"`
	} `json:"usageMetadata"`
}

// parseGeminiSSE 解析 ?alt=sse 的返回流,每条 data: {...} 对应一次 chunk。
func parseGeminiSSE(body io.ReadCloser, ch chan<- ChatChunk) {
	defer body.Close()
	defer close(ch)

	reader := newLineReader(body)
	var lastUsage *ChatUsage

	for {
		line, err := reader.readLine()
		if err != nil {
			if !errors.Is(err, io.EOF) {
				ch <- ChatChunk{Err: err}
			}
			break
		}
		if line == "" || !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "" || data == "[DONE]" {
			continue
		}
		var obj geminiGenResp
		if err := json.Unmarshal([]byte(data), &obj); err != nil {
			continue
		}
		if obj.UsageMetadata != nil {
			lastUsage = &ChatUsage{
				PromptTokens:     obj.UsageMetadata.PromptTokenCount,
				CompletionTokens: obj.UsageMetadata.CandidatesTokenCount,
				TotalTokens:      obj.UsageMetadata.TotalTokenCount,
			}
		}
		for _, cand := range obj.Candidates {
			for _, p := range cand.Content.Parts {
				if p.Text != "" {
					ch <- ChatChunk{Delta: p.Text}
				}
			}
			if cand.FinishReason != "" {
				ch <- ChatChunk{FinishReason: mapGeminiFinish(cand.FinishReason)}
			}
		}
	}
	if lastUsage != nil {
		ch <- ChatChunk{Usage: lastUsage}
	}
}

// parseGeminiNonStream 读取 generateContent 一次性 JSON 响应。
func parseGeminiNonStream(body io.ReadCloser, ch chan<- ChatChunk) {
	defer body.Close()
	defer close(ch)

	var obj geminiGenResp
	if err := json.NewDecoder(body).Decode(&obj); err != nil {
		ch <- ChatChunk{Err: fmt.Errorf("gemini: decode: %w", err)}
		return
	}
	var text strings.Builder
	finish := "stop"
	for _, cand := range obj.Candidates {
		for _, p := range cand.Content.Parts {
			text.WriteString(p.Text)
		}
		if cand.FinishReason != "" {
			finish = mapGeminiFinish(cand.FinishReason)
		}
	}
	ch <- ChatChunk{Delta: text.String(), FinishReason: finish}
	if obj.UsageMetadata != nil {
		ch <- ChatChunk{Usage: &ChatUsage{
			PromptTokens:     obj.UsageMetadata.PromptTokenCount,
			CompletionTokens: obj.UsageMetadata.CandidatesTokenCount,
			TotalTokens:      obj.UsageMetadata.TotalTokenCount,
		}}
	}
}

// mapGeminiFinish 把 Gemini 的 finishReason 归一到 OpenAI 风格。
func mapGeminiFinish(r string) string {
	switch r {
	case "STOP":
		return "stop"
	case "MAX_TOKENS":
		return "length"
	case "SAFETY", "RECITATION", "PROHIBITED_CONTENT", "BLOCKLIST":
		return "content_filter"
	default:
		return strings.ToLower(r)
	}
}

// ImageGenerate 支持两种 Gemini 图像模型:
//   - imagen-4.0-generate-001 这类 imagen-*:predict 接口(真正的图像生成)
//   - gemini-2.5-flash-image-preview 等多模态模型走 generateContent,响应里
//     包含 inlineData(base64)。
//
// 这里按 upstreamModel 名字自动分发。
func (a *geminiAdapter) ImageGenerate(ctx context.Context, upstreamModel string, req *ImageRequest) (*ImageResult, error) {
	if strings.HasPrefix(upstreamModel, "imagen") {
		return a.imagenGenerate(ctx, upstreamModel, req)
	}
	return a.geminiImageGenerate(ctx, upstreamModel, req)
}

// imagenGenerate 走 /v1beta/models/{model}:predict,专门给 imagen-* 用。
func (a *geminiAdapter) imagenGenerate(ctx context.Context, upstreamModel string, req *ImageRequest) (*ImageResult, error) {
	n := req.N
	if n <= 0 {
		n = 1
	}
	payload := map[string]any{
		"instances": []map[string]any{
			{"prompt": req.Prompt},
		},
		"parameters": map[string]any{
			"sampleCount": n,
		},
	}
	body, _ := json.Marshal(payload)
	url := a.baseURL + "/v1beta/models/" + upstreamModel + ":predict"
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("X-Goog-Api-Key", a.apiKey)

	resp, err := a.client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("gemini: imagen: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return nil, upstreamErr(resp)
	}
	var obj struct {
		Predictions []struct {
			BytesBase64Encoded string `json:"bytesBase64Encoded"`
		} `json:"predictions"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&obj); err != nil {
		return nil, err
	}
	r := &ImageResult{}
	for _, p := range obj.Predictions {
		if p.BytesBase64Encoded != "" {
			r.B64s = append(r.B64s, p.BytesBase64Encoded)
		}
	}
	if len(r.B64s) == 0 {
		return nil, errors.New("gemini imagen: empty response")
	}
	return r, nil
}

// geminiImageGenerate 走 gemini-*-image 多模态模型,请求 generateContent,
// 响应里 parts 会含 inlineData(base64)。
func (a *geminiAdapter) geminiImageGenerate(ctx context.Context, upstreamModel string, req *ImageRequest) (*ImageResult, error) {
	payload := map[string]any{
		"contents": []geminiContent{
			{Role: "user", Parts: []geminiPart{{Text: req.Prompt}}},
		},
	}
	body, _ := json.Marshal(payload)
	url := a.baseURL + "/v1beta/models/" + upstreamModel + ":generateContent"
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("X-Goog-Api-Key", a.apiKey)

	resp, err := a.client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("gemini: image: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return nil, upstreamErr(resp)
	}
	var obj geminiGenResp
	if err := json.NewDecoder(resp.Body).Decode(&obj); err != nil {
		return nil, err
	}
	r := &ImageResult{}
	for _, cand := range obj.Candidates {
		for _, p := range cand.Content.Parts {
			if p.InlineData != nil && p.InlineData.Data != "" {
				r.B64s = append(r.B64s, p.InlineData.Data)
			}
		}
	}
	if len(r.B64s) == 0 {
		return nil, errors.New("gemini image: empty response")
	}
	return r, nil
}

// Ping 探活:GET /v1beta/models?pageSize=1。
func (a *geminiAdapter) Ping(ctx context.Context) error {
	url := a.baseURL + "/v1beta/models?pageSize=1"
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	httpReq.Header.Set("X-Goog-Api-Key", a.apiKey)
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

// ---- helpers ----

// lineReader 是个精简的按行读取器,兼容 SSE 场景(遇到 \n 就返回一行)。
type lineReader struct {
	r   io.Reader
	buf []byte
}

func newLineReader(r io.Reader) *lineReader { return &lineReader{r: r, buf: make([]byte, 0, 4096)} }

func (l *lineReader) readLine() (string, error) {
	for {
		if idx := bytes.IndexByte(l.buf, '\n'); idx >= 0 {
			line := string(bytes.TrimRight(l.buf[:idx], "\r"))
			l.buf = l.buf[idx+1:]
			return line, nil
		}
		tmp := make([]byte, 4096)
		n, err := l.r.Read(tmp)
		if n > 0 {
			l.buf = append(l.buf, tmp[:n]...)
		}
		if err != nil {
			if len(l.buf) > 0 {
				line := string(bytes.TrimRight(l.buf, "\r"))
				l.buf = nil
				return line, nil
			}
			return "", err
		}
	}
}
