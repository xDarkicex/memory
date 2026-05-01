package memory

import (
	"testing"
)

func testPool(t *testing.T) *Pool {
	t.Helper()
	pool, err := NewPool(DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { pool.Free() })
	return pool
}

func TestPoolAlloc_Basic(t *testing.T) {
	pool := testPool(t)

	cat := MustPoolAlloc[Cat](pool)
	copy(cat.Name[:], "Whiskers")
	cat.Age = 3

	if cat.Age != 3 {
		t.Errorf("Age = %d, want 3", cat.Age)
	}
	if string(cat.Name[:8]) != "Whiskers" {
		t.Errorf("Name = %q, want Whiskers", string(cat.Name[:8]))
	}
}

func TestPoolAlloc_Error(t *testing.T) {
	pool := testPool(t)

	// Zero-sized type: Pool rejects size=0 allocations.
	_, err := PoolAlloc[struct{}](pool)
	if err == nil {
		t.Error("PoolAlloc[struct{}] did not return error on zero-size alloc")
	}
}

func TestPoolAlloc_MultipleDistinct(t *testing.T) {
	pool := testPool(t)

	a := MustPoolAlloc[Cat](pool)
	b := MustPoolAlloc[Cat](pool)
	a.Age = 1
	b.Age = 2

	if a.Age == b.Age {
		t.Error("allocations returned same pointer for distinct calls")
	}
}

func TestPoolSlice_Basic(t *testing.T) {
	pool := testPool(t)

	ids := MustPoolSlice[int64](pool, 8)
	if len(ids) != 0 {
		t.Errorf("len = %d, want 0", len(ids))
	}
	if cap(ids) != 8 {
		t.Errorf("cap = %d, want 8", cap(ids))
	}

	ids = append(ids, 1, 2, 3)
	if len(ids) != 3 || cap(ids) != 8 {
		t.Errorf("len=%d cap=%d, want len=3 cap=8", len(ids), cap(ids))
	}
}

func TestPoolSlice_ZeroCap(t *testing.T) {
	pool := testPool(t)

	s, err := PoolSlice[int](pool, 0)
	if err != nil {
		t.Fatal(err)
	}
	if s != nil {
		t.Errorf("expected nil slice for cap=0, got %v", s)
	}
}

func TestPoolSlice_LargeBacking(t *testing.T) {
	pool := testPool(t)

	type Big struct {
		Data [4096]byte
	}

	s := MustPoolSlice[Big](pool, 4)
	if cap(s) != 4 {
		t.Errorf("cap = %d, want 4", cap(s))
	}
}

func TestMustPoolAlloc_AfterFree_Panics(t *testing.T) {
	pool, err := NewPool(DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}

	cat := MustPoolAlloc[Cat](pool)
	cat.Age = 42
	pool.Free()

	defer func() {
		if r := recover(); r == nil {
			t.Error("MustPoolAlloc after Free did not panic")
		}
	}()
	MustPoolAlloc[Cat](pool)
}
