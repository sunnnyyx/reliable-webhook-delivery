package config

import (
	"testing"
)

func TestWorkerPolicyConfiguration(t *testing.T) {
	for _, tc := range []struct{ key, value string }{{"DELIVERY_LEASE", "12s"}, {"OUTBOUND_TIMEOUT", "90s"}, {"DEV_RECEIVER_URL", "http://localhost:8090/other"}} {
		_, e := Load(func(k string) string {
			if k == "DATABASE_URL" {
				return "postgres://u:p@db/relayforge"
			}
			if k == tc.key {
				return tc.value
			}
			return ""
		})
		if e == nil {
			t.Fatal("invalid worker configuration accepted")
		}
	}
	_, e := Load(func(k string) string {
		switch k {
		case "DATABASE_URL":
			return "postgres://u:p@db/relayforge?sslmode=verify-full"
		case "PROFILE":
			return "production"
		case "DEV_RECEIVER_URL":
			return "http://localhost:8090/webhooks"
		}
		return ""
	})
	if e == nil {
		t.Fatal("production dev URL accepted")
	}
}
