// Package chatgpt - 图像生成协议
//
// 完整链路(和文字聊天共用 f/conversation,只通过 system_hints=["picture_v2"] 区分):
//
//  0. (可选) HEAD /                             → 拿 oai-did cookie
//  1. POST /backend-api/f/conversation/prepare      → conduit_token
//  2. POST /backend-api/sentinel/chat-requirements → chat_token + 可选 POW 挑战
//  3. POST /backend-api/f/conversation (SSE)         → 边解析边收 file-service://
//  4. SSE 没直出 file-service 时轮询 GET /backend-api/conversation/{id}
//     任何一条 tool 消息出现 file-service / sediment asset_pointer 即算成功,
//     够 N 张立即返回;IMG2 已正式上线,不再做"灰度命中判定"。
//  5. GET /backend-api/files/{fid}/download                   → 签名 URL(file-service)
//     GET /backend-api/conversation/{cid}/attachment/{sid}/download → 签名 URL(sediment)
//  6. GET 签名 URL → 图片字节
//
// 注意:不要调用 /backend-api/conversation/init——这是老客户端路径,在免费账号上会
// 直接 404 让整条链路失败,上游把 picture_v2 路由完全交给 f/conversation 的 payload。
package chatgpt

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"
)

// ImageConvOpts 是图像会话的入参。
type ImageConvOpts struct {
	Prompt         string          // 用户提示词(已处理完的,含可选 CLARITY_SUFFIX)
	UpstreamModel  string          // 默认 "gpt-5-3"
	ConvID         string          // 复用会话时填,空则新建
	ParentMsgID    string          // 复用会话时从 GetConversationHead 取;新会话随机
	MessageID      string          // 可选,留空自动生成
	ChatToken      string          // 必传,来自 ChatRequirements
	ProofToken     string          // 可选
	TurnstileToken string          // 可选,来自 ChatRequirementsV2
	SOToken        string          // 可选,来自 requirements finalize
	ConduitToken   string          // 可选,来自 PrepareFConversation
	TimezoneOffset int             // 留空时使用账号浏览器时区的当前偏移
	SSETimeout     time.Duration   // 默认 120s
	References     []*UploadedFile // 图生图/编辑:已上传的参考图,会插到 prompt 前面
}

// InitConversation 对应 /backend-api/conversation/init。
// 新会话必须调用,否则后续 /f/conversation 会 404。
// systemHints 为空串数组表示文字聊天;图像场景传 []string{"picture_v2"}。
func (c *Client) InitConversation(ctx context.Context, systemHints ...string) error {
	if systemHints == nil {
		systemHints = []string{}
	}
	payload := map[string]interface{}{
		"gizmo_id":                nil,
		"requested_default_model": nil,
		"conversation_id":         nil,
		"timezone_offset_min":     c.profile.TimezoneOffsetMinutes(time.Now()),
		"system_hints":            systemHints,
	}
	body, _ := json.Marshal(payload)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.opts.BaseURL+"/backend-api/conversation/init",
		strings.NewReader(string(body)))
	if err != nil {
		return err
	}
	c.commonHeaders(req)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "*/*")

	res, err := c.hc.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode >= 400 {
		buf, _ := io.ReadAll(res.Body)
		return &UpstreamError{Status: res.StatusCode, Message: "conversation/init failed", Body: string(buf)}
	}
	_, _ = io.Copy(io.Discard, res.Body)
	return nil
}

// PrepareFConversation 对应 /backend-api/f/conversation/prepare,返回 conduit_token。
//
// payload 对齐 HAR 抓包 /f/conversation/prepare(image 通路):
//   - client_prepare_state: "success"
//   - fork_from_shared_post: false
//   - partial_query: 完整的 user message(id+author+content,包含当前 prompt)
//   - system_hints: ["picture_v2"]   ← image 通路特有
//   - client_contextual_info: { "app_name": "chatgpt.com" }   ← prepare 阶段只带 app_name
func (c *Client) PrepareFConversation(ctx context.Context, opt ImageConvOpts) (string, error) {
	if opt.UpstreamModel == "" {
		opt.UpstreamModel = "auto"
	}
	if opt.MessageID == "" {
		opt.MessageID = uuid.NewString()
	}
	payload := map[string]interface{}{
		"action":                "next",
		"fork_from_shared_post": false,
		"parent_message_id":     opt.ParentMsgID,
		"model":                 opt.UpstreamModel,
		"client_prepare_state":  "success",
		"timezone_offset_min":   c.profile.TimezoneOffsetMinutes(time.Now()),
		"timezone":              c.profile.Timezone,
		"conversation_mode":     map[string]string{"kind": "primary_assistant"},
		"system_hints":          []string{"picture_v2"},
		"partial_query": map[string]interface{}{
			"id":     uuid.NewString(),
			"author": map[string]string{"role": "user"},
			"content": map[string]interface{}{
				"content_type": "text",
				"parts":        []string{opt.Prompt},
			},
		},
		"supports_buffering":  true,
		"supported_encodings": []string{"v1"},
		"client_contextual_info": map[string]interface{}{
			"app_name": "chatgpt.com",
		},
	}
	// 只有已有会话才带 conversation_id;新会话不带这个 key(对齐浏览器抓包,
	// 带陌生 UUID 上游会 404)
	if opt.ConvID != "" {
		payload["conversation_id"] = opt.ConvID
	}
	body, _ := json.Marshal(payload)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.opts.BaseURL+"/backend-api/f/conversation/prepare",
		strings.NewReader(string(body)))
	if err != nil {
		return "", err
	}
	c.commonHeaders(req)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "*/*")
	req.Header.Set("Openai-Sentinel-Chat-Requirements-Token", opt.ChatToken)
	if opt.ProofToken != "" {
		req.Header.Set("Openai-Sentinel-Proof-Token", opt.ProofToken)
	}
	if opt.TurnstileToken != "" {
		req.Header.Set("Openai-Sentinel-Turnstile-Token", opt.TurnstileToken)
	}
	if opt.SOToken != "" {
		req.Header.Set("OpenAI-Sentinel-SO-Token", opt.SOToken)
	}

	res, err := c.hc.Do(req)
	if err != nil {
		return "", err
	}
	defer res.Body.Close()
	buf, _ := io.ReadAll(res.Body)
	if res.StatusCode >= 400 {
		return "", &UpstreamError{Status: res.StatusCode, Message: "f/conversation/prepare failed", Body: string(buf)}
	}
	var out struct {
		ConduitToken string `json:"conduit_token"`
	}
	_ = json.Unmarshal(buf, &out)
	return out.ConduitToken, nil
}

