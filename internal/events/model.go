package events

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"regexp"
	"relayforge/internal/jsonutil"
	"time"
)

const MaxInputBytes = 256 << 10

var ErrIdempotencyConflict = errors.New("idempotency key already used with different request bytes")
var eventType = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)

func ValidType(s string) bool { return eventType.MatchString(s) }
func ValidateKey(key string) error {
	if len(key) < 1 || len(key) > 128 {
		return errors.New("Idempotency-Key must contain 1–128 printable ASCII characters")
	}
	for _, b := range []byte(key) {
		if b < 32 || b > 126 {
			return errors.New("Idempotency-Key must contain 1–128 printable ASCII characters")
		}
	}
	return nil
}

type Input struct {
	Type string
	Data json.RawMessage
}

func Parse(raw []byte) (Input, error) {
	bad := errors.New("event requires exactly type and data, with a valid non-wildcard event type")
	if len(raw) > MaxInputBytes {
		return Input{}, errors.New("event input exceeds 256 KiB")
	}
	if e := jsonutil.Validate(raw); e != nil {
		return Input{}, bad
	}
	var fields map[string]json.RawMessage
	if e := json.Unmarshal(raw, &fields); e != nil || len(fields) != 2 {
		return Input{}, bad
	}
	var input Input
	if e := json.Unmarshal(fields["type"], &input.Type); e != nil || !ValidType(input.Type) {
		return input, bad
	}
	var ok bool
	input.Data, ok = fields["data"]
	if !ok {
		return input, bad
	}
	return input, nil
}

// Envelope builds bytes once, retaining the validated data JSON verbatim.
func Envelope(id string, created time.Time, input Input) []byte {
	idJSON, _ := json.Marshal(id)
	typeJSON, _ := json.Marshal(input.Type)
	timeJSON, _ := json.Marshal(created.UTC().Format(time.RFC3339Nano))
	b := append([]byte(`{"id":`), idJSON...)
	b = append(b, []byte(`,"type":`)...)
	b = append(b, typeJSON...)
	b = append(b, []byte(`,"created_at":`)...)
	b = append(b, timeJSON...)
	b = append(b, []byte(`,"data":`)...)
	b = append(b, input.Data...)
	return append(b, '}')
}
func Hash(raw []byte) []byte { h := sha256.Sum256(raw); return h[:] }

type Accepted struct {
	EventID  string
	Status   int
	Body     []byte
	Existing bool
}
type Acceptance struct {
	EventID       string    `json:"event_id"`
	DeliveryCount int       `json:"delivery_count"`
	AcceptedAt    time.Time `json:"accepted_at"`
}
type Event struct {
	ID         string    `json:"id"`
	Type       string    `json:"type"`
	CreatedAt  time.Time `json:"created_at"`
	ExpiresAt  time.Time `json:"expires_at"`
	BodySHA256 string    `json:"body_sha256"`
}
type Detail struct {
	TraceID string `json:"trace_id,omitempty"`

	Event           Event           `json:"event"`
	Payload         json.RawMessage `json:"payload"`
	DeliverySummary map[string]int  `json:"delivery_summary"`
}
