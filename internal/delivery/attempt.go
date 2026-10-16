package delivery

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"relayforge/internal/jsonutil"
	"time"
)

type Attempt struct {
	ID         string     `json:"id"`
	Cycle      int        `json:"cycle_number"`
	Number     int        `json:"attempt_number"`
	WorkerID   string     `json:"worker_id"`
	StartedAt  time.Time  `json:"started_at"`
	FinishedAt *time.Time `json:"finished_at,omitempty"`
	Outcome    string     `json:"outcome"`
	HTTPStatus *int       `json:"http_status,omitempty"`
	LatencyMS  *int64     `json:"latency_ms,omitempty"`
	ErrorCode  *string    `json:"error_code,omitempty"`
}
type AttemptCursor struct {
	Version  int    `json:"v"`
	Tenant   string `json:"tenant"`
	Delivery string `json:"delivery"`
	Cycle    int    `json:"cycle"`
	Attempt  int    `json:"attempt"`
}

func EncodeAttemptCursor(tenant, delivery string, a Attempt) string {
	b, _ := json.Marshal(AttemptCursor{1, tenant, delivery, a.Cycle, a.Number})
	return base64.RawURLEncoding.EncodeToString(b)
}
func DecodeAttemptCursor(raw, tenant, delivery string) (*AttemptCursor, error) {
	bad := errors.New("invalid attempt cursor")
	if len(raw) > 1024 {
		return nil, bad
	}
	b, e := base64.RawURLEncoding.Strict().DecodeString(raw)
	if e != nil || jsonutil.Validate(b) != nil {
		return nil, bad
	}
	var c AttemptCursor
	d := json.NewDecoder(bytes.NewReader(b))
	d.DisallowUnknownFields()
	if e = d.Decode(&c); e != nil || c.Version != 1 || c.Tenant != tenant || c.Delivery != delivery || c.Cycle < 1 || c.Attempt < 1 || c.Cycle > 2147483647 || c.Attempt > 2147483647 {
		return nil, bad
	}
	return &c, nil
}
