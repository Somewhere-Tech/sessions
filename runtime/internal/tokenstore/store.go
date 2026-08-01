package tokenstore

import (
	"crypto/rand"
	"encoding/hex"
)

// Read returns the existing local daemon token without creating one.
func Read(path string) (string, error) {
	return read(path)
}

// ReadOrCreate returns the existing local daemon token or creates one through
// the platform's protected storage boundary.
func ReadOrCreate(path string) (string, error) {
	return readOrCreate(path)
}

// ReadSecret returns a previously stored per-device credential. Unlike Read,
// it accepts the opaque token shapes issued by pairing rather than requiring
// the daemon's fixed master-token shape.
func ReadSecret(path string) (string, error) {
	return readSecret(path)
}

// WriteSecret durably stores an opaque per-device credential through the same
// platform protection boundary used by Sessions' local authentication state.
func WriteSecret(path, value string) error {
	return writeSecret(path, value)
}

// Valid reports whether value is the daemon's stable 32-byte hex token shape.
func Valid(value string) bool {
	if len(value) != 64 {
		return false
	}
	for _, char := range value {
		if (char < '0' || char > '9') && (char < 'a' || char > 'f') {
			return false
		}
	}
	return true
}

func generate() (string, error) {
	value := make([]byte, 32)
	if _, err := rand.Read(value); err != nil {
		return "", err
	}
	return hex.EncodeToString(value), nil
}
