//go:build windows

// Package winconpty isolates the one Windows ABI conversion required to pass
// an HPCON handle value through PROC_THREAD_ATTRIBUTE_PSEUDOCONSOLE.
package winconpty

import (
	"errors"
	"unsafe"

	"golang.org/x/sys/windows"
)

var updateProcThreadAttribute = windows.NewLazySystemDLL("kernel32.dll").NewProc("UpdateProcThreadAttribute")

// SetPseudoConsoleAttribute attaches a pseudoconsole to a child process
// attribute list. HPCON is a pointer-shaped Windows handle and the API expects
// that handle value directly as lpValue, not the address of the Go variable
// that stores it.
func SetPseudoConsoleAttribute(
	attributes *windows.ProcThreadAttributeListContainer,
	pseudoconsole windows.Handle,
) error {
	// ProcThreadAttributeListContainer.Update takes unsafe.Pointer, but HPCON
	// is the exceptional attribute whose value is already pointer-shaped. A
	// Go integer-to-pointer conversion expresses the Windows ABI correctly but
	// is rejected by vet's unsafeptr check. Call the same API with uintptrs so
	// no Go pointer is invented or retained.
	succeeded, _, callErr := updateProcThreadAttribute.Call(
		uintptr(unsafe.Pointer(attributes.List())),
		0,
		windows.PROC_THREAD_ATTRIBUTE_PSEUDOCONSOLE,
		uintptr(pseudoconsole),
		unsafe.Sizeof(pseudoconsole),
		0,
		0,
	)
	if succeeded == 0 {
		if callErr != nil && !errors.Is(callErr, windows.ERROR_SUCCESS) {
			return callErr
		}
		return errors.New("UpdateProcThreadAttribute failed without a Windows error")
	}
	return nil
}
