package chatgpt

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"
)

const (
	DefaultUserAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/143.0.0.0 Safari/537.36 Edg/143.0.0.0"

	defaultAcceptLanguage         = "zh-CN,zh;q=0.9,en;q=0.8,en-GB;q=0.7,en-US;q=0.6"
	defaultSecChUA                = `"Microsoft Edge";v="143", "Chromium";v="143", "Not A(Brand";v="24"`
	defaultSecChUAFullVersion     = `"143.0.3650.96"`
	defaultSecChUAFullVersionList = `"Microsoft Edge";v="143.0.3650.96", "Chromium";v="143.0.7499.147", "Not A(Brand";v="24.0.0.0"`
	defaultSecChUAPlatformVersion = `"19.0.0"`
	defaultBrowserProfileVersion  = 1
	maxBrowserProfileJSONBytes    = 8192
)

var (
	browserProfileLanguage = regexp.MustCompile(`^[A-Za-z]{2,3}(?:-[A-Za-z0-9]{2,8})*$`)
	browserProfileHeaders  = map[string]struct{}{
		"Accept-Language":             {},
		"Sec-Ch-Ua":                   {},
		"Sec-Ch-Ua-Arch":              {},
		"Sec-Ch-Ua-Bitness":           {},
		"Sec-Ch-Ua-Full-Version":      {},
		"Sec-Ch-Ua-Full-Version-List": {},
		"Sec-Ch-Ua-Mobile":            {},
		"Sec-Ch-Ua-Model":             {},
		"Sec-Ch-Ua-Platform":          {},
		"Sec-Ch-Ua-Platform-Version":  {},
		"User-Agent":                  {},
	}
	requiredBrowserProfileHeaders = []string{
		"Accept-Language",
		"Sec-Ch-Ua",
		"Sec-Ch-Ua-Mobile",
		"Sec-Ch-Ua-Platform",
		"User-Agent",
	}
)

// BrowserProfile is the credential-free browser identity captured by the
// registration browser and replayed by every account-scoped Web request.
type BrowserProfile struct {
	Version  int               `json:"version"`
	Headers  map[string]string `json:"headers"`
	Language string            `json:"language"`
	Timezone string            `json:"timezone"`
}

func DefaultBrowserProfile() BrowserProfile {
	return BrowserProfile{
		Version: defaultBrowserProfileVersion,
		Headers: map[string]string{
			"User-Agent":                  DefaultUserAgent,
			"Accept-Language":             defaultAcceptLanguage,
			"Sec-Ch-Ua":                   defaultSecChUA,
			"Sec-Ch-Ua-Arch":              `"x86"`,
			"Sec-Ch-Ua-Bitness":           `"64"`,
			"Sec-Ch-Ua-Full-Version":      defaultSecChUAFullVersion,
			"Sec-Ch-Ua-Full-Version-List": defaultSecChUAFullVersionList,
			"Sec-Ch-Ua-Mobile":            "?0",
			"Sec-Ch-Ua-Model":             `""`,
			"Sec-Ch-Ua-Platform":          `"Windows"`,
			"Sec-Ch-Ua-Platform-Version":  defaultSecChUAPlatformVersion,
		},
		Language: DefaultLanguage,
		Timezone: "Asia/Shanghai",
	}
}

func DefaultBrowserProfileJSON() string {
	raw, err := json.Marshal(DefaultBrowserProfile())
	if err != nil {
		panic("marshal default browser profile: " + err.Error())
	}
	return string(raw)
}

func ParseBrowserProfileJSON(raw string) (BrowserProfile, string, error) {
	var profile BrowserProfile
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return profile, "", errors.New("browser profile is required")
	}
	if len(raw) > maxBrowserProfileJSONBytes {
		return profile, "", errors.New("browser profile is too large")
	}
	decoder := json.NewDecoder(bytes.NewBufferString(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&profile); err != nil {
		return BrowserProfile{}, "", fmt.Errorf("decode browser profile: %w", err)
	}
	if err := ensureJSONEOF(decoder); err != nil {
		return BrowserProfile{}, "", err
	}
	if err := profile.Validate(); err != nil {
		return BrowserProfile{}, "", err
	}
	canonical, err := json.Marshal(profile)
	if err != nil {
		return BrowserProfile{}, "", fmt.Errorf("marshal browser profile: %w", err)
	}
	return profile, string(canonical), nil
}

func ensureJSONEOF(decoder *json.Decoder) error {
	var extra any
	if err := decoder.Decode(&extra); errors.Is(err, io.EOF) {
		return nil
	} else if err != nil {
		return fmt.Errorf("decode browser profile trailer: %w", err)
	}
	return errors.New("browser profile contains multiple JSON values")
}

func (p BrowserProfile) Validate() error {
	if p.Version != defaultBrowserProfileVersion {
		return fmt.Errorf("unsupported browser profile version %d", p.Version)
	}
	if !browserProfileLanguage.MatchString(p.Language) {
		return errors.New("browser profile has an invalid language")
	}
	if len(p.Timezone) == 0 || len(p.Timezone) > 64 || hasUnsafeHeaderBytes(p.Timezone) {
		return errors.New("browser profile has an invalid timezone")
	}
	if _, err := time.LoadLocation(p.Timezone); err != nil {
		return fmt.Errorf("browser profile has an unknown timezone: %w", err)
	}
	for name, value := range p.Headers {
		canonical := http.CanonicalHeaderKey(name)
		if canonical != name {
			return fmt.Errorf("browser profile header %q is not canonical", name)
		}
		if _, ok := browserProfileHeaders[name]; !ok {
			return fmt.Errorf("browser profile contains forbidden header %q", name)
		}
		if value = strings.TrimSpace(value); value == "" || len(value) > 1024 || hasUnsafeHeaderBytes(value) {
			return fmt.Errorf("browser profile header %q has an invalid value", name)
		}
	}
	for _, name := range requiredBrowserProfileHeaders {
		if strings.TrimSpace(p.Headers[name]) == "" {
			return fmt.Errorf("browser profile is missing header %q", name)
		}
	}
	return nil
}

func hasUnsafeHeaderBytes(value string) bool {
	return strings.ContainsAny(value, "\r\n\x00")
}

func (p BrowserProfile) TimezoneOffsetMinutes(at time.Time) int {
	location, err := time.LoadLocation(p.Timezone)
	if err != nil {
		return 0
	}
	_, secondsEastOfUTC := at.In(location).Zone()
	return -secondsEastOfUTC / 60
}

func (p BrowserProfile) UserAgent() string {
	return p.Headers["User-Agent"]
}

func ApplyBrowserFingerprintHeaders(header http.Header, profile BrowserProfile) {
	for name := range browserProfileHeaders {
		header.Del(name)
	}
	for name, value := range profile.Headers {
		header.Set(name, value)
	}
}

func ApplyDefaultBrowserFingerprintHeaders(header http.Header) {
	ApplyBrowserFingerprintHeaders(header, DefaultBrowserProfile())
}