// StreamFConversation 对应 /backend-api/f/conversation(图像走和文字同一端点)。
//
// payload 字段集参考社区维护的 OpenaiChat provider(它在免费/付费账号上实测可用):
// 不带 client_prepare_state / force_parallel_switch;message.metadata 只带
// serialization_metadata + system_hints(有图时再补 attachments)。
func (c *Client) StreamFConversation(ctx context.Context, opt ImageConvOpts) (<-chan SSEEvent, error) {
	if opt.UpstreamModel == "" {
		opt.UpstreamModel = "auto"
	}
	if opt.MessageID == "" {
		opt.MessageID = uuid.NewString()
	}
	if opt.ParentMsgID == "" {
		opt.ParentMsgID = uuid.NewString()
	}
	if opt.TimezoneOffset == 0 {
		opt.TimezoneOffset = c.profile.TimezoneOffsetMinutes(time.Now())
	}
	if opt.SSETimeout == 0 {
		opt.SSETimeout = 180 * time.Second
	}

	// 构造 messages[0] 的 content 与 metadata,按是否有 reference 图决定 multimodal_text。
	// metadata 严格对齐 HAR 抓包的 **image 通路**:
	//   developer_mode_connector_ids: []
	//   selected_github_repos: []
	//   selected_all_github_repos: false
	//   system_hints: ["picture_v2"]            ← image 通路特有
	//   serialization_metadata.custom_symbol_offsets: []
	// 注意:image 通路 **不带 selected_sources**(那是 text 通路专属)。写错会让
	// 上游识别为"客户端类型不匹配",触发 silent rejection。
	msgContent := map[string]interface{}{"content_type": "text", "parts": []string{opt.Prompt}}
	msgMeta := map[string]interface{}{
		"developer_mode_connector_ids": []interface{}{},
		"selected_github_repos":        []interface{}{},
		"selected_all_github_repos":    false,
		"system_hints":                 []string{"picture_v2"},
		"serialization_metadata": map[string]interface{}{
			"custom_symbol_offsets": []interface{}{},
		},
	}
	if len(opt.References) > 0 {
		parts := make([]interface{}, 0, len(opt.References)+1)
		atts := make([]Attachment, 0, len(opt.References))
		for _, u := range opt.References {
			if u == nil || u.FileID == "" {
				continue
			}
			parts = append(parts, u.ToAssetPointerPart())
			atts = append(atts, u.ToAttachment())
		}
		parts = append(parts, opt.Prompt)
		msgContent = map[string]interface{}{
			"content_type": "multimodal_text",
			"parts":        parts,
		}
		msgMeta["attachments"] = atts
	}

	// 顶层 payload 对齐 HAR /f/conversation(image 通路):
	//   client_prepare_state: "sent"
	//   system_hints: ["picture_v2"]
	//   force_parallel_switch: "auto"            ← 必带
	//   client_contextual_info: 7 个字段 + app_name
	payload := map[string]interface{}{
		"action": "next",
		"messages": []map[string]interface{}{{
			"id":          opt.MessageID,
			"author":      map[string]string{"role": "user"},
			"create_time": float64(time.Now().UnixMilli()) / 1000.0,
			"content":     msgContent,
			"metadata":    msgMeta,
		}},
		"parent_message_id":        opt.ParentMsgID,
		"model":                    opt.UpstreamModel,
		"client_prepare_state":     "sent",
		"timezone_offset_min":      opt.TimezoneOffset,
		"timezone":                 c.profile.Timezone,
		"conversation_mode":        map[string]string{"kind": "primary_assistant"},
		"enable_message_followups": true,
		"system_hints":             []string{"picture_v2"},
		"supports_buffering":       true,
		"supported_encodings":      []string{"v1"},
		"client_contextual_info": map[string]interface{}{
			"is_dark_mode":      false,
			"time_since_loaded": 1200,
			"page_height":       1072,
			"page_width":        1724,
			"pixel_ratio":       1.2,
			"screen_height":     1440,
			"screen_width":      2560,
			"app_name":          "chatgpt.com",
		},
		"paragen_cot_summary_display_override": "allow",
		"force_parallel_switch":                "auto",
	}
	// 新会话不带 conversation_id(对齐浏览器抓包);已有会话才带
	if opt.ConvID != "" {
		payload["conversation_id"] = opt.ConvID
	}
	body, _ := json.Marshal(payload)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.opts.BaseURL+"/backend-api/f/conversation",
		strings.NewReader(string(body)))
	if err != nil {
		return nil, err
	}
	c.commonHeaders(req)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	// X-Oai-Turn-Trace-Id:每 turn 一个新 UUID。见 fchat.go 说明。
	req.Header.Set("X-Oai-Turn-Trace-Id", uuid.NewString())
	req.Header.Set("Openai-Sentinel-Chat-Requirements-Token", opt.ChatToken)
	if opt.ProofToken != "" {
		req.Header.Set("Openai-Sentinel-Proof-Token", opt.ProofToken)
	}
	if opt.TurnstileToken != "" {
		req.Header.Set("Openai-Sentinel-Turnstile-Token", opt.TurnstileToken)
	}
	if opt.SOToken != "" {
		req.Header.Set("OpenAI-Sentinel-SO-Token", opt.SOToken)
	}
	if opt.ConduitToken != "" {
		req.Header.Set("X-Conduit-Token", opt.ConduitToken)
	}

	local := *c.hc
	local.Timeout = 0 // 由 ctx 控制

	res, err := local.Do(req)
	if err != nil {
		return nil, err
	}
	if res.StatusCode >= 400 {
		buf, _ := io.ReadAll(res.Body)
		res.Body.Close()
		if l := loggerL(); l != nil {
			l.Error("chatgpt StreamFConversation rejected",
				zap.Int("status", res.StatusCode),
				zap.String("body", string(buf)),
				zap.String("cf_ray", res.Header.Get("cf-ray")),
				zap.String("content_type", res.Header.Get("Content-Type")),
			)
		}
		return nil, &UpstreamError{
			Status:  res.StatusCode,
			Message: fmt.Sprintf("f/conversation failed (status %d): %s", res.StatusCode, strings.TrimSpace(string(buf))),
			Body:    string(buf),
		}
	}
	out := make(chan SSEEvent, 64)
	go parseSSE(res.Body, out, opt.SSETimeout)
	return out, nil
}

