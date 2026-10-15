package chatgpt

import (
	"context"
	"errors"
	"io"
	"testing"
)

type failingSSEReadCloser struct {
	err error
}

func (reader failingSSEReadCloser) Read([]byte) (int, error) { return 0, reader.err }
func (failingSSEReadCloser) Close() error                    { return nil }

type timeoutSSEError struct{}

func (timeoutSSEError) Error() string   { return "secret timeout detail" }
func (timeoutSSEError) Timeout() bool   { return true }
func (timeoutSSEError) Temporary() bool { return true }

// TestParseSSEClassifiesReadFailuresAtTheTransportBoundary verifies the event
// producer, rather than downstream image logic, owns I/O failure recognition.
// Expected: each raw error becomes one fixed secret-safe class.
func TestParseSSEClassifiesReadFailuresAtTheTransportBoundary(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want SSEReadFailureClass
	}{
		{name: "unexpected_eof", err: io.ErrUnexpectedEOF, want: SSEReadFailureUnexpectedEOF},
		{name: "read_timeout", err: timeoutSSEError{}, want: SSEReadFailureTimeout},
		{name: "context_cancelled", err: context.Canceled, want: SSEReadFailureContextCancelled},
		{name: "other_io", err: errors.New("secret other detail"), want: SSEReadFailureOtherIO},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			stream := make(chan SSEEvent, 1)
			parseSSE(failingSSEReadCloser{err: testCase.err}, stream, 0)
			event, ok := <-stream
			if !ok {
				t.Fatal("parseSSE closed without a failure event")
			}
			if event.ReadFailure != testCase.want {
				t.Fatalf("ReadFailure = %q, want %q", event.ReadFailure, testCase.want)
			}
			if event.Err == nil {
				t.Fatal("parseSSE dropped the internal error")
			}
			if _, open := <-stream; open {
				t.Fatal("parseSSE left the stream open after a read failure")
			}
		})
	}
}
