package main

import (
	"testing"

	"github.com/xDarkicex/memory"
)

func newRequestPool(tb testing.TB) *memory.Pool {
	tb.Helper()
	p, err := memory.NewPool(memory.AllocatorConfig{
		PoolSize:  16 * 1024 * 1024,
		SlabSize:  256 * 1024,
		SlabCount: 8,
		Prealloc:  true,
	})
	if err != nil {
		tb.Fatal(err)
	}
	return p
}

func TestRequestPool(t *testing.T) {
	pool := newRequestPool(t)
	defer pool.Free()

	buf := handleRequest(pool, 42, "application/octet-stream", []byte("hello"))
	if len(buf) == 0 {
		t.Fatal("empty response buffer")
	}
	// Verify tag structure: 4 tags × (1 tag + 1 len + N value)
	if len(buf) < 8 {
		t.Fatalf("response too short: %d bytes", len(buf))
	}
}

func TestRequestPoolWithHelpers(t *testing.T) {
	pool := newRequestPool(t)
	defer pool.Free()

	buf := handleRequestWithHelpers(pool, 42, "application/octet-stream", []byte("hello"))
	if len(buf) == 0 {
		t.Fatal("empty response buffer")
	}
	if len(buf) < 8 {
		t.Fatalf("response too short: %d bytes", len(buf))
	}
}

func TestRequestPoolReset(t *testing.T) {
	pool := newRequestPool(t)

	for i := 0; i < 100; i++ {
		_ = handleRequest(pool, uint64(i), "text/plain", []byte("data"))
		pool.Reset()
	}

	s := pool.Stats()
	if s.Reserved != 0 {
		t.Fatalf("expected 0 reserved after reset, got %d", s.Reserved)
	}
}

func BenchmarkArenaRequestPool(b *testing.B) {
	pool := newRequestPool(b)

	b.ReportAllocs()
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		_ = handleRequest(pool, uint64(i%1000), "application/octet-stream", []byte("benchmark-payload"))
		pool.Reset()
	}
}

var requestSink []byte

func BenchmarkStdRequestPool(b *testing.B) {
	body := []byte("benchmark-payload")

	b.ReportAllocs()
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		requestSink = stdHandleRequest(uint64(i%1000), body)
	}
}

func stdHandleRequest(reqID uint64, body []byte) []byte {
	buf := make([]byte, 0, 4096)
	buf = appendTag(buf, tagStatusCode, []byte{0xC8, 0x00})
	buf = appendTag(buf, tagContentLen, []byte{byte(len(body))})
	buf = appendTag(buf, tagBody, body)
	return buf
}

func appendTag(buf []byte, tag byte, value []byte) []byte {
	buf = append(buf, tag, byte(len(value)))
	return append(buf, value...)
}
