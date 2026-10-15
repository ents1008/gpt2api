package gateway

import (
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

func TestChatCompletionsRequestBindsReasoningEffort(t *testing.T) {
	gin.SetMode(gin.TestMode)

	body := `{
		"model":"gpt-5-5",
		"messages":[{"role":"user","content":"hello"}],
		"reasoning":{"effort":"high"}
	}`
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
	ctx.Request = req

	var payload ChatCompletionsRequest
	if err := ctx.ShouldBindJSON(&payload); err != nil {
		t.Fatalf("bind request: %v", err)
	}

	reasoning := reflect.ValueOf(payload).FieldByName("Reasoning")
	if !reasoning.IsValid() {
		t.Fatalf("ChatCompletionsRequest should expose Reasoning field")
	}
	if reasoning.IsNil() {
		t.Fatalf("Reasoning should be populated when request contains reasoning.effort")
	}
	effort := reasoning.Elem().FieldByName("Effort")
	if !effort.IsValid() {
		t.Fatalf("Reasoning should expose Effort field")
	}
	if effort.String() != "high" {
		t.Fatalf("Reasoning.Effort = %q, want %q", effort.String(), "high")
	}
}
