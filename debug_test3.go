package memory
import (
	"testing"
	"unsafe"
	"fmt"
)
func TestDebug3(t *testing.T) {
	m, _ := NewHashMap(HashMapConfig{Capacity: 8, Alignment: 128})
	for i := uint64(1); i <= 7; i++ {
		dummy := new(int)
		*dummy = int(i)
		m.Put(i, unsafe.Pointer(dummy))
	}
	for i := uint64(1); i <= 7; i++ {
		v, ok := m.Get(i)
		if !ok {
			fmt.Printf("Key %d: !ok\n", i)
		} else {
			fmt.Printf("Key %d: v=%d\n", i, *(*int)(v))
		}
	}
}
