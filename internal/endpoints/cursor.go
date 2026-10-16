package endpoints

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"relayforge/internal/security"
	"time"
)

type Cursor struct {
	Version int       `json:"v"`
	Tenant  string    `json:"tenant"`
	Time    time.Time `json:"time"`
	ID      string    `json:"id"`
}

func EncodeCursor(tenant string, e Endpoint) string {
	b, _ := json.Marshal(Cursor{1, tenant, e.CreatedAt, e.ID})
	return base64.RawURLEncoding.EncodeToString(b)
}
func DecodeCursor(raw, tenant string) (*Cursor, error) {
	bad := errors.New("invalid cursor")
	if len(raw) > 1024 {
		return nil, bad
	}
	b, e := base64.RawURLEncoding.Strict().DecodeString(raw)
	if e != nil {
		return nil, bad
	}
	var c Cursor
	d := json.NewDecoder(bytes.NewReader(b))
	d.DisallowUnknownFields()
	if e = d.Decode(&c); e != nil || c.Version != 1 || c.Tenant != tenant || !security.ValidID(c.ID) || c.Time.IsZero() || c.Time.Year() < 1 || c.Time.Year() > 9999 {
		return nil, bad
	}
	var extra any
	if d.Decode(&extra) != io.EOF {
		return nil, bad
	}
	return &c, nil
}
