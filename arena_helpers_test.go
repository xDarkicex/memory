package memory

import (
	"errors"
	"testing"
)

type Cat struct {
	Name [32]byte
	Age  int
}

func TestArenaAlloc_Basic(t *testing.T) {
	arena, err := NewArena(64 << 10)
	if err != nil {
		t.Fatal(err)
	}
	defer arena.Free()

	cat := MustArenaAlloc[Cat](arena)
	copy(cat.Name[:], "Whiskers")
	cat.Age = 3

	if cat.Age != 3 {
		t.Errorf("Age = %d, want 3", cat.Age)
	}
	if string(cat.Name[:8]) != "Whiskers" {
		t.Errorf("Name = %q, want Whiskers", string(cat.Name[:8]))
	}
}

func TestArenaAlloc_Error(t *testing.T) {
	arena, err := NewArena(64 << 10)
	if err != nil {
		t.Fatal(err)
	}
	defer arena.Free()

	// Zero-sized type: Arena rejects size=0 allocations.
	_, err = ArenaAlloc[struct{}](arena)
	if !errors.Is(err, ErrInvalidSize) {
		t.Errorf("expected ErrInvalidSize, got %v", err)
	}
}

func TestArenaAlloc_MultipleDistinct(t *testing.T) {
	arena, err := NewArena(64 << 10)
	if err != nil {
		t.Fatal(err)
	}
	defer arena.Free()

	a := MustArenaAlloc[Cat](arena)
	b := MustArenaAlloc[Cat](arena)
	a.Age = 1
	b.Age = 2

	if a.Age == b.Age {
		t.Error("allocations returned same pointer for distinct calls")
	}
}

func TestArenaSlice_Basic(t *testing.T) {
	arena, err := NewArena(64 << 10)
	if err != nil {
		t.Fatal(err)
	}
	defer arena.Free()

	toys := MustArenaSlice[Cat](arena, 4)
	if len(toys) != 0 {
		t.Errorf("len = %d, want 0", len(toys))
	}
	if cap(toys) != 4 {
		t.Errorf("cap = %d, want 4", cap(toys))
	}

	toys = append(toys, Cat{Age: 1}, Cat{Age: 2})
	if len(toys) != 2 {
		t.Errorf("len = %d, want 2", len(toys))
	}
	if cap(toys) != 4 {
		t.Errorf("cap grew = %d, want 4", cap(toys))
	}
}

func TestArenaSlice_ZeroCap(t *testing.T) {
	arena, err := NewArena(64 << 10)
	if err != nil {
		t.Fatal(err)
	}
	defer arena.Free()

	s, err := ArenaSlice[int](arena, 0)
	if err != nil {
		t.Fatal(err)
	}
	if s != nil {
		t.Errorf("expected nil slice for cap=0, got %v", s)
	}
}

func TestArenaNewString_Basic(t *testing.T) {
	arena, err := NewArena(64 << 10)
	if err != nil {
		t.Fatal(err)
	}
	defer arena.Free()

	s := MustArenaNewString(arena, "hello, arena")
	if s != "hello, arena" {
		t.Errorf("got %q, want %q", s, "hello, arena")
	}
}

func TestArenaNewString_Empty(t *testing.T) {
	arena, err := NewArena(64 << 10)
	if err != nil {
		t.Fatal(err)
	}
	defer arena.Free()

	s, err := ArenaNewString(arena, "")
	if err != nil {
		t.Fatal(err)
	}
	if s != "" {
		t.Errorf("got %q, want empty", s)
	}
}

func TestArenaNewString_InStruct(t *testing.T) {
	arena, err := NewArena(64 << 10)
	if err != nil {
		t.Fatal(err)
	}
	defer arena.Free()

	type Dog struct {
		Name string
		Age  int
	}

	dog := MustArenaAlloc[Dog](arena)
	dog.Name = MustArenaNewString(arena, "Rex")
	dog.Age = 5

	if dog.Name != "Rex" {
		t.Errorf("Name = %q, want Rex", dog.Name)
	}
	if dog.Age != 5 {
		t.Errorf("Age = %d, want 5", dog.Age)
	}
}

func TestArenaAppend_Basic(t *testing.T) {
	arena, err := NewArena(64 << 10)
	if err != nil {
		t.Fatal(err)
	}
	defer arena.Free()

	nums := MustArenaSlice[int](arena, 4)
	nums = ArenaAppend(arena, nums, 1, 2, 3)
	if len(nums) != 3 {
		t.Errorf("len = %d, want 3", len(nums))
	}
	if cap(nums) != 4 {
		t.Errorf("cap = %d, want 4", cap(nums))
	}
}

func TestArenaAppend_PanicsOnOverflow(t *testing.T) {
	arena, err := NewArena(64 << 10)
	if err != nil {
		t.Fatal(err)
	}
	defer arena.Free()

	defer func() {
		r := recover()
		if r == nil {
			t.Error("ArenaAppend did not panic on overflow")
		}
		if !errors.Is(r.(error), ErrArenaCapacityExceeded) {
			t.Errorf("panic value = %v, want ErrArenaCapacityExceeded", r)
		}
	}()

	nums := MustArenaSlice[int](arena, 2)
	nums = ArenaAppend(arena, nums, 1, 2)
	nums = ArenaAppend(arena, nums, 3)
}

func TestArenaAppend_ZeroElems(t *testing.T) {
	arena, err := NewArena(64 << 10)
	if err != nil {
		t.Fatal(err)
	}
	defer arena.Free()

	nums := MustArenaSlice[int](arena, 4)
	nums = ArenaAppend(arena, nums, 1) // len=1
	nums = ArenaAppend(arena, nums)    // no-op append
	if len(nums) != 1 || nums[0] != 1 {
		t.Error("empty ArenaAppend modified slice")
	}
}

func TestMustArenaAlloc_AfterFree_Panics(t *testing.T) {
	arena, err := NewArena(64 << 10)
	if err != nil {
		t.Fatal(err)
	}

	cat := MustArenaAlloc[Cat](arena)
	cat.Age = 42
	arena.Free()

	defer func() {
		if r := recover(); r == nil {
			t.Error("MustArenaAlloc after Free did not panic")
		}
	}()
	MustArenaAlloc[Cat](arena)
}

func TestArenaAlloc_LargeType(t *testing.T) {
	arena, err := NewArena(1 << 20)
	if err != nil {
		t.Fatal(err)
	}
	defer arena.Free()

	type Big struct {
		Data [8192]byte
	}

	big := MustArenaAlloc[Big](arena)
	copy(big.Data[:], "payload")
	if string(big.Data[:7]) != "payload" {
		t.Error("large alloc failed")
	}
}
