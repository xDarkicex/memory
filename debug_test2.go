package memory
import (
	"testing"
	"unsafe"
	"fmt"
)
func TestDebugPutGet2(t *testing.T) {
	m, _ := NewHashMap(HashMapConfig{Capacity: 8, Alignment: 128})
	for i := uint64(1); i <= 7; i++ {
		dummy := new(int)
		*dummy = int(i)
		m.Put(i, unsafe.Pointer(dummy))
		
		s := m.state.Load()
		hash := hashKey(i)
		bucketIdx := hash & (s.size - 1)
		bAddr := s.base + uintptr(bucketIdx)*128
		b := (*Bucket)(unsafe.Pointer(bAddr))
		meta := b.Metadata.Load()
		O := uint8(meta & 0x7F)
		fmt.Printf("Put %d: hash=%x, bucketIdx=%d, O=%b\n", i, hash, bucketIdx, O)
	}
	for i := uint64(1); i <= 7; i++ {
		_, ok := m.Get(i)
		fmt.Printf("Get %d: %v\n", i, ok)
	}
}
