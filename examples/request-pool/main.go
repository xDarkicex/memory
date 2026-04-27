// Request pool: demonstrates per-request scratch allocation with bulk-free
// after each response. The arena replaces sync.Pool for request-scoped buffers.
//
//	go run ./examples/request-pool/
package main

import (
	"encoding/binary"
	"fmt"

	"github.com/xDarkicex/memory"
)

const (
	tagStatusCode = 1
	tagContentLen = 2
	tagBody       = 3
	tagRequestID  = 4
)

func main() {
	pool, err := memory.NewPool(memory.AllocatorConfig{
		PoolSize:  16 * 1024 * 1024, // 16MB
		SlabSize:  256 * 1024,       // 256KB slabs
		SlabCount: 8,
		Prealloc:  true,
	})
	if err != nil {
		panic(err)
	}

	totalBytes := 0
	for i := 0; i < 1000; i++ {
		buf := handleRequest(pool, uint64(i), "application/octet-stream", []byte("payload-"+itoa(i)))
		totalBytes += len(buf)
		pool.Reset()
	}

	s := pool.Stats()
	fmt.Printf("Served 1000 requests\n")
	fmt.Printf("Total response bytes: %d\n", totalBytes)
	fmt.Printf("Peak allocated: %d bytes\n", s.Allocated)
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	b := make([]byte, 0, 10)
	neg := n < 0
	if neg {
		n = -n
	}
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	if neg {
		b = append([]byte{'-'}, b...)
	}
	return string(b)
}

func handleRequest(pool *memory.Pool, reqID uint64, contentType string, body []byte) []byte {
	data, err := pool.Allocate(4096)
	if err != nil {
		panic(err)
	}
	buf := data[:0]

	buf = appendTLV(buf, tagStatusCode, []byte{0xC8, 0x00}) // 200

	cl := make([]byte, 8)
	binary.LittleEndian.PutUint64(cl, uint64(len(body)))
	buf = appendTLV(buf, tagContentLen, cl)

	buf = appendTLV(buf, tagBody, body)

	rid := make([]byte, 8)
	binary.LittleEndian.PutUint64(rid, reqID)
	buf = appendTLV(buf, tagRequestID, rid)

	return buf
}

func appendTLV(buf []byte, tag byte, value []byte) []byte {
	buf = append(buf, tag)
	buf = append(buf, byte(len(value)))
	return append(buf, value...)
}
