package chatgpt

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
)

type assetEgressRoundTripFunc func(*http.Request) (*http.Response, error)

func (f assetEgressRoundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func assetEgressResponse(status int, contentType, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Header:     http.Header{"Content-Type": []string{contentType}},
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}

// TestNewSeparatesControlAndAssetTransports verifies the production constructor
// assigns two strict network responsibilities. Expected: every authenticated
// ChatGPT request uses proxied uTLS, while public signed assets use standard
// verified TLS without account cookies or an environment proxy.
func TestNewSeparatesControlAndAssetTransports(t *testing.T) {
	proxied, err := New(Options{
		AuthToken: "at",
		DeviceID:  "device",
		ProxyURL:  "http://proxy.example:8080",
	})
	if err != nil {
		t.Fatalf("New() proxied error = %v", err)
	}
	if proxied.hc == proxied.assetHC || proxied.hc == proxied.bearerAssetHC ||
		proxied.assetHC == proxied.bearerAssetHC {
		t.Fatal("control, public asset, and bearer asset clients must be distinct")
	}
	controlTransport, ok := proxied.hc.Transport.(*utlsRoundTripper)
	if !ok || controlTransport.proxyURL == nil {
		t.Fatal("control transport must retain the configured proxy")
	}
	assetTransport, ok := proxied.assetHC.Transport.(*http.Transport)
	if !ok || assetTransport.Proxy != nil {
		t.Fatal("public asset transport must be standard direct TLS")
	}
	if proxied.assetHC.Jar != nil {
		t.Fatal("public asset client must not receive account cookies")
	}
	bearerAssetTransport, ok := proxied.bearerAssetHC.Transport.(*utlsRoundTripper)
	if !ok || bearerAssetTransport.proxyURL != nil {
		t.Fatal("bearer asset transport must use direct browser TLS")
	}
	if proxied.bearerAssetHC.Jar != nil {
		t.Fatal("bearer asset client must not receive account cookies")
	}

	direct, err := New(Options{AuthToken: "at", DeviceID: "device"})
	if err != nil {
		t.Fatalf("New() direct error = %v", err)
	}
	if direct.hc == direct.assetHC || direct.hc == direct.bearerAssetHC ||
		direct.assetHC == direct.bearerAssetHC {
		t.Fatal("asset clients must not reuse the ChatGPT control client")
	}
}

// TestUploadFileSendsOnlyRawReferenceBytesThroughDirectEgress verifies the
// three-stage reference upload follows the intended control/data trajectory.
// The create-file and uploaded-confirmation requests must use the account's
// control client, while the potentially large Azure PUT must use the direct
// asset client and preserve the exact reference bytes.
func TestUploadFileSendsOnlyRawReferenceBytesThroughDirectEgress(t *testing.T) {
	var controlCalls atomic.Int32
	controlClient := &http.Client{Transport: assetEgressRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		controlCalls.Add(1)
		switch req.URL.Path {
		case "/backend-api/files":
			return assetEgressResponse(http.StatusOK, "application/json", `{"file_id":"file_1","upload_url":"https://blob.example/upload"}`), nil
		case "/backend-api/files/file_1/uploaded":
			return assetEgressResponse(http.StatusOK, "application/json", `{"status":"success","download_url":"https://chatgpt.example/file_1"}`), nil
		default:
			return nil, errors.New("unexpected control request: " + req.URL.String())
		}
	})}

	reference := []byte("\x89PNG\r\n\x1a\nreference-image-bytes")
	var assetCalls atomic.Int32
	assetClient := &http.Client{Transport: assetEgressRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		assetCalls.Add(1)
		if req.Method != http.MethodPut || req.URL.String() != "https://blob.example/upload" {
			return nil, errors.New("unexpected asset request: " + req.Method + " " + req.URL.String())
		}
		body, err := io.ReadAll(req.Body)
		if err != nil {
			return nil, err
		}
		if !bytes.Equal(body, reference) {
			return nil, errors.New("asset request changed reference bytes")
		}
		return assetEgressResponse(http.StatusCreated, "application/json", ""), nil
	})}

	client := &Client{
		opts: Options{
			BaseURL: "https://chatgpt.example", AuthToken: "at", DeviceID: "device",
			TrafficMeter: &TrafficMeter{},
		},
		hc:      controlClient,
		assetHC: assetClient,
	}
	if _, err := client.UploadFile(context.Background(), reference, "reference.png"); err != nil {
		t.Fatalf("UploadFile() error = %v", err)
	}
	if got := controlCalls.Load(); got != 2 {
		t.Fatalf("control calls = %d, want create + uploaded confirmation", got)
	}
	if got := assetCalls.Load(); got != 1 {
		t.Fatalf("asset calls = %d, want one raw PUT", got)
	}
	if got := client.opts.TrafficMeter.Snapshot().DirectAssetWrittenBytes; got != int64(len(reference)) {
		t.Fatalf("direct asset upload bytes = %d, want %d", got, len(reference))
	}
}

