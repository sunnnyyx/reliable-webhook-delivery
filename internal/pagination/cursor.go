package pagination

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"relayforge/internal/jsonutil"
	"relayforge/internal/security"
	"time"
)

type Cursor struct {
	Version  int       `json:"v"`
	Tenant   string    `json:"tenant"`
	Resource string    `json:"resource"`
	Filter   string    `json:"filter"`
	Time     time.Time `json:"time"`
	ID       string    `json:"id"`
}

func Encode(tenant, resource, filter string, at time.Time, id string) string {
	b, _ := json.Marshal(Cursor{1, tenant, resource, filter, at, id})
	return base64.RawURLEncoding.EncodeToString(b)
}
func Decode(raw, tenant, resource, filter string) (*Cursor, error) {
	bad := errors.New("invalid cursor")
	if len(raw) > 1024 {
		return nil, bad
	}
	b, e := base64.RawURLEncoding.Strict().DecodeString(raw)
	if e != nil || jsonutil.Validate(b) != nil {
		return nil, bad
	}
	d := json.NewDecoder(bytes.NewReader(b))
	d.DisallowUnknownFields()
	var c Cursor
	if e = d.Decode(&c); e != nil || c.Version != 1 || c.Tenant != tenant || c.Resource != resource || c.Filter != filter || !security.ValidID(c.ID) || c.Time.IsZero() || c.Time.Year() < 1 || c.Time.Year() > 9999 {
		return nil, bad
	}
	var extra any
	if d.Decode(&extra) != io.EOF {
		return nil, bad
	}
	return &c, nil
}
