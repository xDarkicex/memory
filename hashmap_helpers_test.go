package memory

import (
	"testing"
)

func TestSWARMatch(t *testing.T) {
	// 5-bit fingerprint: 0x15 (21)
	// We want to pack it into slot 3.
	// Slot 3 shift is 29 + 3*5 = 44.
	// We also must set the Occupancy bit for slot 3 (bit 3).
	meta := (uint64(0x15) << 44) | (1 << 3)
	match := matchMask(meta, 0x15)

	if match != (1 << 3) {
		t.Fatalf("expected 1<<3, got %x", match)
	}
}

func TestSWARMatchIgnoresEmptyAndTombstoneSlots(t *testing.T) {
	const fp = uint64(0x15)

	meta := uint64(0)
	meta |= fp << (29 + 0*5) // empty: matching fingerprint, no O bit
	meta |= fp << (29 + 1*5) // tombstone: matching fingerprint, no O bit
	meta |= 1 << (21 + 1)    // P bit
	meta |= fp << (29 + 2*5) // occupied match
	meta |= 1 << 2
	meta |= uint64(0x03) << (29 + 3*5) // occupied non-match
	meta |= 1 << 3

	match := matchMask(meta, uint8(fp))
	if match != 1<<2 {
		t.Fatalf("expected only occupied slot 2 to match, got %07b", match)
	}
}

func TestSWARMatchDoesNotMissPackedFingerprints(t *testing.T) {
	for target := uint64(0); target < 32; target++ {
		for slot := uint64(0); slot < 7; slot++ {
			meta := uint64(0)
			for i := uint64(0); i < 7; i++ {
				fp := (target + i + 1) & 0x1F
				if i == slot {
					fp = target
				}
				meta |= 1 << i
				meta |= fp << (29 + i*5)
			}

			match := matchMask(meta, uint8(target))
			if match&(1<<slot) == 0 {
				t.Fatalf("target=%d slot=%d missed; match=%07b meta=%016x", target, slot, match, meta)
			}
		}
	}
}

func TestSWAREmpty(t *testing.T) {
	// All slots empty except slot 0 (Occupancy bit 0 is set)
	meta := uint64(1)
	empty := emptyMask(meta)

	// Expected: all bits 1-6 are set (empty), bit 0 is 0 (occupied).
	// So mask is 0x7E (1111110)
	if empty != 0x7E {
		t.Fatalf("expected 0x7E, got %x", empty)
	}
}
