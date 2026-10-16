package postgres

import (
	"context"
	"errors"
	"relayforge/internal/endpoints"
	"testing"
)

func TestCreateValidatesBeforeDatabase(t *testing.T) {
	s := &Store{}
	_, _, e := s.CreateEndpoint(context.Background(), "tenant", endpoints.Create{URL: "http://localhost", Subscriptions: []string{"*"}})
	var validation ValidationError
	if !errors.As(e, &validation) {
		t.Fatal("invalid input reached database")
	}
}
