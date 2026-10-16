package delivery

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"time"
)

func ValidStatus(s string) bool {
	switch s {
	case "pending", "in_flight", "retry_scheduled", "delivered", "dead_letter", "cancelled":
		return true
	}
	return false
}

type Delivery struct {
	ID                   string     `json:"id"`
	EventID              string     `json:"event_id"`
	EndpointID           string     `json:"endpoint_id"`
	URLSnapshot          string     `json:"url_snapshot"`
	Status               string     `json:"status"`
	CycleNumber          int        `json:"cycle_number"`
	CycleAttemptCount    int        `json:"cycle_attempt_count"`
	LifetimeAttemptCount int        `json:"lifetime_attempt_count"`
	NextAttemptAt        time.Time  `json:"next_attempt_at"`
	CycleStartedAt       time.Time  `json:"cycle_started_at"`
	LeaseExpiresAt       *time.Time `json:"lease_expires_at,omitempty"`
	LastErrorCode        *string    `json:"last_error_code,omitempty"`
	LastHTTPStatus       *int       `json:"last_http_status,omitempty"`
	DeliveredAt          *time.Time `json:"delivered_at,omitempty"`
	CreatedAt            time.Time  `json:"created_at"`
	UpdatedAt            time.Time  `json:"updated_at"`
}
type Filters struct {
	Status     string `json:"status"`
	EndpointID string `json:"endpoint_id"`
	EventID    string `json:"event_id"`
}

func (f Filters) Fingerprint() string {
	b, _ := json.Marshal(f)
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}
