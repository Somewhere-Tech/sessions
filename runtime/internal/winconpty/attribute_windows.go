//go:build windows

// Package winconpty isolates the one Windows ABI conversion required to pass
// an HPCON handle value through PROC_THREAD_ATTRIBUTE_PSEUDOCONSOLE.
package winconpty

import (
	"unsafe"

	"golang.org/x/sys/windows"
)

// SetPseudoConsoleAttribute attaches a pseudoconsole to a child process
// attribute list. HPCON is a pointer-shaped Windows handle and the API expects
// that handle value directly as lpValue, not the address of the Go variable
// that stores it.
func SetPseudoConsoleAttribute(
	attributes *windows.ProcThreadAttributeListContainer,
	pseudoconsole windows.Handle,
) error {
	return attributes.Update(
		windows.PROC_THREAD_ATTRIBUTE_PSEUDOCONSOLE,
		unsafe.Pointer(pseudoconsole),
		unsafe.Sizeof(pseudoconsole),
	)
}
