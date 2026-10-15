package proxy

import (
	"net/url"
	"strings"
	"testing"
)

// TestURLWithPasswordEscapesProxyCredentials verifies account-style proxy
// usernames and reserved password characters round-trip through a valid URL.
// Expected: the serialized URL contains no raw spaces and parsing restores the
// exact username and password used by the proxy provider.
func TestURLWithPasswordEscapesProxyCredentials(t *testing.T) {
	const username = "account-region-US-st-California-city-Los Angeles"
	const password = "p@ss:/?# word"

	p := &Proxy{
		Scheme:   "http",
		Host:     "us.cliproxy.io",
		Port:     3010,
		Username: username,
	}

	rawURL := p.URLWithPassword(password)
	if strings.Contains(rawURL, " ") {
		t.Fatalf("proxy URL contains an unescaped space: %q", rawURL)
	}

	parsed, err := url.Parse(rawURL)
	if err != nil {
		t.Fatalf("parse proxy URL: %v", err)
	}
	if got := parsed.User.Username(); got != username {
		t.Fatalf("username = %q, want %q", got, username)
	}
	gotPassword, ok := parsed.User.Password()
	if !ok {
		t.Fatal("parsed proxy URL has no password")
	}
	if gotPassword != password {
		t.Fatalf("password = %q, want %q", gotPassword, password)
	}
	if got := parsed.Host; got != "us.cliproxy.io:3010" {
		t.Fatalf("host = %q, want %q", got, "us.cliproxy.io:3010")
	}
}
