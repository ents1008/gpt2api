package chatgpt

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"
)

type storedCookie struct {
	Name   string `json:"name"`
	Value  string `json:"value"`
	Domain string `json:"domain"`
	Path   string `json:"path"`
}

// NormalizeCookieJSON accepts the two formats produced by the registration
// stack (a JSON array or an HTTP Cookie request header) and emits one canonical
// deterministic JSON representation for encrypted account storage.
func NormalizeCookieJSON(raw, base string) (string, error) {
	records, err := parseStoredCookies(raw, base)
	if err != nil {
		return "", err
	}
	data, err := json.Marshal(records)
	if err != nil {
		return "", fmt.Errorf("marshal cookies: %w", err)
	}
	return string(data), nil
}

func parseStoredCookies(raw, base string) ([]storedCookie, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return []storedCookie{}, nil
	}
	baseURL, err := url.Parse(base)
	if err != nil || baseURL.Hostname() == "" {
		return nil, fmt.Errorf("invalid cookie base URL %q", base)
	}
	host := strings.ToLower(baseURL.Hostname())

	var records []storedCookie
	if strings.HasPrefix(trimmed, "[") {
		if err := json.Unmarshal([]byte(trimmed), &records); err != nil {
			return nil, fmt.Errorf("parse cookie JSON: %w", err)
		}
	} else {
		req := &http.Request{Header: make(http.Header)}
		req.Header.Set("Cookie", trimmed)
		parsed := req.Cookies()
		if len(parsed) == 0 {
			return nil, errors.New("cookie header contains no valid cookies")
		}
		records = make([]storedCookie, 0, len(parsed))
		for _, cookie := range parsed {
			records = append(records, storedCookie{
				Name: cookie.Name, Value: cookie.Value, Domain: host, Path: "/",
			})
		}
	}

	normalized := make([]storedCookie, 0, len(records))
	seen := make(map[string]int, len(records))
	droppedForeignDomain := false
	for _, record := range records {
		record.Name = strings.TrimSpace(record.Name)
		if record.Name == "" || record.Value == "" {
			continue
		}
		record.Domain = strings.ToLower(strings.TrimPrefix(strings.TrimSpace(record.Domain), "."))
		if record.Domain == "" {
			record.Domain = host
		}
		if record.Domain != host && !strings.HasSuffix(host, "."+record.Domain) {
			// Registration/login clients can export auth.openai.com cookies in
			// the same snapshot. They are not valid for the ChatGPT request jar
			// and must not make the account unusable.
			droppedForeignDomain = true
			continue
		}
		record.Path = strings.TrimSpace(record.Path)
		if record.Path == "" {
			record.Path = "/"
		}
		if !strings.HasPrefix(record.Path, "/") {
			return nil, fmt.Errorf("cookie %q has invalid path", record.Name)
		}
		key := record.Name + "\x00" + record.Domain + "\x00" + record.Path
		if idx, ok := seen[key]; ok {
			normalized[idx] = record
			continue
		}
		seen[key] = len(normalized)
		normalized = append(normalized, record)
	}
	if len(normalized) == 0 {
		if droppedForeignDomain {
			return []storedCookie{}, nil
		}
		return nil, errors.New("cookies contain no usable entries")
	}
	sort.Slice(normalized, func(i, j int) bool {
		if normalized[i].Name != normalized[j].Name {
			return normalized[i].Name < normalized[j].Name
		}
		if normalized[i].Domain != normalized[j].Domain {
			return normalized[i].Domain < normalized[j].Domain
		}
		return normalized[i].Path < normalized[j].Path
	})
	return normalized, nil
}

func loadCookies(jar http.CookieJar, base, raw string) error {
	records, err := parseStoredCookies(raw, base)
	if err != nil {
		return err
	}
	u, err := url.Parse(base)
	if err != nil {
		return err
	}
	cookies := make([]*http.Cookie, 0, len(records))
	for _, record := range records {
		cookie := &http.Cookie{
			Name: record.Name, Value: record.Value, Domain: record.Domain, Path: record.Path,
		}
		if cookie.Valid() != nil {
			continue
		}
		cookies = append(cookies, cookie)
	}
	jar.SetCookies(u, cookies)
	return nil
}

// LoadCookies seeds an external HTTP cookie jar with the canonical account
// snapshot. It is used by quota probing, which owns a lighter HTTP client than
// the full ChatGPT client.
func LoadCookies(jar http.CookieJar, base, raw string) error {
	return loadCookies(jar, base, raw)
}

func cookieJarJSON(jar http.CookieJar, base string) (string, error) {
	if jar == nil {
		return "", errors.New("cookie jar is nil")
	}
	u, err := url.Parse(base)
	if err != nil || u.Hostname() == "" {
		return "", fmt.Errorf("invalid cookie base URL %q", base)
	}
	records := make([]storedCookie, 0)
	for _, cookie := range jar.Cookies(u) {
		if cookie.Name == "" || cookie.Value == "" {
			continue
		}
		records = append(records, storedCookie{
			Name: cookie.Name, Value: cookie.Value, Domain: strings.ToLower(u.Hostname()), Path: "/",
		})
	}
	sort.Slice(records, func(i, j int) bool {
		if records[i].Name != records[j].Name {
			return records[i].Name < records[j].Name
		}
		if records[i].Domain != records[j].Domain {
			return records[i].Domain < records[j].Domain
		}
		return records[i].Path < records[j].Path
	})
	data, err := json.Marshal(records)
	if err != nil {
		return "", fmt.Errorf("marshal cookie jar: %w", err)
	}
	return string(data), nil
}

// CookieJarJSON exports an external HTTP client's ChatGPT cookie jar using the
// same canonical encrypted-storage format as Client.CookieJSON.
func CookieJarJSON(jar http.CookieJar, base string) (string, error) {
	return cookieJarJSON(jar, base)
}
