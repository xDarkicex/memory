//go:build arm64
#include "textflag.h"

// func CpuPause()
TEXT ·CpuPause(SB), NOSPLIT, $0-0
	WORD $0xD503203F // YIELD instruction
	RET

// func Spin()
TEXT ·Spin(SB), NOSPLIT, $0-0
	WORD $0xD503203F // YIELD instruction
	RET
