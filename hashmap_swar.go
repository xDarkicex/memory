package memory

import "math/bits"

const (
	fingerprintShift   = 29
	fingerprintMask    = uint64(0xFFFFFFFFE0000000)
	fingerprintLaneRep = uint64(1<<0 | 1<<5 | 1<<10 | 1<<15 | 1<<20 | 1<<25 | 1<<30)
	fingerprintLSBMask = fingerprintLaneRep << fingerprintShift
	fingerprintMSBMask = uint64(1<<33 | 1<<38 | 1<<43 | 1<<48 | 1<<53 | 1<<58 | 1<<63)
)

// extractMasks extracts the state-machine masks from the 64-bit metadata word.
//
//go:nosplit
func extractMasks(meta uint64) (O, H, F, P uint8, migrating bool) {
	O = uint8(meta & 0x7F)
	H = uint8((meta >> 7) & 0x7F)
	F = uint8((meta >> 14) & 0x7F)
	P = uint8((meta >> 21) & 0x7F)
	migrating = meta&bucketMigratingBit != 0
	return
}

// matchMask returns a 7-bit mask indicating which occupied slots match the 5-bit fingerprint.
//
//go:nosplit
func matchMask(meta uint64, h2 uint8) uint8 {
	target := (uint64(h2&0x1F) * fingerprintLaneRep) << fingerprintShift
	x := (meta ^ target) & fingerprintMask
	zeroLanes := (x - fingerprintLSBMask) &^ x & fingerprintMSBMask

	match := ((zeroLanes >> 33) & 0x01) |
		((zeroLanes >> 37) & 0x02) |
		((zeroLanes >> 41) & 0x04) |
		((zeroLanes >> 45) & 0x08) |
		((zeroLanes >> 49) & 0x10) |
		((zeroLanes >> 53) & 0x20) |
		((zeroLanes >> 57) & 0x40)

	return uint8(match) & uint8(meta&0x7F)
}

// emptyMask returns a 7-bit mask of empty slots using the Occupancy mask (O).
//
//go:nosplit
func emptyMask(meta uint64) uint8 {
	return (^uint8(meta & 0x7F)) & 0x7F
}

// firstMatch returns the 0-6 index of the first 1-bit in a 7-bit mask.
//
//go:nosplit
func firstMatch(mask uint8) uint32 {
	return uint32(bits.TrailingZeros8(mask))
}

// buildMetadata constructs a new 64-bit metadata word given the component fields.
//
//go:nosplit
func buildMetadata(O, H, F, P uint8, migrating bool, fps uint64) uint64 {
	var migBit uint64
	if migrating {
		migBit = bucketMigratingBit
	}

	// fps must be correctly shifted into the upper 35 bits (bits 29-63) prior to passing,
	// or we pass it raw and shift here. Let's assume fps is strictly the raw 35-bit block.
	return uint64(O&0x7F) |
		(uint64(H&0x7F) << 7) |
		(uint64(F&0x7F) << 14) |
		(uint64(P&0x7F) << 21) |
		migBit |
		((fps & 0x7FFFFFFFF) << 29)
}
