package account

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/432539/gpt2api/pkg/resp"
)

type fakeQuotaProbeRunner struct {
	singleID     uint64
	singleResult *QuotaResult
	singleErr    error

	allLimit  int
	allResult []*QuotaResult
	allErr    error
}

func (f *fakeQuotaProbeRunner) ProbeByID(_ context.Context, id uint64) (*QuotaResult, error) {
	f.singleID = id
	return f.singleResult, f.singleErr
}

func (f *fakeQuotaProbeRunner) ProbeAllActive(_ context.Context, limit int) ([]*QuotaResult, error) {
	f.allLimit = limit
	return f.allResult, f.allErr
}

type handlerResponseBody struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data"`
}

// TestProbeQuotaEndpointRunsInjectedQuotaProber verifies that the manual
// single-account endpoint reaches the quota runner instead of rejecting the
// request. Expected: account id from the route is passed through and the quota
// result is returned as the response data.
func TestProbeQuotaEndpointRunsInjectedQuotaProber(t *testing.T) {
	gin.SetMode(gin.TestMode)
	runner := &fakeQuotaProbeRunner{
		singleResult: &QuotaResult{
			AccountID:    17,
			Email:        "user@example.com",
			OK:           true,
			Remaining:    24,
			Total:        25,
			DefaultModel: "auto",
		},
	}
	h := NewHandler(nil)
	h.SetQuotaProber(runner)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "17"}}
	c.Request = httptest.NewRequest(http.MethodPost, "/api/admin/accounts/17/probe-quota", nil)

	h.ProbeQuota(c)

	if w.Code != http.StatusOK {
		t.Fatalf("ProbeQuota status = %d, want %d", w.Code, http.StatusOK)
	}
	if runner.singleID != 17 {
		t.Fatalf("ProbeByID id = %d, want 17", runner.singleID)
	}
	var body handlerResponseBody
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if body.Code != resp.CodeOK {
		t.Fatalf("ProbeQuota code = %d, want %d", body.Code, resp.CodeOK)
	}
	var got QuotaResult
	if err := json.Unmarshal(body.Data, &got); err != nil {
		t.Fatalf("decode quota result: %v", err)
	}
	if !got.OK || got.Remaining != 24 || got.DefaultModel != "auto" {
		t.Fatalf("quota result = %+v", got)
	}
}

// TestProbeQuotaEndpointReturnsProbeResultOnProbeError verifies failed upstream
// probes still return the structured QuotaResult. Expected: the frontend can
// display the account-specific failure without losing the result body to a 500.
func TestProbeQuotaEndpointReturnsProbeResultOnProbeError(t *testing.T) {
	gin.SetMode(gin.TestMode)
	runner := &fakeQuotaProbeRunner{
		singleResult: &QuotaResult{AccountID: 18, OK: false, Error: "上游拒绝访问(403)"},
		singleErr:    errors.New("conversation/init http=403"),
	}
	h := NewHandler(nil)
	h.SetQuotaProber(runner)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: "18"}}
	c.Request = httptest.NewRequest(http.MethodPost, "/api/admin/accounts/18/probe-quota", nil)

	h.ProbeQuota(c)

	if w.Code != http.StatusOK {
		t.Fatalf("ProbeQuota status = %d, want %d", w.Code, http.StatusOK)
	}
	var body handlerResponseBody
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	var got QuotaResult
	if err := json.Unmarshal(body.Data, &got); err != nil {
		t.Fatalf("decode quota result: %v", err)
	}
	if got.OK || got.Error != "上游拒绝访问(403)" {
		t.Fatalf("quota result = %+v", got)
	}
}

// TestProbeQuotaAllEndpointRunsActiveQuotaProbes verifies the existing
// "probe all" admin button keeps its semantics. Expected: empty body scans all
// active accounts, aggregates success/failure counts, and returns each result.
func TestProbeQuotaAllEndpointRunsActiveQuotaProbes(t *testing.T) {
	gin.SetMode(gin.TestMode)
	runner := &fakeQuotaProbeRunner{
		allResult: []*QuotaResult{
			{AccountID: 21, OK: true, Remaining: 12, Total: 25},
			{AccountID: 22, OK: false, Error: "探测超时"},
		},
	}
	h := NewHandler(nil)
	h.SetQuotaProber(runner)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/api/admin/accounts/probe-quota-all", nil)

	h.ProbeQuotaAll(c)

	if w.Code != http.StatusOK {
		t.Fatalf("ProbeQuotaAll status = %d, want %d", w.Code, http.StatusOK)
	}
	if runner.allLimit != 0 {
		t.Fatalf("ProbeAllActive limit = %d, want 0 for unlimited manual all", runner.allLimit)
	}
	var body handlerResponseBody
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	var got struct {
		Total   int            `json:"total"`
		Success int            `json:"success"`
		Failed  int            `json:"failed"`
		Results []*QuotaResult `json:"results"`
	}
	if err := json.Unmarshal(body.Data, &got); err != nil {
		t.Fatalf("decode quota-all result: %v", err)
	}
	if got.Total != 2 || got.Success != 1 || got.Failed != 1 {
		t.Fatalf("summary = %+v", got)
	}
}