// ImageSSEResult 是 ParseImageSSE 的扫描结果。
type ImageSSEResult struct {
	ConversationID string   // SSE 里捕获到的新会话 id(可能和入参不同)
	FileIDs        []string // file-service:// 引用(直出灰度图就在这里)
	SedimentIDs    []string // sediment:// 引用(通常是预览,需要轮询)
	FinishType     string   // finish_details.type(interrupted/stop/...)
	ImageGenTaskID string
	ToolInvoked    bool
	TurnUseCase    string
	SawHandoff     bool
	AssistantText  string
	TimedOut       bool
	Termination    string
	ReadFailure    SSEReadFailureClass
}

const (
	ImageSSETerminationStreamClosed = "stream_closed"
	ImageSSETerminationStreamError  = "stream_error"
	ImageSSETerminationDone         = "done"
	ImageSSETerminationReady        = "ready"
	ImageSSETerminationPollable     = "pollable"
	ImageSSETerminationTimeout      = "timeout"
)

var (
	reFileRef = regexp.MustCompile(`file-service://([A-Za-z0-9_-]+)`)
	reSedRef  = regexp.MustCompile(`sediment://([A-Za-z0-9_-]+)`)
)

type ImageSSEParseOpts struct {
	ExpectedN          int
	ReturnWhenPollable bool
	MaxWait            time.Duration
}

func (r ImageSSEResult) ShouldPollForImage() bool {
	if r.ConversationID == "" {
		return false
	}
	return r.ImageGenTaskID != "" ||
		r.ToolInvoked ||
		r.TurnUseCase == "image gen" ||
		r.SawHandoff
}

// HasStartedImageTask reports concrete image output work, not merely an image
// turn intent. Intent-only metadata is still useful for entering conversation
// polling, but must not hide a terminal assistant response during that poll.
func (r ImageSSEResult) HasStartedImageTask() bool {
	return r.ImageGenTaskID != "" || len(r.FileIDs) > 0 || len(r.SedimentIDs) > 0
}

// ParseImageSSE 消费 SSE 事件流,把图像相关的字段提取出来。
// 调用方可以根据返回的 FileIDs 判断是否已灰度直出。
func ParseImageSSE(stream <-chan SSEEvent) ImageSSEResult {
	return ParseImageSSEUntilReady(stream, ImageSSEParseOpts{})
}

func ParseImageSSEUntilReady(stream <-chan SSEEvent, opt ImageSSEParseOpts) ImageSSEResult {
	var r ImageSSEResult
	seenFile := map[string]struct{}{}
	seenSed := map[string]struct{}{}

	var timeout <-chan time.Time
	var timer *time.Timer
	if opt.MaxWait > 0 {
		timer = time.NewTimer(opt.MaxWait)
		defer timer.Stop()
		timeout = timer.C
	}

	for {
		select {
		case ev, ok := <-stream:
			if !ok {
				r.Termination = ImageSSETerminationStreamClosed
				return r
			}
			if ev.Err != nil {
				r.Termination = ImageSSETerminationStreamError
				r.ReadFailure = ev.ReadFailure
				if r.ReadFailure == "" {
					r.ReadFailure = SSEReadFailureOtherIO
				}
				return r
			}
			data := ev.Data
			if len(data) == 0 {
				continue
			}
			if string(data) == "[DONE]" {
				r.Termination = ImageSSETerminationDone
				return r
			}

			updateImageSSEResult(&r, data, seenFile, seenSed)
			if opt.ExpectedN > 0 && len(r.FileIDs)+len(r.SedimentIDs) >= opt.ExpectedN {
				r.Termination = ImageSSETerminationReady
				return r
			}
			if opt.ReturnWhenPollable && r.ShouldPollForImage() {
				r.Termination = ImageSSETerminationPollable
				return r
			}
		case <-timeout:
			r.TimedOut = true
			r.Termination = ImageSSETerminationTimeout
			return r
		}
	}
}

func updateImageSSEResult(r *ImageSSEResult, data []byte, seenFile, seenSed map[string]struct{}) {
	var obj map[string]interface{}
	if err := json.Unmarshal(data, &obj); err != nil {
		return
	}
	if cid, _ := obj["conversation_id"].(string); cid != "" && r.ConversationID == "" {
		r.ConversationID = cid
	}
	switch typ, _ := obj["type"].(string); typ {
	case "stream_handoff":
		r.SawHandoff = true
	case "server_ste_metadata":
		if meta, _ := obj["metadata"].(map[string]interface{}); meta != nil {
			if invoked, ok := meta["tool_invoked"].(bool); ok {
				r.ToolInvoked = invoked
			}
			if useCase, _ := meta["turn_use_case"].(string); useCase != "" {
				r.TurnUseCase = useCase
			}
		}
	}

	if v, ok := obj["v"].(map[string]interface{}); ok {
		if cid, ok := v["conversation_id"].(string); ok && cid != "" && r.ConversationID == "" {
			r.ConversationID = cid
		}
	}

	msg := imageSSEMessage(obj)
	if msg != nil {
		if text := assistantTextFromSSEMessage(msg); text != "" {
			r.AssistantText = text
		}
		if meta, ok := msg["metadata"].(map[string]interface{}); ok {
			if tid, ok := meta["image_gen_task_id"].(string); ok {
				r.ImageGenTaskID = tid
			}
			if fd, ok := meta["finish_details"].(map[string]interface{}); ok {
				if ft, ok := fd["type"].(string); ok {
					r.FinishType = ft
				}
			}
		}
	}

	isPatchEvent := obj["o"] == "patch"
	isUserMsg := isUserMessageSSEEvent(obj)
	imageContext := isImageOutputSSEMessage(msg) ||
		(r.ToolInvoked && !isUserMsg) ||
		(isPatchEvent && !isUserMsg && hasImageAssetPointerInText(string(data)))
	if !imageContext {
		return
	}

	addUniqueRefs(data, reFileRef, seenFile, func(id string) {
		r.FileIDs = append(r.FileIDs, id)
	})
	addUniqueRefs(data, reSedRef, seenSed, func(id string) {
		r.SedimentIDs = append(r.SedimentIDs, id)
	})
}

func assistantTextFromSSEMessage(msg map[string]interface{}) string {
	if msg == nil {
		return ""
	}
	author, _ := msg["author"].(map[string]interface{})
	if strings.TrimSpace(strings.ToLower(toString(author["role"]))) != "assistant" {
		return ""
	}
	content, _ := msg["content"].(map[string]interface{})
	if content == nil || toString(content["content_type"]) != "text" {
		return ""
	}
	return strings.TrimSpace(collectTextParts(content["parts"]))
}

func imageSSEMessage(event map[string]interface{}) map[string]interface{} {
	if msg, ok := event["message"].(map[string]interface{}); ok {
		return msg
	}
	if v, ok := event["v"].(map[string]interface{}); ok {
		if msg, ok := v["message"].(map[string]interface{}); ok {
			return msg
		}
	}
	return nil
}

