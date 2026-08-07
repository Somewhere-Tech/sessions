//go:build !darwin && !linux && !windows

package resource

import "errors"

// Every platform Sessions ships a runtime for -- macOS, Linux and Windows --
// has a real enumerator beside this file. This build exists so the package
// compiles anywhere Go does, and it refuses rather than inventing an answer:
// an empty process table would make every session report a tree of zero
// processes, which readers render as unknown but which would silently look
// like a machine holding nothing at all.
var errUnsupportedPlatform = errors.New("per-session resource sampling is not implemented on this platform")

type unsupportedEnumerator struct{}

// SystemEnumerator returns the enumerator for this platform.
func SystemEnumerator() Enumerator { return unsupportedEnumerator{} }

func (unsupportedEnumerator) Enumerate() ([]Process, error) { return nil, errUnsupportedPlatform }
