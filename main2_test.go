package memory

import (
	"fmt"
	"testing"
	"unsafe"
)

func TestMapState2(t *testing.T) {
	m, _ := NewHashMap(HashMapConfig{Capacity: 8, Alignment: 128})
	for i := uint64(1); i <= 7; i++ {
		dummy := new(int)
		*dummy = int(i)
		m.Put(i, unsafe.Pointer(dummy))
	}
	
	// Test GET 1
	key := uint64(1)
	hash := hashKey(key)
	h2 := uint8(hash >> 56)
	s := m.state.Load()
	startIdx := hash & (s.size - 1)
	
	fmt.Printf("Get(1): hash=%x h2=%d startIdx=%d\n", hash, h2, startIdx)
	
	for i := uint64(0); i < s.size; i++ {
		bucketIdx := (startIdx + i) & (s.size - 1)
		bAddr := s.base + uintptr(bucketIdx)*128
		b := (*Bucket)(unsafe.Pointer(bAddr))
		meta := b.Metadata.Load()
		fmt.Printf("  Bucket %d: meta=%016x\n", bucketIdx, meta)
		
		match := matchMask(meta, h2)
		fmt.Printf("    match=%b\n", match)
		
		for match != 0 {
			idx := firstMatch(match)
			k := b.Keys[idx].Load()
			fmt.Printf("      check slot %d: key=%d\n", idx, k)
			match &= match - 1
		}
		
		O := uint8(meta & 0x7F)
		P := uint8((meta >> 21) & 0x7F)
		if (O | P) != 0x7F {
			fmt.Printf("    break early! O=%b P=%b\n", O, P)
			break
		}
	}
}