func isUserMessageSSEEvent(event map[string]interface{}) bool {
	msg := imageSSEMessage(event)
	if msg == nil {
		return false
	}
	author, _ := msg["author"].(map[string]interface{})
	role, _ := author["role"].(string)
	return strings.TrimSpace(strings.ToLower(role)) == "user"
}

func isImageOutputSSEMessage(msg map[string]interface{}) bool {
	if msg == nil {
		return false
	}
	author, _ := msg["author"].(map[string]interface{})
	role := strings.TrimSpace(strings.ToLower(toString(author["role"])))
	if role != "tool" && role != "assistant" {
		return false
	}
	meta, _ := msg["metadata"].(map[string]interface{})
	content, _ := msg["content"].(map[string]interface{})
	if toString(meta["async_task_type"]) == "image_gen" {
		return true
	}
	return hasImageAssetPointer(content) || hasImageAssetPointer(meta)
}

func addUniqueRefs(raw []byte, re *regexp.Regexp, seen map[string]struct{}, add func(string)) {
	for _, m := range re.FindAllSubmatch(raw, -1) {
		id := string(m[1])
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		add(id)
	}
}

func hasImageAssetPointerInText(text string) bool {
	return strings.Contains(text, "asset_pointer") ||
		strings.Contains(text, "file-service://") ||
		strings.Contains(text, "sediment://")
}

func hasImageAssetPointer(v interface{}) bool {
	switch x := v.(type) {
	case nil:
		return false
	case string:
		return strings.HasPrefix(x, "file-service://") ||
			strings.HasPrefix(x, "sediment://") ||
			strings.Contains(x, "file-service://") ||
			strings.Contains(x, "sediment://")
	case []interface{}:
		for _, item := range x {
			if hasImageAssetPointer(item) {
				return true
			}
		}
	case map[string]interface{}:
		if asset := toString(x["asset_pointer"]); strings.HasPrefix(asset, "file-service://") || strings.HasPrefix(asset, "sediment://") {
			return true
		}
		for _, item := range x {
			if hasImageAssetPointer(item) {
				return true
			}
		}
	}
	return false
}

func imageRefsFromValue(v interface{}, re *regexp.Regexp) []string {
	raw, err := json.Marshal(v)
	if err != nil {
		return nil
	}
	var out []string
	seen := map[string]struct{}{}
	for _, m := range re.FindAllSubmatch(raw, -1) {
		id := string(m[1])
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		out = append(out, id)
	}
	return out
}

func toString(v interface{}) string {
	s, _ := v.(string)
	return s
}

// ImageToolMsg 是 conversation.mapping 里一条 IMG2 tool 消息的关键字段。
type ImageToolMsg struct {
	MessageID     string
	CreateTime    float64
	ModelSlug     string
	Recipient     string
	AuthorName    string
	ImageGenTitle string
	FileIDs       []string // file-service
	SedimentIDs   []string // sediment
}

// ImageErrorMsg 是 conversation.mapping 里一条与 image_gen 相关的 assistant 错误消息。
type ImageErrorMsg struct {
	MessageID     string
	CreateTime    float64
	Recipient     string
	AsyncTaskType string
	AsyncTaskID   string
	IsError       bool
	Hidden        bool
	Text          string
	ErrorType     string // content_policy / async_error
}

// AssistantTextReply 是上游把图片请求当普通聊天处理后的文本回复。
type AssistantTextReply struct {
	MessageID  string
	CreateTime float64
	Recipient  string
	Text       string
}

// ConversationSummary is the bounded identity/time projection returned by the
// recent-conversation endpoint. Message content remains owned by
// GetConversationMapping and is not duplicated into this list contract.
type ConversationSummary struct {
	ID string
}

