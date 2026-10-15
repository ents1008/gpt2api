package chatgpt

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	"github.com/google/uuid"
)

type fchatHandoffState struct {
	ConversationID string
	SawHandoff     bool
}

func (c *Client) wrapFChatStream(
	ctx context.Context,
	raw <-chan SSEEvent,
	sseTimeout time.Duration,
) <-chan SSEEvent {
	out := make(chan SSEEvent, 64)
	go func() {
		defer close(out)

		state := fchatHandoffState{}
	for ev := range raw {
			if ev.Err != nil {
				out <- ev
				return
			}
			if len(ev.Data) == 0 {
				continue
			}

			updateFChatHandoffState(&state, ev.Data)
			if isDoneSSEData(ev.Data) {
				if state.SawHandoff {
					c.recoverFChatAfterHandoff(ctx, out, state.ConversationID, sseTimeout)
					return
				}
				out <- ev
				return
			}

			out <- ev
		}

		if state.SawHandoff {
			c.recoverFChatAfterHandoff(ctx, out, state.ConversationID, sseTimeout)
		}
	}()
	return out
}

func updateFChatHandoffState(state *fchatHandoffState, data []byte) {
	var raw map[string]interface{}
	if err := json.Unmarshal(data, &raw); err != nil {
		return
	}
	if cid, _ := raw["conversation_id"].(string); cid != "" && state.ConversationID == "" {
		state.ConversationID = cid
	}
	if typ, _ := raw["type"].(string); typ == "stream_handoff" {
		state.SawHandoff = true
	}
}

func isDoneSSEData(data []byte) bool {
	return strings.TrimSpace(string(data)) == "[DONE]"
}

func (c *Client) recoverFChatAfterHandoff(
	ctx context.Context,
	out chan<- SSEEvent,
	conversationID string,
	sseTimeout time.Duration,
) {
	if conversationID == "" {
		out <- SSEEvent{Data: []byte("[DONE]")}
		return
	}

	waitTimeout := sseTimeout
	if waitTimeout < 30*time.Second {
		waitTimeout = 30 * time.Second
	}
	waitCtx, cancelWait := context.WithTimeout(ctx, waitTimeout)
	_, _ = c.waitForConversationComplete(waitCtx, conversationID)
	cancelWait()

	mapCtx, cancelMap := context.WithTimeout(ctx, 45*time.Second)
	defer cancelMap()
	reply, err := c.waitForConversationAssistantReply(mapCtx, conversationID)
	if err != nil || reply == nil || strings.TrimSpace(reply.Text) == "" {
		out <- SSEEvent{Data: []byte("[DONE]")}
		return
	}

	messageID := reply.MessageID
	if messageID == "" {
		messageID = uuid.NewString()
	}
	recipient := reply.Recipient
	if recipient == "" {
		recipient = "all"
	}
	payload, _ := json.Marshal(map[string]interface{}{
		"message": map[string]interface{}{
			"id":          messageID,
			"author":      map[string]string{"role": "assistant"},
			"recipient":   recipient,
			"create_time": reply.CreateTime,
			"status":      "finished_successfully",
			"content": map[string]interface{}{
				"content_type": "text",
				"parts":        []string{reply.Text},
			},
		},
	})
	out <- SSEEvent{Data: payload}
	out <- SSEEvent{Data: []byte("[DONE]")}
}

func (c *Client) waitForConversationComplete(ctx context.Context, conversationID string) (string, error) {
	ticker := time.NewTicker(3 * time.Second)
	defer ticker.Stop()

	lastStatus := ""
	for {
		status, err := c.GetConversationStreamStatus(ctx, conversationID)
		if err == nil {
			lastStatus = strings.TrimSpace(status)
			switch lastStatus {
			case "COMPLETE", "FAILED", "INTERRUPTED":
				return lastStatus, nil
			}
		}

		select {
		case <-ctx.Done():
			return lastStatus, ctx.Err()
		case <-ticker.C:
		}
	}
}

func (c *Client) waitForConversationAssistantReply(
	ctx context.Context,
	conversationID string,
) (*AssistantTextReply, error) {
	ticker := time.NewTicker(1500 * time.Millisecond)
	defer ticker.Stop()

	for {
		full, err := c.GetConversationMapping(ctx, conversationID)
		if err == nil {
			mapping, _ := full["mapping"].(map[string]interface{})
			if mapping != nil {
				reply := pickBestAssistantTextReply(ExtractAssistantTextReplies(mapping))
				if reply != nil && strings.TrimSpace(reply.Text) != "" {
					return reply, nil
				}
			}
		}

		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-ticker.C:
		}
	}
}
