package memory
import (
	"testing"
	"unsafe"
)
func TestDebugPutGet(t *testing.T) {
	m, _ := NewHashMap(HashMapConfig{Capacity: 8, Alignment: 128})
	dummy := new(int)
	*dummy = 1
	m.Put(1, unsafe.Pointer(dummy))
	v, ok := m.Get(1)
	if !ok {
		t.Fatalf("FAILED TO GET 1")
	}
	t.Logf("Got %v", v)
}
