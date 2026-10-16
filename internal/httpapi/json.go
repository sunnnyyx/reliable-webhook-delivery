package httpapi

import "relayforge/internal/jsonutil"

func validateJSON(b []byte) error { return jsonutil.Validate(b) }
