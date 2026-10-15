package gateway

import "strings"

type upstreamChatTarget struct {
	Model          string
	ThinkingEffort string
}

func resolveUpstreamChatTarget(modelSlug string, reasoning *ChatReasoningRequest) upstreamChatTarget {
	target := upstreamChatTarget{
		Model: mapUpstreamModelSlug(modelSlug),
	}

	if reasoning == nil {
		return target
	}

	thinkingEffort := mapReasoningEffortToThinkingEffort(reasoning.Effort)
	if thinkingEffort == "" {
		return target
	}

	thinkingModel, ok := mapThinkingModelSlug(target.Model)
	if !ok {
		return target
	}

	target.Model = thinkingModel
	target.ThinkingEffort = thinkingEffort
	return target
}

func mapReasoningEffortToThinkingEffort(effort string) string {
	switch strings.ToLower(strings.TrimSpace(effort)) {
	case "low":
		return "minimal"
	case "medium":
		return "standard"
	case "high":
		return "extended"
	default:
		return ""
	}
}

func mapThinkingModelSlug(model string) (string, bool) {
	switch mapUpstreamModelSlug(model) {
	case "gpt-5-2", "gpt-5-2-thinking":
		return "gpt-5-2-thinking", true
	case "gpt-5-4", "gpt-5-4-thinking":
		return "gpt-5-4-thinking", true
	case "gpt-5-5", "gpt-5-5-thinking":
		return "gpt-5-5-thinking", true
	default:
		return "", false
	}
}
