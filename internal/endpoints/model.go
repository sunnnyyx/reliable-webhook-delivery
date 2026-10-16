package endpoints

import (
	"errors"
	"relayforge/internal/events"
	"relayforge/internal/netpolicy"
	"time"
)

var (
	ErrNotFound = errors.New("resource not found")
	ErrLimit    = errors.New("tenant endpoint limit reached")
	ErrOverlap  = errors.New("secret rotation overlap is still active")
)

type Endpoint struct {
	ID            string    `json:"id"`
	URL           string    `json:"url"`
	Enabled       bool      `json:"enabled"`
	Subscriptions []string  `json:"subscribed_event_types"`
	CreatedAt     time.Time `json:"created_at"`
	UpdatedAt     time.Time `json:"updated_at"`
}
type Create struct {
	URL           string   `json:"url"`
	Subscriptions []string `json:"subscribed_event_types"`
}
type Patch struct {
	URL           *string   `json:"url,omitempty"`
	Enabled       *bool     `json:"enabled,omitempty"`
	Subscriptions *[]string `json:"subscribed_event_types,omitempty"`
}

func Validate(url string, types []string) error {
	if e := netpolicy.ValidateURL(url); e != nil {
		return e
	}
	return ValidateSubscriptions(types)
}
func ValidateSubscriptions(types []string) error {
	if len(types) == 0 || len(types) > 50 {
		return errors.New("supply 1–50 subscribed_event_types")
	}
	seen := map[string]bool{}
	for _, s := range types {
		if s != "*" && !events.ValidType(s) {
			return errors.New("invalid event type")
		}
		if seen[s] {
			return errors.New("duplicate event type")
		}
		seen[s] = true
	}
	if seen["*"] && len(types) > 1 {
		return errors.New("wildcard must be the only subscription")
	}
	return nil
}