// TestFetchImageUsesDirectEgress verifies generated image bytes never touch
// the account's paid control proxy. The signed URL must be fetched exclusively
// by the direct asset client while retaining the expected image bytes and
// content type returned to the gateway.
func TestFetchImageUsesDirectEgress(t *testing.T) {
	var controlCalls atomic.Int32
	controlClient := &http.Client{Transport: assetEgressRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		controlCalls.Add(1)
		return nil, errors.New("generated image reached control client: " + req.URL.String())
	})}

	want := []byte("generated-image-bytes")
	var assetCalls atomic.Int32
	assetClient := &http.Client{Transport: assetEgressRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		assetCalls.Add(1)
		if req.Method != http.MethodGet || req.URL.String() != "https://cdn.example/image.png" {
			return nil, errors.New("unexpected asset request: " + req.Method + " " + req.URL.String())
		}
		if req.Header.Get("Authorization") != "" || req.Header.Get("Oai-Device-Id") != "" || req.Header.Get("Cookie") != "" {
			return nil, errors.New("public signed asset received account identity headers")
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"image/png"}},
			Body:       io.NopCloser(bytes.NewReader(want)),
		}, nil
	})}

	client := &Client{
		opts: Options{
			BaseURL: "https://chatgpt.example", AuthToken: "at", DeviceID: "device",
			TrafficMeter: &TrafficMeter{},
		},
		hc:      controlClient,
		assetHC: assetClient,
	}
	got, contentType, err := client.FetchImage(context.Background(), "https://cdn.example/image.png", 1024)
	if err != nil {
		t.Fatalf("FetchImage() error = %v", err)
	}
	if !bytes.Equal(got, want) || contentType != "image/png" {
		t.Fatalf("FetchImage() = (%q, %q), want (%q, image/png)", got, contentType, want)
	}
	if got := controlCalls.Load(); got != 0 {
		t.Fatalf("control calls = %d, want zero for image bytes", got)
	}
	if got := assetCalls.Load(); got != 1 {
		t.Fatalf("asset calls = %d, want one direct image fetch", got)
	}
	if got := client.opts.TrafficMeter.Snapshot().DirectAssetReadBytes; got != int64(len(want)) {
		t.Fatalf("direct asset bytes = %d, want %d", got, len(want))
	}
}

// TestFetchImageUsesDirectBearerEgressForAuthenticatedResources verifies an
// internal estuary image keeps the minimum required Bearer credential while
// its large body bypasses both the paid control proxy and the public CDN
// client. Expected: no Cookie or Oai identity header crosses the direct path.
func TestFetchImageUsesDirectBearerEgressForAuthenticatedResources(t *testing.T) {
	blocked := func(owner string) *http.Client {
		return &http.Client{Transport: assetEgressRoundTripFunc(func(req *http.Request) (*http.Response, error) {
			return nil, errors.New(owner + " unexpectedly received " + req.URL.String())
		})}
	}
	bearerCalls := atomic.Int32{}
	bearerClient := &http.Client{Transport: assetEgressRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		bearerCalls.Add(1)
		if req.Header.Get("Authorization") != "Bearer at" {
			return nil, errors.New("authenticated image request omitted Bearer token")
		}
		if req.Header.Get("Cookie") != "" || req.Header.Get("Oai-Device-Id") != "" ||
			req.Header.Get("Oai-Session-Id") != "" {
			return nil, errors.New("direct bearer request leaked account identity headers")
		}
		return assetEgressResponse(http.StatusOK, "image/png", "internal-image"), nil
	})}
	client := &Client{
		opts: Options{
			BaseURL: "https://chatgpt.example", AuthToken: "at", DeviceID: "device",
			TrafficMeter: &TrafficMeter{},
		},
		hc:            blocked("control client"),
		assetHC:       blocked("public asset client"),
		bearerAssetHC: bearerClient,
		profile:       DefaultBrowserProfile(),
	}

	got, contentType, err := client.FetchImage(
		context.Background(),
		"https://chatgpt.example/backend-api/estuary/content?id=1",
		1024,
	)
	if err != nil {
		t.Fatalf("FetchImage() error = %v", err)
	}
	if string(got) != "internal-image" || contentType != "image/png" || bearerCalls.Load() != 1 {
		t.Fatalf("FetchImage() = (%q, %q), bearer calls=%d", got, contentType, bearerCalls.Load())
	}
	if measured := client.opts.TrafficMeter.Snapshot().DirectAssetReadBytes; measured != int64(len(got)) {
		t.Fatalf("direct bearer image bytes = %d, want %d", measured, len(got))
	}
}
