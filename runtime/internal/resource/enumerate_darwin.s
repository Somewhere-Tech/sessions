// Assembly trampoline for one libSystem entry point, in the shape
// golang.org/x/sys/unix uses for every darwin syscall it wraps. The Go side
// declares proc_pidinfo with //go:cgo_import_dynamic; the linker resolves it
// at load time and this stub is the address the runtime's libc call path jumps
// through. Identical for amd64 and arm64, so one file covers both.

//go:build darwin

#include "textflag.h"

TEXT ·libcProcPidinfoTrampoline(SB),NOSPLIT,$0-0
	JMP	libc_proc_pidinfo(SB)

GLOBL	·libcProcPidinfoTrampolineAddr(SB), RODATA, $8
DATA	·libcProcPidinfoTrampolineAddr(SB)/8, $·libcProcPidinfoTrampoline(SB)
