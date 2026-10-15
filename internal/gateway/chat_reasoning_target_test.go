package gateway

import "testing"

// TestResolveUpstreamChatTargetHighEffortUsesThinkingSlug verifies that a high-effort reasoning request
// upgrades GPT-5.5 to the official thinking slug and emits the upstream thinking_effort value expected by ChatGPT Web.
func TestResolveUpstreamChatTargetHighEffortUsesThinkingSlug(t *testing.T) {
	target := resolveUpstreamChatTarget("gpt-5-5", &ChatReasoningRequest{Effort: "high"})

	if target.Model != "gpt-5-5-thinking" {
		t.Fatalf("Model = %q, want %q", target.Model, "gpt-5-5-thinking")
	}
	if target.ThinkingEffort != "extended" {
		t.Fatalf("ThinkingEffort = %q, want %q", target.ThinkingEffort, "extended")
	}
}

// TestResolveUpstreamChatTargetMediumEffortUsesThinkingSlug verifies that a medium-effort reasoning request
// upgrades GPT-5.4 to the thinking slug and maps the OpenAI-style effort name into the ChatGPT Web payload value.
func TestResolveUpstreamChatTargetMediumEffortUsesThinkingSlug(t *testing.T) {
	target := resolveUpstreamChatTarget("gpt-5-4", &ChatReasoningRequest{Effort: "medium"})

	if target.Model != "gpt-5-4-thinking" {
		t.Fatalf("Model = %q, want %q", target.Model, "gpt-5-4-thinking")
	}
	if target.ThinkingEffort != "standard" {
		t.Fatalf("ThinkingEffort = %q, want %q", target.ThinkingEffort, "standard")
	}
}

// TestResolveUpstreamChatTargetLeavesUnsupportedModelsUntouched verifies that models without a known thinking counterpart
// are not force-rewritten, which prevents accidentally attaching unsupported thinking parameters to unrelated slugs.
func TestResolveUpstreamChatTargetLeavesUnsupportedModelsUntouched(t *testing.T) {
	target := resolveUpstreamChatTarget("gpt-5-3", &ChatReasoningRequest{Effort: "high"})

	if target.Model != "gpt-5-3" {
		t.Fatalf("Model = %q, want %q", target.Model, "gpt-5-3")
	}
	if target.ThinkingEffort != "" {
		t.Fatalf("ThinkingEffort = %q, want empty", target.ThinkingEffort)
	}
}
