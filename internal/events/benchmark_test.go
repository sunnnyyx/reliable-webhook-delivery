package events

import (
	"bytes"
	"testing"
	"time"
)

// CPU/allocation profiles isolate JSON validation and immutable envelope creation;
// they do not stand in for API/PostgreSQL end-to-end measurements.
func BenchmarkAcceptancePayload(b *testing.B) {
	for _, size := range []int{1024, 64 << 10, 250 << 10} {
		b.Run(stringSize(size), func(b *testing.B) {
			raw := append([]byte(`{"type":"bench.event","data":{"synthetic":"`), bytes.Repeat([]byte("x"), size)...)
			raw = append(raw, []byte(`"}}`)...)
			now := time.Date(2026, 10, 16, 0, 0, 0, 0, time.UTC)
			b.SetBytes(int64(len(raw)))
			b.ReportAllocs()
			for b.Loop() {
				input, e := Parse(raw)
				if e != nil {
					b.Fatal(e)
				}
				body := Envelope("11111111-1111-4111-8111-111111111111", now, input)
				_ = Hash(body)
			}
		})
	}
}
func stringSize(n int) string {
	switch n {
	case 1024:
		return "1KiB"
	case 64 << 10:
		return "64KiB"
	default:
		return "250KiB"
	}
}
