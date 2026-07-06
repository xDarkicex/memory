package memory
import (
	"fmt"
	"testing"
	"unsafe"
)
func TestDebug5(t *testing.T) {
	m, _ := NewHashMap(HashMapConfig{Capacity: 8, Alignment: 128})
	for i := uint64(1); i <= 7; i++ {
		dummy := new(int)
		*dummy = int(i)
		m.Put(i, unsafe.Pointer(dummy))
		
		// Print map state
		s := m.state.Load()
		fmt.Printf("After inserting %d:\n", i)
		for bIdx := uint64(0); bIdx < s.size; bIdx++ {
			bAddr := s.base + uintptr(bIdx)*128
			b := (*Bucket)(unsafe.Pointer(bAddr))
			meta := b.Metadata.Load()
			if meta != 0 {
				fmt.Printf("  Bucket %d: meta=%016x O=%b\n", bIdx, meta, meta&0x7F)
				for j := 0; j < 7; j++ {
					if (meta & (1 << j)) != 0 {
						k := b.Keys[j].Load()
						fmt.Printf("    Slot %d: key=%d\n", j, k)
					}
				}
			}
		}
	}
	
	for i := uint64(1); i <= 7; i++ {
		_, ok := m.Get(i)
		if !ok {
			fmt.Printf("FAILED to get key %d\n", i)
		}
	}
}
