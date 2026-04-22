package imageproxy

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"time"
)

var secret []byte

func init() {
	secret = make([]byte, 32)
	if _, err := rand.Read(secret); err != nil {
		for i := range secret {
			secret[i] = byte(i*31 + 7)
		}
	}
}

// TTL 是图片代理 URL 的默认有效期。
const TTL = 24 * time.Hour

func BuildURL(taskID string, idx int, ttl time.Duration) string {
	if ttl <= 0 {
		ttl = TTL
	}
	expMs := time.Now().Add(ttl).UnixMilli()
	sig := computeSig(taskID, idx, expMs)
	return fmt.Sprintf("/p/img/%s/%d?exp=%d&sig=%s", taskID, idx, expMs, sig)
}

func Verify(taskID string, idx int, expMs int64, sig string) bool {
	if expMs < time.Now().UnixMilli() {
		return false
	}
	want := computeSig(taskID, idx, expMs)
	return hmac.Equal([]byte(sig), []byte(want))
}

func computeSig(taskID string, idx int, expMs int64) string {
	mac := hmac.New(sha256.New, secret)
	fmt.Fprintf(mac, "%s|%d|%d", taskID, idx, expMs)
	return hex.EncodeToString(mac.Sum(nil))[:24]
}
