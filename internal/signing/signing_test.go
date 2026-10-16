package signing

import (
	"bytes"
	"encoding/base64"
	"strconv"
	"testing"
	"time"
)

func TestPublishedStandardWebhooksVector(t *testing.T) {
	key, e := DecodeSecret("MfKQ9r8GKYqrTwjUPD8ILPZIo2LaLaSw")
	if e != nil {
		t.Fatal(e)
	}
	got, e := Sign("msg_p5jXN8AQM9LWM0D4loKWxJek", "1614265330", []byte(`{"test": 2432232314}`), [][]byte{key})
	if e != nil || got != "v1,g0hM9SsE+OTPJTGt/tmIKtSyZlE3uFJELVlNIOLJ1OE=" {
		t.Fatal("published HMAC vector mismatch")
	}
}
func TestDecodeGeneratedSecret(t *testing.T) {
	want := bytes.Repeat([]byte{42}, 32)
	encoded := base64.StdEncoding.EncodeToString(want)
	for _, input := range []string{encoded, "whsec_" + encoded} {
		got, err := DecodeSecret(input)
		if err != nil || !bytes.Equal(got, want) {
			t.Fatal("generated signing secret did not decode")
		}
	}
}

func TestVerifyRawBodyTimestampAndMultipleKeys(t *testing.T) {
	keys := [][]byte{bytes.Repeat([]byte{1}, 32), bytes.Repeat([]byte{2}, 32)}
	now := time.Unix(1700000000, 0)
	timestamp := strconv.FormatInt(now.Unix(), 10)
	raw := []byte(`{ "x": 1 }`)
	sig, e := Sign("delivery-id", timestamp, raw, keys)
	if e != nil {
		t.Fatal(e)
	}
	for _, accepted := range [][][]byte{keys, keys[:1], keys[1:]} {
		if e = Verify("delivery-id", timestamp, "v2,ignored "+sig, raw, accepted, now, 5*time.Minute); e != nil {
			t.Fatal(e)
		}
	}
	for _, tc := range []struct {
		id, stamp string
		body      []byte
		clock     time.Time
	}{{"other", timestamp, raw, now}, {"delivery-id", timestamp, []byte(`{"x":1}`), now}, {"delivery-id", timestamp, raw, now.Add(301 * time.Second)}, {"delivery-id", timestamp, raw, now.Add(-301 * time.Second)}, {"delivery-id", "01700000000", raw, now}} {
		if Verify(tc.id, tc.stamp, sig, tc.body, keys, tc.clock, 5*time.Minute) == nil {
			t.Fatal("invalid signature accepted")
		}
	}
	if _, e = DecodeSecret("whsec_bad"); e == nil {
		t.Fatal("bad secret accepted")
	}
	if _, e = Sign("ambiguous.id", timestamp, raw, keys); e == nil {
		t.Fatal("ambiguous ID accepted")
	}
}
