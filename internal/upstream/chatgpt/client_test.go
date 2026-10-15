package chatgpt

import "testing"

// TestDefaultClientBuildMatchesPythonImageReference verifies the browser client
// version headers stay aligned with the Python image path that is known to
// complete ChatGPT image tasks. These headers participate in sentinel/browser
// fingerprinting; stale values can let a request start but leave the async image
// task without a committed result.
func TestDefaultClientBuildMatchesPythonImageReference(t *testing.T) {
	if DefaultClientVersion != "prod-a194cd50d4416d3c0b47c740f206b12ce60f5887" {
		t.Fatalf("DefaultClientVersion = %q, want Python image reference build", DefaultClientVersion)
	}
	if DefaultClientBuildNum != "6708908" {
		t.Fatalf("DefaultClientBuildNum = %q, want 6708908", DefaultClientBuildNum)
	}
}

// TestUTLSTransportPrefersHTTP2ByDefault protects the sentinel prepare path
// used by new free accounts. The Python image reference succeeds with a
// browser-like HTTP/2 stack; forcing HTTP/1.1 currently makes
// /backend-api/sentinel/chat-requirements/prepare return upstream 403.
func TestUTLSTransportPrefersHTTP2ByDefault(t *testing.T) {
	if forceH1 {
		t.Fatal("forceH1 must be false by default")
	}
}
