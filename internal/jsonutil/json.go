package jsonutil

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"unicode/utf8"
)

// Walk tokens to reject duplicate keys before typed decoding; encoding/json otherwise
// silently takes the last value. A depth limit bounds recursive validation work.
func Validate(b []byte) error {
	bad := errors.New("invalid JSON")
	if !utf8.Valid(b) {
		return bad
	}
	d := json.NewDecoder(bytes.NewReader(b))
	d.UseNumber()
	var value func(int) error
	value = func(depth int) error {
		if depth > 64 {
			return bad
		}
		t, e := d.Token()
		if e != nil {
			return bad
		}
		if delim, ok := t.(json.Delim); ok {
			switch delim {
			case '{':
				seen := map[string]bool{}
				for d.More() {
					k, e := d.Token()
					if e != nil {
						return bad
					}
					s, ok := k.(string)
					if !ok || seen[s] {
						return bad
					}
					seen[s] = true
					if e = value(depth + 1); e != nil {
						return e
					}
				}
				end, e := d.Token()
				if e != nil || end != json.Delim('}') {
					return bad
				}
			case '[':
				for d.More() {
					if e = value(depth + 1); e != nil {
						return e
					}
				}
				end, e := d.Token()
				if e != nil || end != json.Delim(']') {
					return bad
				}
			default:
				return bad
			}
		}
		return nil
	}
	if len(bytes.TrimSpace(b)) == 0 || bytes.TrimSpace(b)[0] != '{' {
		return bad
	}
	if e := value(0); e != nil {
		return e
	}
	if _, e := d.Token(); e != io.EOF {
		return bad
	}
	return nil
}
