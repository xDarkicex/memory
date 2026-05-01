package main

import (
	"testing"

	"github.com/xDarkicex/memory"
)

func newTestPool(tb testing.TB) *memory.Pool {
	tb.Helper()
	p, err := memory.NewPool(memory.AllocatorConfig{
		PoolSize:  4 * 1024 * 1024,
		SlabSize:  64 * 1024,
		SlabCount: 4,
		Prealloc:  true,
	})
	if err != nil {
		tb.Fatal(err)
	}
	return p
}

func TestParserScratch(t *testing.T) {
	pool := newTestPool(t)
	defer pool.Free()

	input := `{"key":"value","num":123}`
	tokens, _ := tokenize(pool, input)
	if len(tokens) != 9 { // {, "key", :, "value", ,, "num", :, 123, }
		t.Fatalf("expected 9 tokens, got %d", len(tokens))
	}
}

func TestParserScratchWithHelpers(t *testing.T) {
	pool := newTestPool(t)
	defer pool.Free()

	input := `{"key":"value","num":123}`
	tokens, _ := tokenizeWithHelpers(pool, input)
	if len(tokens) != 9 {
		t.Fatalf("expected 9 tokens, got %d", len(tokens))
	}
}

func TestParserScratchReset(t *testing.T) {
	pool := newTestPool(t)

	for i := 0; i < 10; i++ {
		_, _ = tokenize(pool, `{"a":"b"}`)
		pool.Reset()
	}

	s := pool.Stats()
	if s.Reserved != 0 {
		t.Fatalf("expected 0 reserved after reset, got %d", s.Reserved)
	}
}

func BenchmarkArenaParser(b *testing.B) {
	pool := newTestPool(b)
	input := `{"name":"alice","score":42,"meta":{"active":true}}`

	b.ReportAllocs()
	b.SetBytes(int64(len(input)))
	b.ResetTimer()

	tokens := make([]token, 0, 32)
	for i := 0; i < b.N; i++ {
		tokens, _ = arenaTokenize(pool, input, tokens[:0])
		pool.Reset()
	}
	_ = tokens
}

var parserSink []byte

func BenchmarkStdParser(b *testing.B) {
	input := `{"name":"alice","score":42,"meta":{"active":true}}`

	b.ReportAllocs()
	b.SetBytes(int64(len(input)))
	b.ResetTimer()

	tokens := make([]token, 0, 32)
	for i := 0; i < b.N; i++ {
		tokens, parserSink = stdTokenize(input, tokens[:0])
	}
	_ = tokens
}

func arenaTokenize(pool *memory.Pool, input string, tokens []token) ([]token, []byte) {
	size := uint64(len(input)) + 1024
	data, _ := pool.Allocate(size)
	buf := data[:0]
	inputBytes := []byte(input)

	for i := 0; i < len(inputBytes); i++ {
		c := inputBytes[i]
		switch c {
		case '{':
			tokens = append(tokens, token{tokLBrace, len(buf), len(buf)})
		case '}':
			tokens = append(tokens, token{tokRBrace, len(buf), len(buf)})
		case ':':
			tokens = append(tokens, token{tokColon, len(buf), len(buf)})
		case ',':
			tokens = append(tokens, token{tokComma, len(buf), len(buf)})
		case '"':
			start := len(buf)
			i++
			for i < len(inputBytes) && inputBytes[i] != '"' {
				buf = append(buf, inputBytes[i])
				i++
			}
			tokens = append(tokens, token{tokString, start, len(buf)})
		case ' ', '\t', '\n', '\r':
		default:
			if c >= '0' && c <= '9' || c == '-' {
				start := len(buf)
				for i < len(inputBytes) && (inputBytes[i] >= '0' && inputBytes[i] <= '9' || inputBytes[i] == '.' || inputBytes[i] == '-') {
					buf = append(buf, inputBytes[i])
					i++
				}
				i--
				tokens = append(tokens, token{tokNumber, start, len(buf)})
			}
		}
	}
	return tokens, buf
}

func stdTokenize(input string, tokens []token) ([]token, []byte) {
	buf := make([]byte, 0, 4096)
	inputBytes := []byte(input)

	for i := 0; i < len(inputBytes); i++ {
		c := inputBytes[i]
		switch c {
		case '{':
			tokens = append(tokens, token{tokLBrace, len(buf), len(buf)})
		case '}':
			tokens = append(tokens, token{tokRBrace, len(buf), len(buf)})
		case ':':
			tokens = append(tokens, token{tokColon, len(buf), len(buf)})
		case ',':
			tokens = append(tokens, token{tokComma, len(buf), len(buf)})
		case '"':
			start := len(buf)
			i++
			for i < len(inputBytes) && inputBytes[i] != '"' {
				buf = append(buf, inputBytes[i])
				i++
			}
			tokens = append(tokens, token{tokString, start, len(buf)})
		case ' ', '\t', '\n', '\r':
		default:
			if c >= '0' && c <= '9' || c == '-' {
				start := len(buf)
				for i < len(inputBytes) && (inputBytes[i] >= '0' && inputBytes[i] <= '9' || inputBytes[i] == '.' || inputBytes[i] == '-') {
					buf = append(buf, inputBytes[i])
					i++
				}
				i--
				tokens = append(tokens, token{tokNumber, start, len(buf)})
			}
		}
	}
	return tokens, buf
}