func (summary *ConversationSummary) UnmarshalJSON(data []byte) error {
	var raw struct {
		ID             string `json:"id"`
		ConversationID string `json:"conversation_id"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	summary.ID = strings.TrimSpace(raw.ID)
	if summary.ID == "" {
		summary.ID = strings.TrimSpace(raw.ConversationID)
	}
	return nil
}

// ListRecentConversations returns the most recently updated conversation
// summaries without mutating or starting a conversation.
func (c *Client) ListRecentConversations(
	ctx context.Context,
	limit int,
) ([]ConversationSummary, error) {
	if limit < 1 || limit > 20 {
		return nil, fmt.Errorf("conversation limit must be between 1 and 20")
	}
	endpoint, err := url.Parse(c.opts.BaseURL + "/backend-api/conversations")
	if err != nil {
		return nil, err
	}
	query := endpoint.Query()
	query.Set("offset", "0")
	query.Set("limit", strconv.Itoa(limit))
	query.Set("order", "updated")
	query.Set("conversation_filter", "all")
	endpoint.RawQuery = query.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
	if err != nil {
		return nil, err
	}
	c.commonHeaders(req)
	req.Header.Set("Accept", "application/json")

	res, err := c.hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	buf, err := io.ReadAll(res.Body)
	if err != nil {
		return nil, err
	}
	if res.StatusCode >= 400 {
		return nil, &UpstreamError{
			Status: res.StatusCode, Message: "conversations list failed", Body: string(buf),
		}
	}
	var out struct {
		Items         []ConversationSummary `json:"items"`
		Conversations []ConversationSummary `json:"conversations"`
	}
	if err := json.Unmarshal(buf, &out); err != nil {
		return nil, fmt.Errorf("decode conversations list: %w", err)
	}
	if out.Items != nil {
		return out.Items, nil
	}
	return out.Conversations, nil
}

// GetConversationMapping 读取会话全量 mapping(轮询用)。
func (c *Client) GetConversationMapping(ctx context.Context, convID string) (map[string]interface{}, error) {
	if convID == "" {
		return nil, fmt.Errorf("conv_id required")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		c.opts.BaseURL+"/backend-api/conversation/"+convID, nil)
	if err != nil {
		return nil, err
	}
	c.commonHeaders(req)
	req.Header.Set("Accept", "*/*")

	res, err := c.hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	buf, _ := io.ReadAll(res.Body)
	if res.StatusCode >= 400 {
		return nil, &UpstreamError{Status: res.StatusCode, Message: "conversation get failed", Body: string(buf)}
	}
	var out map[string]interface{}
	if err := json.Unmarshal(buf, &out); err != nil {
		return nil, fmt.Errorf("decode conversation: %w", err)
	}
	return out, nil
}

// ExtractImageToolMsgs 从 conversation.mapping 里提取所有 IMG2 tool 消息。
func ExtractImageToolMsgs(mapping map[string]interface{}) []ImageToolMsg {
	out := make([]ImageToolMsg, 0, 4)
	for mid, raw := range mapping {
		node, _ := raw.(map[string]interface{})
		if node == nil {
			continue
		}
		msg, _ := node["message"].(map[string]interface{})
		if msg == nil {
			continue
		}
		author, _ := msg["author"].(map[string]interface{})
		meta, _ := msg["metadata"].(map[string]interface{})
		content, _ := msg["content"].(map[string]interface{})
		if author == nil {
			continue
		}
		role := strings.TrimSpace(strings.ToLower(toString(author["role"])))
		if role != "tool" && role != "assistant" {
			continue
		}
		isImageGen := meta != nil && toString(meta["async_task_type"]) == "image_gen"
		hasAsset := hasImageAssetPointer(content) || hasImageAssetPointer(meta)
		if !isImageGen && !hasAsset {
			continue
		}

		tm := ImageToolMsg{MessageID: mid}
		if v, ok := msg["create_time"].(float64); ok {
			tm.CreateTime = v
		}
		if v, ok := meta["model_slug"].(string); ok {
			tm.ModelSlug = v
		}
		if v, ok := msg["recipient"].(string); ok {
			tm.Recipient = v
		}
		if v, ok := author["name"].(string); ok {
			tm.AuthorName = v
		}
		if v, ok := meta["image_gen_title"].(string); ok {
			tm.ImageGenTitle = v
		}

		seenF := map[string]struct{}{}
		seenS := map[string]struct{}{}
		appendFile := func(id string) {
			if _, ok := seenF[id]; ok {
				return
			}
			seenF[id] = struct{}{}
			tm.FileIDs = append(tm.FileIDs, id)
		}
		appendSed := func(id string) {
			if _, ok := seenS[id]; ok {
				return
			}
			seenS[id] = struct{}{}
			tm.SedimentIDs = append(tm.SedimentIDs, id)
		}
		for _, id := range imageRefsFromValue(content, reFileRef) {
			appendFile(id)
		}
		for _, id := range imageRefsFromValue(meta, reFileRef) {
			appendFile(id)
		}
		for _, id := range imageRefsFromValue(content, reSedRef) {
			appendSed(id)
		}
		for _, id := range imageRefsFromValue(meta, reSedRef) {
			appendSed(id)
		}
		out = append(out, tm)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreateTime < out[j].CreateTime })
	return out
}

// ExtractImageErrorMsgs 提取 image_gen 相关的 assistant 错误消息。
func ExtractImageErrorMsgs(mapping map[string]interface{}) []ImageErrorMsg {
	out := make([]ImageErrorMsg, 0, 2)
	for mid, raw := range mapping {
		node, _ := raw.(map[string]interface{})
		if node == nil {
			continue
		}
		msg, _ := node["message"].(map[string]interface{})
		if msg == nil {
			continue
		}
		author, _ := msg["author"].(map[string]interface{})
		if author == nil {
			continue
		}
		if s, _ := author["role"].(string); s != "assistant" {
			continue
		}
		content, _ := msg["content"].(map[string]interface{})
		if content == nil {
			continue
		}
		if s, _ := content["content_type"].(string); s != "text" {
			continue
		}
		meta, _ := msg["metadata"].(map[string]interface{})
		text := collectTextParts(content["parts"])
		if strings.TrimSpace(text) == "" {
			continue
		}

		em := ImageErrorMsg{
			MessageID: mid,
			Text:      strings.TrimSpace(text),
		}
		if v, ok := msg["create_time"].(float64); ok {
			em.CreateTime = v
		}
		if v, ok := msg["recipient"].(string); ok {
			em.Recipient = v
		}
		if meta != nil {
			if v, ok := meta["async_task_type"].(string); ok {
				em.AsyncTaskType = v
			}
			if v, ok := meta["async_task_id"].(string); ok {
				em.AsyncTaskID = v
			}
			if v, ok := meta["is_error"].(bool); ok {
				em.IsError = v
			}
			if v, ok := meta["is_visually_hidden_from_conversation"].(bool); ok {
				em.Hidden = v
			}
		}
		if em.AsyncTaskType != "image_gen" && !em.IsError && !em.Hidden {
			continue
		}
		if IsContentPolicyText(em.Text) {
			em.ErrorType = "content_policy"
		} else {
			em.ErrorType = "async_error"
		}
		out = append(out, em)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreateTime < out[j].CreateTime })
	return out
}

// ExtractAssistantTextReplies 提取非 image_gen async_error 的普通 assistant 文本回复。
func ExtractAssistantTextReplies(mapping map[string]interface{}) []AssistantTextReply {
	out := make([]AssistantTextReply, 0, 2)
	for mid, raw := range mapping {
		node, _ := raw.(map[string]interface{})
		if node == nil {
			continue
		}
		msg, _ := node["message"].(map[string]interface{})
		if msg == nil {
			continue
		}
		author, _ := msg["author"].(map[string]interface{})
		if author == nil {
			continue
		}
		if role, _ := author["role"].(string); role != "assistant" {
			continue
		}
		content, _ := msg["content"].(map[string]interface{})
		if content == nil {
			continue
		}
		if ctype, _ := content["content_type"].(string); ctype != "text" {
			continue
		}
		meta, _ := msg["metadata"].(map[string]interface{})
		if meta != nil {
			if taskType, _ := meta["async_task_type"].(string); taskType == "image_gen" {
				continue
			}
			if isErr, _ := meta["is_error"].(bool); isErr {
				continue
			}
			if hidden, _ := meta["is_visually_hidden_from_conversation"].(bool); hidden {
				continue
			}
		}
		text := strings.TrimSpace(collectTextParts(content["parts"]))
		if text == "" {
			continue
		}
		reply := AssistantTextReply{
			MessageID: mid,
			Text:      text,
		}
		if v, ok := msg["create_time"].(float64); ok {
			reply.CreateTime = v
		}
		if v, ok := msg["recipient"].(string); ok {
			reply.Recipient = v
		}
		out = append(out, reply)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreateTime < out[j].CreateTime })
	return out
}

func pickBestAssistantTextReply(msgs []AssistantTextReply) *AssistantTextReply {
	if len(msgs) == 0 {
		return nil
	}
	return &msgs[len(msgs)-1]
}

func pickBestImageError(msgs []ImageErrorMsg) *ImageErrorMsg {
	if len(msgs) == 0 {
		return nil
	}
	for i := range msgs {
		if msgs[i].AsyncTaskType == "image_gen" && msgs[i].IsError && msgs[i].ErrorType == "content_policy" {
			return &msgs[i]
		}
	}
	for i := range msgs {
		if msgs[i].AsyncTaskType == "image_gen" && msgs[i].IsError {
			return &msgs[i]
		}
	}
	for i := range msgs {
		if msgs[i].ErrorType == "content_policy" {
			return &msgs[i]
		}
	}
	return &msgs[len(msgs)-1]
}

func collectTextParts(raw interface{}) string {
	parts, _ := raw.([]interface{})
	var text []string
	for _, part := range parts {
		if s, ok := part.(string); ok && strings.TrimSpace(s) != "" {
			text = append(text, strings.TrimSpace(s))
		}
	}
	return strings.Join(text, "\n")
}

// IsContentPolicyText 识别上游生图内容政策、安全围栏、风控违规等拒绝文案。
func IsContentPolicyText(text string) bool {
	s := strings.ToLower(strings.TrimSpace(text))
	if s == "" {
		return false
	}

	// 中文特征
	if strings.Contains(s, "内容政策") ||
		strings.Contains(s, "内容安全") ||
		strings.Contains(s, "安全策略") ||
		strings.Contains(s, "安全系统") ||
		strings.Contains(s, "安全准则") ||
		strings.Contains(s, "安全规范") ||
		strings.Contains(s, "违反政策") ||
		strings.Contains(s, "违反策略") ||
		strings.Contains(s, "违背政策") ||
		strings.Contains(s, "违规") {
		return true
	}

	// 英文政策与围栏特征 (支持 policy / policies / guidelines / guardrails / scam)
	if strings.Contains(s, "content policy") ||
		strings.Contains(s, "content policies") ||
		strings.Contains(s, "content polic") ||
		strings.Contains(s, "safety policy") ||
		strings.Contains(s, "safety policies") ||
		strings.Contains(s, "safety polic") ||
		strings.Contains(s, "usage policy") ||
		strings.Contains(s, "usage policies") ||
		strings.Contains(s, "safety system") ||
		strings.Contains(s, "safety guidelines") ||
		strings.Contains(s, "policy violation") ||
		strings.Contains(s, "policies violation") ||
		strings.Contains(s, "guardrail") ||
		strings.Contains(s, "fraudulent or scam") {
		return true
	}

	// 组合判定: 包含 violate / violation 并且涉及 policy / policies / terms / guidelines / guardrails
	if (strings.Contains(s, "violate") || strings.Contains(s, "violat") || strings.Contains(s, "violation")) &&
		(strings.Contains(s, "polic") || strings.Contains(s, "guardrail") || strings.Contains(s, "guideline") || strings.Contains(s, "safety") || strings.Contains(s, "term")) {
		return true
	}

	return false
}

func isImageToolArgumentText(text string) bool {
	s := strings.ToLower(strings.TrimSpace(text))
	if s == "" {
		return false
	}
	if strings.Contains(s, `"referenced_image_ids"`) {
		return true
	}
	return strings.Contains(s, `"prompt"`) &&
		strings.Contains(s, `"size"`) &&
		strings.Contains(s, `"n"`)
}

// PollOpts 控制 PollConversationForImages 的等待策略。
//
// IMG2 正式上线后不再做灰度命中判定,逻辑极简化:
//   - 任一 tool 消息里出现 file-service / sediment asset_pointer → 立即算成功
//   - 够 ExpectedN 张就立即返回,不等多余的
//   - 没够数继续轮询,到 MaxWait 仍有至少 1 张也算成功(速度优先)
//   - 全程没拿到图才是 timeout
type PollOpts struct {
	BaselineToolIDs  map[string]struct{} // 发送前已存在的 tool 消息 id,本次回合只看新增
	ExpectedN        int                 // 期望返回的图片张数,够了立即短路,默认 1
	MaxWait          time.Duration       // 总超时,默认 2m
	Interval         time.Duration       // 轮询间隔,默认 3s
	ImageTaskStarted bool                // SSE 已返回 image_gen_task_id/图片引用时,assistant 文本不视为终态
}

// PollStatus 是 PollConversationForImages 的结果状态。
type PollStatus string

const (
	PollStatusSuccess  PollStatus = "success" // 至少拿到 1 张图
	PollStatusTimeout  PollStatus = "timeout" // 等到 MaxWait 仍然 0 张
	PollStatusRejected PollStatus = "rejected"
	PollStatusError    PollStatus = "error" // 上游错误(429 熔断 / ctx 取消)
)

// PollDebug 是一次 conversation 轮询的观察结果,用于定位卡在哪一层。
type PollDebug struct {
	Attempts                int
	ConversationErrors      int
	RateLimitHits           int
	ToolMessages            int
	NewToolMessages         int
	ErrorMessages           int
	AssistantReplies        int
	LastToolModelSlug       string
	LastToolRecipient       string
	LastToolAuthor          string
	LastToolTitle           string
	AsyncErrorType          string
	AsyncErrorText          string
	AsyncErrorMessageID     string
	AssistantReplyText      string
	AssistantReplyMessageID string
	LastError               string
}

const (
	maxConsecutiveConversationPollErrors = 3
	maxStartedImageTaskEmptyPollWait     = 2 * time.Minute
	backendTasksPollTimeout              = 5 * time.Second
)

func normalizePollOpts(opt PollOpts) PollOpts {
	if opt.ExpectedN <= 0 {
		opt.ExpectedN = 1
	}
	if opt.MaxWait <= 0 {
		opt.MaxWait = 2 * time.Minute
	}
	if opt.Interval <= 0 {
		opt.Interval = 3 * time.Second
	}
	if opt.ImageTaskStarted && opt.MaxWait > maxStartedImageTaskEmptyPollWait {
		opt.MaxWait = maxStartedImageTaskEmptyPollWait
	}
	return opt
}

// PollConversationForImages 轮询会话直到图片可用。
//
// 返回 (status, file_ids, sediment_ids)。status=success 时 file_ids/sediment_ids 至少一个非空。
// file-service 优先(优先级更高),sediment 作为补充一并带出,调用方自行决定用几张。
func (c *Client) PollConversationForImages(ctx context.Context, convID string, opt PollOpts) (PollStatus, []string, []string, PollDebug) {
	opt = normalizePollOpts(opt)
	baseline := opt.BaselineToolIDs
	var dbg PollDebug

	deadline := time.Now().Add(opt.MaxWait)

	// 累计全程看到的 fid/sid,循环外可用(超时兜底:有图就算成功)
	var (
		allFile        []string
		allSed         []string
		seenFile       = map[string]struct{}{}
		seenSed        = map[string]struct{}{}
		consecutive429 int
		consecutiveErr int
	)

	for time.Now().Before(deadline) {
		dbg.Attempts++
		select {
		case <-ctx.Done():
			dbg.LastError = ctx.Err().Error()
			return PollStatusError, nil, nil, dbg
		default:
		}

		if taskErr := c.pollBackendTaskError(ctx, convID, deadline); taskErr != nil {
			if opt.ImageTaskStarted && isImageToolArgumentText(taskErr.Text) {
				// ChatGPT can store the image tool argument JSON as an
				// assistant metadata.is_error task row while the real image is
				// still delivered later through conversation tool messages.
				// Treat only that shape as non-terminal; real policy errors
				// still fail fast below.
			} else {
				dbg.ErrorMessages = 1
				dbg.AsyncErrorType = taskErr.ErrorType
				dbg.AsyncErrorText = taskErr.Text
				dbg.AsyncErrorMessageID = taskErr.MessageID
				return PollStatusRejected, nil, nil, dbg
			}
		}

		mapping, err := c.getMappingRaw(ctx, convID)
		if err != nil {
			dbg.ConversationErrors++
			dbg.LastError = err.Error()
			if ue, ok := err.(*UpstreamError); ok && ue.Status == 429 {
				consecutive429++
				dbg.RateLimitHits++
				if consecutive429 >= 3 {
					return PollStatusError, nil, nil, dbg
				}
				sleep(ctx, 10*time.Second)
				continue
			}
			consecutiveErr++
			if consecutiveErr >= maxConsecutiveConversationPollErrors {
				return PollStatusError, nil, nil, dbg
			}
			sleep(ctx, opt.Interval)
			continue
		}
		consecutive429 = 0
		consecutiveErr = 0

		msgs := ExtractImageToolMsgs(mapping)
		dbg.ToolMessages = len(msgs)
		errMsgs := ExtractImageErrorMsgs(mapping)
		dbg.ErrorMessages = len(errMsgs)
		if bestErr := pickBestImageError(errMsgs); bestErr != nil {
			dbg.AsyncErrorType = bestErr.ErrorType
			dbg.AsyncErrorText = bestErr.Text
			dbg.AsyncErrorMessageID = bestErr.MessageID
			return PollStatusRejected, nil, nil, dbg
		}
		replyMsgs := ExtractAssistantTextReplies(mapping)
		dbg.AssistantReplies = len(replyMsgs)
		if bestReply := pickBestAssistantTextReply(replyMsgs); bestReply != nil {
			dbg.AssistantReplyText = bestReply.Text
			dbg.AssistantReplyMessageID = bestReply.MessageID
			if len(msgs) == 0 && !opt.ImageTaskStarted {
				return PollStatusRejected, nil, nil, dbg
			}
		}
		// baseline diff:只看本回合新增 tool 消息
		var newMsgs []ImageToolMsg
		if len(baseline) > 0 {
			for _, m := range msgs {
				if _, ok := baseline[m.MessageID]; !ok {
					newMsgs = append(newMsgs, m)
				}
			}
		} else {
			newMsgs = msgs
		}
		dbg.NewToolMessages = len(newMsgs)
		if len(newMsgs) > 0 {
			last := newMsgs[len(newMsgs)-1]
			dbg.LastToolModelSlug = last.ModelSlug
			dbg.LastToolRecipient = last.Recipient
			dbg.LastToolAuthor = last.AuthorName
			dbg.LastToolTitle = last.ImageGenTitle
		}

		// 跨 tool 消息聚合 fid/sid。IMG2 一次调用可能先出 sediment 预览再补
		// file-service 终稿,或同一条消息里带 N 张 file id;这里都累计起来。
		for _, m := range newMsgs {
			for _, f := range m.FileIDs {
				if _, ok := seenFile[f]; !ok {
					seenFile[f] = struct{}{}
					allFile = append(allFile, f)
				}
			}
			for _, s := range m.SedimentIDs {
				if _, ok := seenSed[s]; !ok {
					seenSed[s] = struct{}{}
					allSed = append(allSed, s)
				}
			}
		}

		// 够 N 张立即短路返回(file-service 优先占配额,sediment 补位)
		if len(allFile)+len(allSed) >= opt.ExpectedN {
			return PollStatusSuccess, allFile, allSed, dbg
		}

		sleep(ctx, opt.Interval)
	}

	// 超时兜底:只要拿到过至少 1 张,就算成功(速度优先,不等齐 N)
	if len(allFile)+len(allSed) > 0 {
		return PollStatusSuccess, allFile, allSed, dbg
	}
	return PollStatusTimeout, nil, nil, dbg
}

func (c *Client) pollBackendTaskError(ctx context.Context, convID string, deadline time.Time) *ImageErrorMsg {
	remaining := time.Until(deadline)
	if remaining <= 0 {
		return nil
	}
	timeout := backendTasksPollTimeout
	if remaining < timeout {
		timeout = remaining
	}
	tasks, err := c.queryBackendTasks(ctx, convID, timeout)
	if err != nil {
		return nil
	}
	errMsgs := ExtractImageErrorMsgs(tasksToImageErrorMapping(tasks))
	return pickBestImageError(errMsgs)
}

type backendTask struct {
	TaskID                 string                 `json:"task_id"`
	ConversationID         string                 `json:"conversation_id"`
	OriginalConversationID string                 `json:"original_conversation_id"`
	ImageGenMessage        map[string]interface{} `json:"image_gen_message"`
}

func (c *Client) queryBackendTasks(ctx context.Context, convID string, timeout time.Duration) ([]backendTask, error) {
	if timeout <= 0 {
		return nil, nil
	}
	reqCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet,
		c.opts.BaseURL+"/backend-api/tasks", nil)
	if err != nil {
		return nil, err
	}
	c.commonHeaders(req)
	req.Header.Set("Accept", "application/json")

	res, err := c.hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()

	buf, _ := io.ReadAll(res.Body)
	if res.StatusCode >= 400 {
		return nil, &UpstreamError{Status: res.StatusCode, Message: "tasks get failed", Body: string(buf)}
	}

	var out struct {
		Tasks []backendTask `json:"tasks"`
	}
	if err := json.Unmarshal(buf, &out); err != nil {
		return nil, fmt.Errorf("decode tasks: %w", err)
	}
	if convID == "" {
		return out.Tasks, nil
	}
	filtered := make([]backendTask, 0, len(out.Tasks))
	for _, task := range out.Tasks {
		if task.ConversationID == convID || task.OriginalConversationID == convID {
			filtered = append(filtered, task)
		}
	}
	return filtered, nil
}

func tasksToImageErrorMapping(tasks []backendTask) map[string]interface{} {
	mapping := make(map[string]interface{}, len(tasks))
	for idx, task := range tasks {
		if len(task.ImageGenMessage) == 0 {
			continue
		}
		messageID, _ := task.ImageGenMessage["id"].(string)
		if messageID == "" {
			messageID = task.TaskID
		}
		if messageID == "" {
			messageID = fmt.Sprintf("task_%d", idx)
		}
		mapping[messageID] = map[string]interface{}{
			"message": task.ImageGenMessage,
		}
	}
	return mapping
}

// getMappingRaw 拉 conversation 并返回 mapping。
func (c *Client) getMappingRaw(ctx context.Context, convID string) (map[string]interface{}, error) {
	full, err := c.GetConversationMapping(ctx, convID)
	if err != nil {
		return nil, err
	}
	mapping, _ := full["mapping"].(map[string]interface{})
	if mapping == nil {
		mapping = map[string]interface{}{}
	}
	return mapping, nil
}

// GetConversationHead 返回会话最新叶子消息 id(current_node),复用会话时做 parent_message_id。
func (c *Client) GetConversationHead(ctx context.Context, convID string) (string, error) {
	full, err := c.GetConversationMapping(ctx, convID)
	if err != nil {
		return "", err
	}
	head, _ := full["current_node"].(string)
	if head == "" {
		return "", fmt.Errorf("current_node missing")
	}
	return head, nil
}

// ImageDownloadURL 取单张图的签名下载 URL。
// fileRef 可以是:
//   - "xxxxxx"      → /backend-api/files/{fid}/download(file-service)
//   - "sed:xxxxxx"  → /backend-api/conversation/{cid}/attachment/{fid}/download(需要 convID)
func (c *Client) ImageDownloadURL(ctx context.Context, convID, fileRef string) (string, error) {
	var apiURL string
	if strings.HasPrefix(fileRef, "sed:") {
		if convID == "" {
			return "", fmt.Errorf("conv_id required for sediment")
		}
		fid := strings.TrimPrefix(fileRef, "sed:")
		apiURL = fmt.Sprintf("%s/backend-api/conversation/%s/attachment/%s/download",
			c.opts.BaseURL, url.PathEscape(convID), url.PathEscape(fid))
	} else {
		apiURL = fmt.Sprintf("%s/backend-api/files/%s/download",
			c.opts.BaseURL, url.PathEscape(fileRef))
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, apiURL, nil)
	if err != nil {
		return "", err
	}
	c.commonHeaders(req)
	req.Header.Set("Accept", "*/*")

	res, err := c.hc.Do(req)
	if err != nil {
		return "", err
	}
	defer res.Body.Close()
	buf, _ := io.ReadAll(res.Body)
	if res.StatusCode >= 400 {
		return "", &UpstreamError{Status: res.StatusCode, Message: "files/download failed", Body: string(buf)}
	}
	var out struct {
		DownloadURL string `json:"download_url"`
		Status      string `json:"status"`
		FileName    string `json:"file_name"`
	}
	if err := json.Unmarshal(buf, &out); err != nil {
		return "", fmt.Errorf("decode files/download: %w", err)
	}
	if out.DownloadURL == "" {
		return "", fmt.Errorf("empty download_url (status=%s)", out.Status)
	}
	return out.DownloadURL, nil
}

// FetchImage 通过无代理 client 下载指定 URL 的图片字节(调用 ImageDownloadURL
// 返回的签名 URL)。图片原始字节不得消耗账号控制代理流量。
// 返回 (bytes, content_type)。超过 maxBytes 的响应会被截断报错。
//
// 鉴权策略:
//   - files.oaiusercontent.com / cdn.oaistatic.com 等**预签名直链**:不带 Authorization,
//     它们依赖 URL 自带的 sig 参数鉴权;带 Bearer 反而会被某些 CDN 因"双鉴权"400。
//   - chatgpt.com/backend-api/estuary/... (sediment 回源 URL):必须带 Bearer,
//     但不需要 Cookie/Oai-* 账号身份头。该大图 GET 使用独立无代理 uTLS client,
//     控制请求仍保持账号绑定代理与浏览器身份。
func (c *Client) FetchImage(ctx context.Context, signedURL string, maxBytes int64) ([]byte, string, error) {
	if maxBytes <= 0 {
		maxBytes = 16 * 1024 * 1024 // 16MB 默认
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, signedURL, nil)
	if err != nil {
		return nil, "", err
	}

	// 以 BaseURL(通常 https://chatgpt.com)开头的 estuary 回源 URL 只需 Bearer;
	// 外部 CDN 直链依赖 URL 自带签名,不带任何账号凭据。
	needAuth := strings.HasPrefix(signedURL, c.opts.BaseURL+"/")
	if needAuth {
		ApplyBrowserFingerprintHeaders(req.Header, c.profile)
		req.Header.Set("Authorization", "Bearer "+c.opts.AuthToken)
		req.Header.Set("Origin", c.opts.BaseURL)
		req.Header.Set("Referer", c.opts.BaseURL+"/")
		req.Header.Set("Accept", "image/avif,image/webp,image/apng,image/svg+xml,image/*,*/*;q=0.8")
		req.Header.Set("Sec-Fetch-Dest", "image")
		req.Header.Set("Sec-Fetch-Mode", "no-cors")
		req.Header.Set("Sec-Fetch-Site", "same-origin")
	} else {
		req.Header.Set("User-Agent", DefaultUserAgent)
	}

	downloadClient := c.assetHC
	if needAuth {
		downloadClient = c.bearerAssetHC
	}
	res, err := downloadClient.Do(req)
	if err != nil {
		return nil, "", err
	}
	defer res.Body.Close()
	if res.StatusCode >= 400 {
		return nil, "", &UpstreamError{Status: res.StatusCode, Message: "fetch image failed"}
	}
	ct := res.Header.Get("Content-Type")
	body, err := io.ReadAll(io.LimitReader(res.Body, maxBytes+1))
	if err != nil {
		return nil, ct, err
	}
	if int64(len(body)) > maxBytes {
		return nil, ct, fmt.Errorf("image exceeds max bytes (%d)", maxBytes)
	}
	if c.opts.TrafficMeter != nil {
		c.opts.TrafficMeter.directAssetReadBytes.Add(int64(len(body)))
	}
	return body, ct, nil
}

// sleep 可取消的 sleep。
func sleep(ctx context.Context, d time.Duration) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
	}
}
