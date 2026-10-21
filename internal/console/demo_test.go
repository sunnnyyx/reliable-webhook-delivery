package console

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestDemoSessionNeedsNoKeyAndKeepsCSRF(t *testing.T) {
	s, e := NewDemo("127.0.0.1:8091", "")
	if e != nil {
		t.Fatal(e)
	}
	defer s.Close()
	w := send(s, "POST", "/demo/session", `{}`, nil, "")
	if w.Code != 200 {
		t.Fatal(w.Code)
	}
	var data struct {
		CSRF string `json:"csrf"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &data)
	c := w.Result().Cookies()[0]
	if data.CSRF == "" || !c.HttpOnly {
		t.Fatal("unsafe anonymous session")
	}
	if send(s, "POST", "/api/v1/events", `{"type":"sample","data":{}}`, c, "").Code != 403 {
		t.Fatal("CSRF disabled")
	}
	if send(s, "POST", "/session", `{"key":"`+testKey+`"}`, nil, "").Code != 404 {
		t.Fatal("private API key accepted in playground")
	}
	r := httptest.NewRequest("POST", "http://127.0.0.1:8091/demo/session", strings.NewReader(`{}`))
	r.Header.Set("Origin", "https://evil.example")
	out := httptest.NewRecorder()
	s.ServeHTTP(out, r)
	if out.Code != 403 {
		t.Fatal("cross-origin visitor creation accepted")
	}
	if send(s, "DELETE", "/session", "", c, data.CSRF).Code != 204 {
		t.Fatal("could not end demo")
	}
	if send(s, "GET", "/api/v1/events", "", c, "").Code != 401 {
		t.Fatal("ended demo remained accessible")
	}
	if _, err := NewDemo("127.0.0.1:8091", "http://public.example"); err == nil {
		t.Fatal("public insecure origin accepted")
	}
}
