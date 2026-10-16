package config

import (
	"encoding/base64"
	"testing"
)

func TestEncryptionKey(t *testing.T) {
	for _, key := range []string{"not-base64", base64.StdEncoding.EncodeToString([]byte("short"))} {
		_, e := Load(func(k string) string {
			switch k {
			case "DATABASE_URL":
				return "postgres://u:p@db/relayforge"
			case "ENCRYPTION_KEY":
				return key
			}
			return ""
		})
		if e == nil {
			t.Fatal("invalid key accepted")
		}
	}
	c, e := Load(func(k string) string {
		switch k {
		case "DATABASE_URL":
			return "postgres://u:p@db/relayforge"
		case "ENCRYPTION_KEY":
			return base64.StdEncoding.EncodeToString(make([]byte, 32))
		}
		return ""
	})
	if e != nil || len(c.EncryptionKey) != 32 {
		t.Fatal("valid key rejected")
	}
}
