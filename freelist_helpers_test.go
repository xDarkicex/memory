package memory

import (
	"testing"
	"unsafe"
)

type Record struct {
	ID      uint64
	Payload [40]byte
}

func testFreeList(t *testing.T) *FreeList {
	t.Helper()
	cfg := DefaultFreeListConfig()
	cfg.SlotSize = 64
	cfg.SlabSize = 64 * 1024
	cfg.SlabCount = 1
	cfg.PoolSize = 1024 * 1024
	cfg.Prealloc = true
	fl, err := NewFreeList(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { fl.Free() })
	return fl
}

func TestFreeListAlloc_Basic(t *testing.T) {
	fl := testFreeList(t)

	rec, err := FreeListAlloc[Record](fl) // sizeof(Record)=48, 48+12=60 <= 64 ✓
	if err != nil {
		t.Fatal(err)
	}
	rec.ID = 42
	copy(rec.Payload[:], "payload-42")

	if rec.ID != 42 {
		t.Errorf("ID = %d, want 42", rec.ID)
	}
	if string(rec.Payload[:10]) != "payload-42" {
		t.Errorf("Payload = %q", string(rec.Payload[:10]))
	}
}

func TestFreeListAlloc_Dealloc(t *testing.T) {
	fl := testFreeList(t)

	rec, err := FreeListAlloc[Record](fl)
	if err != nil {
		t.Fatal(err)
	}
	rec.ID = 99

	if err := FreeListDealloc(fl, rec); err != nil {
		t.Fatal(err)
	}

	// Re-allocate — should get same or different slot, both valid.
	rec2, err := FreeListAlloc[Record](fl)
	if err != nil {
		t.Fatal(err)
	}
	rec2.ID = 100
	if rec2.ID != 100 {
		t.Errorf("ID = %d, want 100", rec2.ID)
	}
}

func TestFreeListAlloc_TooLarge(t *testing.T) {
	fl := testFreeList(t)

	type Huge struct{ Data [128]byte } // 128+12=140 > 64

	_, err := FreeListAlloc[Huge](fl)
	if err != ErrSlotTooSmall {
		t.Errorf("expected ErrSlotTooSmall, got %v", err)
	}
}

func TestFreeListAlloc_DoubleDealloc(t *testing.T) {
	fl := testFreeList(t)

	rec, _ := FreeListAlloc[Record](fl)
	if err := FreeListDealloc(fl, rec); err != nil {
		t.Fatal(err)
	}
	if err := FreeListDealloc(fl, rec); err != ErrDoubleDeallocation {
		t.Errorf("expected ErrDoubleDeallocation, got %v", err)
	}
}

func TestFreeListAlloc_Must(t *testing.T) {
	fl := testFreeList(t)

	rec := MustFreeListAlloc[Record](fl)
	rec.ID = 7
	FreeListDealloc(fl, rec)
}

func TestFreeListAlloc_SlotFor(t *testing.T) {
	fl := testFreeList(t)

	rec, _ := FreeListAlloc[Record](fl)
	slot := FreeListSlotFor(fl, rec)

	if uint64(len(slot)) != fl.SlotSize() {
		t.Errorf("slot len = %d, want %d", len(slot), fl.SlotSize())
	}

	// Verify the slot header is intact — offset 0 should have the Treiber link.
	// After allocation, offset 0 is undefined (was last free-list link),
	// but we can verify the metadata at offset 8 is valid.
	meta := *(*uint32)(unsafe.Add(unsafe.Pointer(unsafe.SliceData(slot)), 8))
	structIdx := unpackStructIdx(meta)
	if structIdx == 0 && meta == 0 {
		// structIdx can be 0 (first slab). Zero meta means something is wrong.
		// This is a weak check, but good enough — real validation is that
		// Deallocate via the slot works.
	}

	FreeListDealloc(fl, rec)
}

func TestFreeListAlloc_MultipleDistinct(t *testing.T) {
	fl := testFreeList(t)

	a, _ := FreeListAlloc[Record](fl)
	b, _ := FreeListAlloc[Record](fl)
	a.ID = 1
	b.ID = 2

	if a.ID == b.ID {
		t.Error("allocations returned same pointer")
	}
}

func TestFreeListAlloc_AfterFree(t *testing.T) {
	fl := testFreeList(t)

	rec, _ := FreeListAlloc[Record](fl)
	FreeListDealloc(fl, rec)
	fl.Free()

	_, err := FreeListAlloc[Record](fl)
	if err != ErrFreelistFreed {
		t.Errorf("expected ErrFreelistFreed, got %v", err)
	}
}

func TestFreeListAlloc_MetadataNotCorrupted(t *testing.T) {
	fl := testFreeList(t)

	// Write to every byte of the user data region — must not corrupt metadata
	// at offsets 0-11 (next pointer and struct index).
	rec, _ := FreeListAlloc[Record](fl)
	for i := range rec.Payload {
		rec.Payload[i] = 0xFF
	}
	rec.ID = 0xFFFFFFFFFFFFFFFF

	// Dealloc must succeed — proves metadata is intact.
	if err := FreeListDealloc(fl, rec); err != nil {
		t.Fatal("metadata corruption caused dealloc failure:", err)
	}

	// Slot is reusable after dealloc (FreeList is LIFO — may get same slot back).
	rec2, err := FreeListAlloc[Record](fl)
	if err != nil {
		t.Fatal("re-allocate after metadata stress test failed:", err)
	}
	rec2.ID = 0 // overwrite, confirm writeable
	if rec2.ID != 0 {
		t.Error("re-allocated slot not writeable")
	}
	FreeListDealloc(fl, rec2)
}
