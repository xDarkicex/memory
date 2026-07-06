//go:build amd64
#include "textflag.h"

// func CpuPause()
TEXT ·CpuPause(SB), NOSPLIT, $0-0
	PAUSE
	RET

// func Spin()
TEXT ·Spin(SB), NOSPLIT, $0-0
	PAUSE
	RET
