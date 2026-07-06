package memory

import (
	"testing"
)

type ShardedRecord struct {
	ID      uint64
	Payload [40]byte
}

func testShardedFreeList(t *testing.T) *ShardedFreeList {
	t.Helper()
	cfg := DefaultFreeListConfig()
	cfg.SlotSize = 64
	cfg.SlabSize = 64 * 1024
	cfg.SlabCount = 1
	cfg.PoolSize = 1024 * 1024
	cfg.Prealloc = true
	sfl, err := NewShardedFreeList(cfg, 64, 4)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { sfl.Free() })
	return sfl
}

func TestShardedFreeListAlloc_Basic(t *testing.T) {
	sfl := testShardedFreeList(t)

	rec, err := ShardedFreeListAlloc[ShardedRecord](sfl)
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

func TestShardedFreeListAlloc_Dealloc(t *testing.T) {
	sfl := testShardedFreeList(t)

	rec, err := ShardedFreeListAlloc[ShardedRecord](sfl)
	if err != nil {
		t.Fatal(err)
	}
	rec.ID = 99

	if err := ShardedFreeListDealloc(sfl, rec); err != nil {
		t.Fatal(err)
	}

	rec2, err := ShardedFreeListAlloc[ShardedRecord](sfl)
	if err != nil {
		t.Fatal(err)
	}
	rec2.ID = 100
	if rec2.ID != 100 {
		t.Errorf("ID = %d, want 100", rec2.ID)
	}
}

func TestShardedFreeListAlloc_Retire(t *testing.T) {
	sfl := testShardedFreeList(t)

	rec, err := ShardedFreeListAlloc[ShardedRecord](sfl)
	if err != nil {
		t.Fatal(err)
	}
	rec.ID = 99

	if err := ShardedFreeListRetire(sfl, rec); err != nil {
		t.Fatal(err)
	}
}

func TestShardedFreeListAlloc_TooLarge(t *testing.T) {
	sfl := testShardedFreeList(t)

	type Huge struct{ Data [128]byte } // 128+16=144 > 64

	_, err := ShardedFreeListAlloc[Huge](sfl)
	if err != ErrSlotTooSmall {
		t.Errorf("expected ErrSlotTooSmall, got %v", err)
	}
}

func TestShardedFreeListAlloc_Must(t *testing.T) {
	sfl := testShardedFreeList(t)

	rec := MustShardedFreeListAlloc[ShardedRecord](sfl)
	rec.ID = 7
	ShardedFreeListDealloc(sfl, rec)
}

func TestShardedFreeListAlloc_SlotFor(t *testing.T) {
	sfl := testShardedFreeList(t)

	rec, _ := ShardedFreeListAlloc[ShardedRecord](sfl)
	slot := ShardedFreeListSlotFor(sfl, rec)

	if uint64(len(slot)) != sfl.cfg.SlotSize {
		t.Errorf("slot len = %d, want %d", len(slot), sfl.cfg.SlotSize)
	}

	ShardedFreeListDealloc(sfl, rec)
}

func TestShardedFreeListAlloc_MultipleDistinct(t *testing.T) {
	sfl := testShardedFreeList(t)

	a, _ := ShardedFreeListAlloc[ShardedRecord](sfl)
	b, _ := ShardedFreeListAlloc[ShardedRecord](sfl)
	a.ID = 1
	b.ID = 2

	if a.ID == b.ID {
		t.Error("allocations returned same pointer")
	}
}

func TestShardedFreeListAlloc_DeallocFallbackOnCorruptedMetadata(t *testing.T) {
	sfl := testShardedFreeList(t)

	rec, _ := ShardedFreeListAlloc[ShardedRecord](sfl)
	for i := range rec.Payload {
		rec.Payload[i] = 0xFF
	}
	rec.ID = 0xFFFFFFFFFFFFFFFF

	if err := ShardedFreeListDealloc(sfl, rec); err != nil {
		t.Fatal("dealloc fallback failed after metadata corruption:", err)
	}

	rec2, err := ShardedFreeListAlloc[ShardedRecord](sfl)
	if err != nil {
		t.Fatal("re-allocate after metadata stress test failed:", err)
	}
	rec2.ID = 0
	if rec2.ID != 0 {
		t.Error("re-allocated slot not writeable")
	}
	ShardedFreeListDealloc(sfl, rec2)
}
