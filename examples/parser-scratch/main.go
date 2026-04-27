// Parser scratch buffer: demonstrates using the off-heap arena as a zero-GC
// tokenization buffer for JSON-like DSL parsing.
//
//	go run ./examples/parser-scratch/
package main

import (
	"fmt"

	"github.com/xDarkicex/memory"
)

type tokenKind byte

const (
	tokString tokenKind = iota
	tokNumber
	tokLBrace
	tokRBrace
	tokColon
	tokComma
)

type token struct {
	kind  tokenKind
	start int
	end   int
}

func main() {
	input := `{"name":"alice","score":42,"meta":{"active":true}}`

	pool, err := memory.NewPool(memory.AllocatorConfig{
		PoolSize:  4 * 1024 * 1024, // 4MB
		SlabSize:  64 * 1024,       // 64KB slabs
		SlabCount: 4,
		Prealloc:  true,
	})
	if err != nil {
		panic(err)
	}

	for i := 0; i < 1000; i++ {
		tokens, buf := tokenize(pool, input)
		_ = tokens
		_ = buf
		pool.Reset()
	}

	s := pool.Stats()
	fmt.Printf("Parsed 1000 requests\n")
	fmt.Printf("Tokens per parse: %d\n", countTokens(input))
	fmt.Printf("Peak allocated: %d bytes\n", s.Allocated)
}

func countTokens(input string) int {
	n := 0
	inputBytes := []byte(input)
	for i := 0; i < len(inputBytes); i++ {
		switch inputBytes[i] {
		case '{', '}', ':', ',':
			n++
		case '"':
			n++
			for i++; i < len(inputBytes) && inputBytes[i] != '"'; i++ {
			}
		default:
			if inputBytes[i] >= '0' && inputBytes[i] <= '9' {
				n++
				for i < len(inputBytes) && inputBytes[i] >= '0' && inputBytes[i] <= '9' {
					i++
				}
				i--
			}
		}
	}
	return n
}

func tokenize(pool *memory.Pool, input string) ([]token, []byte) {
	size := uint64(len(input)) + 1024
	data, err := pool.Allocate(size)
	if err != nil {
		panic(err)
	}
	buf := data[:0]
	inputBytes := []byte(input)
	tokens := make([]token, 0, 32)

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
			// skip whitespace
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
