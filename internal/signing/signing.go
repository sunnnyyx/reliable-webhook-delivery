package signing

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"strconv"
	"strings"
	"time"
)

func DecodeSecret(encoded string) ([]byte, error) {
	raw := strings.TrimPrefix(encoded, "whsec_")
	key, e := base64.StdEncoding.Strict().DecodeString(raw)
	if e != nil || len(key) < 24 || len(key) > 64 {
		return nil, errors.New("invalid signing secret")
	}
	return key, nil
}
func Sign(id, timestamp string, body []byte, keys [][]byte) (string, error) {
	if id == "" || len(id) > 128 || strings.ContainsAny(id, ". \t\r\n") || len(keys) < 1 || len(keys) > 2 {
		return "", errors.New("invalid signing metadata")
	}
	n, e := strconv.ParseInt(timestamp, 10, 64)
	if e != nil || n < 0 || strconv.FormatInt(n, 10) != timestamp {
		return "", errors.New("invalid signing timestamp")
	}
	sigs := make([]string, 0, len(keys))
	for _, key := range keys {
		if len(key) < 24 || len(key) > 64 {
			return "", errors.New("invalid signing key")
		}
		mac := hmac.New(sha256.New, key)
		_, _ = mac.Write([]byte(id + "." + timestamp + "."))
		_, _ = mac.Write(body)
		sigs = append(sigs, "v1,"+base64.StdEncoding.EncodeToString(mac.Sum(nil)))
	}
	return strings.Join(sigs, " "), nil
}
func Verify(id, timestamp, header string, body []byte, keys [][]byte, now time.Time, tolerance time.Duration) error {
	bad := errors.New("invalid webhook signature")
	if len(header) > 1024 || tolerance <= 0 {
		return bad
	}
	seconds, e := strconv.ParseInt(timestamp, 10, 64)
	if e != nil || seconds < 0 || strconv.FormatInt(seconds, 10) != timestamp {
		return bad
	}
	stamp := time.Unix(seconds, 0)
	if stamp.Before(now.Add(-tolerance)) || stamp.After(now.Add(tolerance)) {
		return bad
	}
	expected, e := Sign(id, timestamp, body, keys)
	if e != nil {
		return bad
	}
	valid := false
	for _, want := range strings.Fields(expected) {
		wantBytes, _ := base64.StdEncoding.DecodeString(strings.TrimPrefix(want, "v1,"))
		for _, item := range strings.Fields(header) {
			parts := strings.SplitN(item, ",", 2)
			if len(parts) != 2 || parts[0] != "v1" {
				continue
			}
			got, e := base64.StdEncoding.Strict().DecodeString(parts[1])
			if e == nil && hmac.Equal(wantBytes, got) {
				valid = true
			}
		}
	}
	if !valid {
		return bad
	}
	return nil
}
