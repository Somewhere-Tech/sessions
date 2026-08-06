package backup

import (
	"crypto/cipher"
	"crypto/rand"
	"encoding/base32"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/somewhere-tech/sessions/runtime/internal/state"
	"golang.org/x/crypto/chacha20poly1305"
)

const (
	keySize             = chacha20poly1305.KeySize
	recoveryGroupCount  = 8
	recoveryGroupBytes  = keySize / recoveryGroupCount
	recoveryGroupLength = 7
)

var (
	recoveryEncoding = base32.StdEncoding.WithPadding(base32.NoPadding)
	// ErrWrongKeyOrCorruptedFile deliberately hides the underlying AEAD error,
	// which is not actionable and can expose implementation details.
	ErrWrongKeyOrCorruptedFile = errors.New("wrong key or corrupted file")
)

// KeySetup describes the local key used while enabling encrypted backups.
type KeySetup struct {
	RecoveryPhrase string
	Reused         bool
}

// KeyPath follows the platform user configuration root so the backup key lands
// beside backup.json on every host. Losing track of this file means every
// existing encrypted backup reads as ErrWrongKeyOrCorruptedFile.
func KeyPath(home string) string {
	return filepath.Join(state.UserConfigRootFor(home), "backup.key")
}

func keyPathForConfig(configPath string) string {
	return filepath.Join(filepath.Dir(configPath), "backup.key")
}

// LoadOrCreateKey returns the existing backup key or creates one from secure
// random bytes. It never replaces an existing key.
func LoadOrCreateKey(path string) ([]byte, bool, error) {
	key, err := ReadKey(path)
	if err == nil {
		if err := os.Chmod(path, 0o600); err != nil {
			return nil, false, fmt.Errorf("secure backup key %s: %w", path, err)
		}
		return key, false, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return nil, false, err
	}

	directory := filepath.Dir(path)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return nil, false, fmt.Errorf("create backup key directory: %w", err)
	}
	if err := os.Chmod(directory, 0o700); err != nil {
		return nil, false, fmt.Errorf("secure backup key directory: %w", err)
	}
	key = make([]byte, keySize)
	if _, err := io.ReadFull(rand.Reader, key); err != nil {
		return nil, false, fmt.Errorf("generate backup key: %w", err)
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if errors.Is(err, os.ErrExist) {
		key, readErr := ReadKey(path)
		if readErr != nil {
			return nil, false, readErr
		}
		return key, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("create backup key %s: %w", path, err)
	}
	created := true
	defer func() {
		if created {
			_ = os.Remove(path)
		}
	}()
	if _, err := file.Write(key); err != nil {
		_ = file.Close()
		return nil, false, fmt.Errorf("write backup key %s: %w", path, err)
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return nil, false, fmt.Errorf("sync backup key %s: %w", path, err)
	}
	if err := file.Close(); err != nil {
		return nil, false, fmt.Errorf("close backup key %s: %w", path, err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		return nil, false, fmt.Errorf("secure backup key %s: %w", path, err)
	}
	created = false
	return key, true, nil
}

func ReadKey(path string) ([]byte, error) {
	key, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read backup key %s: %w", path, err)
	}
	if len(key) != keySize {
		return nil, fmt.Errorf("invalid backup key %s: expected %d bytes", path, keySize)
	}
	return key, nil
}

// RecoveryPhrase formats a key as eight independently decodable base32
// groups. Each seven-character group represents exactly four key bytes.
func RecoveryPhrase(key []byte) (string, error) {
	if len(key) != keySize {
		return "", fmt.Errorf("invalid backup key: expected %d bytes", keySize)
	}
	groups := make([]string, 0, recoveryGroupCount)
	for offset := 0; offset < len(key); offset += recoveryGroupBytes {
		groups = append(groups, recoveryEncoding.EncodeToString(key[offset:offset+recoveryGroupBytes]))
	}
	return strings.Join(groups, " "), nil
}

// KeyFromRecoveryPhrase accepts the printed space-separated form, as well as
// lowercase input or hyphens in place of spaces.
func KeyFromRecoveryPhrase(phrase string) ([]byte, error) {
	groups := strings.Fields(strings.ReplaceAll(strings.TrimSpace(phrase), "-", " "))
	if len(groups) != recoveryGroupCount {
		return nil, fmt.Errorf("invalid recovery phrase: expected %d base32 groups", recoveryGroupCount)
	}
	key := make([]byte, 0, keySize)
	for index, group := range groups {
		if len(group) != recoveryGroupLength {
			return nil, fmt.Errorf("invalid recovery phrase: group %d must be %d characters", index+1, recoveryGroupLength)
		}
		decoded, err := recoveryEncoding.DecodeString(strings.ToUpper(group))
		if err != nil || len(decoded) != recoveryGroupBytes {
			return nil, fmt.Errorf("invalid recovery phrase: group %d is not valid base32", index+1)
		}
		key = append(key, decoded...)
	}
	return key, nil
}

// PAYLOAD IDENTITY, AND WHY THERE ARE TWO FORMATS
//
// XChaCha20-Poly1305 proves that a payload was sealed with this key and has not
// been altered. It proves nothing about which session the payload is. Sealed
// with no additional data, every encrypted backup is interchangeable with every
// other one under the same key: anyone who can write to the backup destination
// can copy session A's object over session B's, and B decrypts cleanly into A's
// conversation. Nothing in the ciphertext contradicts the swap, so a restore
// reports success while handing back the wrong conversation.
//
// A sealed payload therefore carries the name of the object it was written as,
// and that name is the AEAD's additional authenticated data. Moving a payload to
// another name no longer decrypts under that name: OpenFor rejects it by
// comparing the sealed-in identity with the identity the caller asked for.
//
// The identity is the storage path (BackupIdentity: project + remote path),
// which is exactly the thing an attacker overwrites, and is already visible as
// the object's own name in the store -- so storing it in the header in the clear
// leaks nothing that the destination did not already publish.
//
// Two formats exist and both are read, because an archive sealed before this
// existed must keep opening:
//
//	v1     "SESSENC1" | uint16 big-endian identity length | identity |
//	       24-byte nonce | ciphertext+tag, sealed with the whole header
//	       (magic, length and identity) as AAD.
//	legacy 24-byte nonce | ciphertext+tag, sealed with nil AAD.
//
// A legacy payload is recognised by exclusion: it does not begin with the eight
// ASCII bytes of identityMagic. A legacy payload begins with a uniformly random
// nonce, so the chance it opens with those bytes is 2^-64, and even that case is
// handled rather than mishandled -- the v1 read is attempted first and, when the
// AEAD rejects it, the legacy read is attempted on the untouched payload. The
// reverse confusion cannot happen: a v1 payload read as legacy would use the
// magic and identity as its nonce and fail authentication.
//
// Nothing rewrites or migrates an existing object. A session whose transcript
// has not changed is skipped by the push cache and keeps its legacy payload
// remotely for as long as it stays unchanged; a session that is pushed again is
// sealed v1. Both are readable, forever, with the same key.
const (
	identityMagic     = "SESSENC1"
	identityLengthLen = 2
	// maxIdentityBytes is far above any real storage path (project name plus
	// "sessions/<machine>/<tool>/<uuid>.jsonl.enc"). It exists so a corrupted
	// length field cannot make a reader address memory it does not have.
	maxIdentityBytes = 4096
)

// ErrIdentityMismatch reports a payload that is intact and sealed with this key
// but belongs to a different backup object than the caller asked for. Unlike
// ErrWrongKeyOrCorruptedFile this is actionable and names both identities: it
// means the object at that path is not the object the manifest says it is.
var ErrIdentityMismatch = errors.New("encrypted backup was sealed for a different path")

// Decrypted is one opened payload together with what the payload proves about
// itself.
type Decrypted struct {
	Plaintext []byte

	// Identity is the storage path this payload was sealed as, authenticated by
	// the AEAD. Empty for a legacy payload, which was sealed before payloads
	// carried one.
	Identity string

	// Legacy reports a payload sealed with no identity binding. It decrypted
	// correctly, but nothing in it proves which session it is: a caller that
	// needs that proof must treat a legacy payload as unverified rather than as
	// verified. It is reported instead of refused because refusing it would take
	// the user's existing archive away, which is a worse failure than the one
	// binding prevents.
	Legacy bool
}

// BackupIdentity is the single definition of a payload's identity, so the
// pusher that seals one and any reader that verifies one cannot disagree about
// its spelling. It is the object's full storage path.
func BackupIdentity(project, remotePath string) string {
	return strings.TrimSuffix(project, "/") + "/" + strings.TrimPrefix(remotePath, "/")
}

// EncryptFor seals plaintext with XChaCha20-Poly1305, binding the payload to the
// storage path it is being written to. See the format note above.
func EncryptFor(key []byte, identity string, plaintext []byte) ([]byte, error) {
	aead, err := chacha20poly1305.NewX(key)
	if err != nil {
		return nil, fmt.Errorf("invalid backup key: expected %d bytes", keySize)
	}
	if len(identity) > maxIdentityBytes {
		return nil, fmt.Errorf("backup identity is longer than %d bytes", maxIdentityBytes)
	}
	header := make([]byte, 0, len(identityMagic)+identityLengthLen+len(identity))
	header = append(header, identityMagic...)
	header = binary.BigEndian.AppendUint16(header, uint16(len(identity)))
	header = append(header, identity...)

	payload := make([]byte, len(header), len(header)+aead.NonceSize()+len(plaintext)+aead.Overhead())
	copy(payload, header)
	nonce := make([]byte, aead.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, fmt.Errorf("generate encryption nonce: %w", err)
	}
	payload = append(payload, nonce...)
	return aead.Seal(payload, nonce, plaintext, header), nil
}

// Encrypt seals a payload that is bound to nothing. Prefer EncryptFor wherever
// the destination is known, which is every path inside the pusher: an unbound
// payload can be moved over another one without detection.
func Encrypt(key, plaintext []byte) ([]byte, error) {
	return EncryptFor(key, "", plaintext)
}

// Open decrypts a payload in either format and reports what it proves about
// itself. It does not check the identity against anything -- OpenFor does that.
func Open(key, payload []byte) (Decrypted, error) {
	aead, err := chacha20poly1305.NewX(key)
	if err != nil {
		return Decrypted{}, fmt.Errorf("invalid backup key: expected %d bytes", keySize)
	}
	if identity, header, body, ok := splitIdentityPayload(payload); ok {
		if plaintext, err := openSealed(aead, body, header); err == nil {
			return Decrypted{Plaintext: plaintext, Identity: identity}, nil
		}
		// Fall through. Either the payload is damaged, or it is a legacy payload
		// whose random nonce happens to start with the magic bytes.
	}
	plaintext, err := openSealed(aead, payload, nil)
	if err != nil {
		return Decrypted{}, ErrWrongKeyOrCorruptedFile
	}
	return Decrypted{Plaintext: plaintext, Legacy: true}, nil
}

// OpenFor decrypts a payload that must be the object stored at identity. A v1
// payload sealed for any other path fails with ErrIdentityMismatch instead of
// returning somebody else's conversation.
//
// A legacy payload cannot be checked -- it was sealed before payloads carried a
// path -- so it is returned with Legacy set rather than refused. A caller that
// requires proof must reject Legacy itself; a caller restoring an old archive
// must not.
func OpenFor(key []byte, identity string, payload []byte) (Decrypted, error) {
	opened, err := Open(key, payload)
	if err != nil {
		return Decrypted{}, err
	}
	if opened.Legacy || opened.Identity == identity {
		return opened, nil
	}
	return Decrypted{}, fmt.Errorf("%w: sealed for %q, expected %q",
		ErrIdentityMismatch, opened.Identity, identity)
}

// Decrypt opens a payload of either format and returns just the plaintext. It
// answers "can this key read this file", which is what the recovery-phrase and
// CLI paths ask; use OpenFor when the caller knows which object it wants.
func Decrypt(key, payload []byte) ([]byte, error) {
	opened, err := Open(key, payload)
	if err != nil {
		return nil, err
	}
	return opened.Plaintext, nil
}

func openSealed(aead cipher.AEAD, body, additional []byte) ([]byte, error) {
	if len(body) < aead.NonceSize()+aead.Overhead() {
		return nil, ErrWrongKeyOrCorruptedFile
	}
	plaintext, err := aead.Open(nil, body[:aead.NonceSize()], body[aead.NonceSize():], additional)
	if err != nil {
		return nil, ErrWrongKeyOrCorruptedFile
	}
	return plaintext, nil
}

// splitIdentityPayload recognises the v1 framing. It reports ok only for a
// well-formed header; the AEAD, not this function, decides whether the bytes are
// genuine, so a legacy payload that happens to look like a header is still read
// correctly by the caller's fallback.
func splitIdentityPayload(payload []byte) (identity string, header, body []byte, ok bool) {
	prefix := len(identityMagic) + identityLengthLen
	if len(payload) < prefix || string(payload[:len(identityMagic)]) != identityMagic {
		return "", nil, nil, false
	}
	length := int(binary.BigEndian.Uint16(payload[len(identityMagic):prefix]))
	if length > maxIdentityBytes || len(payload) < prefix+length {
		return "", nil, nil, false
	}
	headerEnd := prefix + length
	return string(payload[prefix:headerEnd]), payload[:headerEnd], payload[headerEnd:], true
}

// DecryptFile decrypts any backup payload this key can open. It is the
// self-describing read: a payload carries its own identity, so a file that was
// downloaded by hand still decrypts without the caller knowing where it came
// from. Use DecryptFileFor when the caller knows which object it asked for.
func DecryptFile(inputPath, outputPath string, key []byte) error {
	_, err := decryptFile(inputPath, outputPath, key, "", false)
	return err
}

// DecryptFileFor decrypts a backup payload and requires it to be the object
// stored at identity, so a swapped or misfiled object fails loudly instead of
// writing the wrong conversation to outputPath. It returns what the payload
// proved: Legacy means the identity could not be checked because the payload
// predates identity binding.
func DecryptFileFor(inputPath, outputPath, identity string, key []byte) (Decrypted, error) {
	return decryptFile(inputPath, outputPath, key, identity, true)
}

func decryptFile(inputPath, outputPath string, key []byte, identity string, verify bool) (Decrypted, error) {
	payload, err := os.ReadFile(inputPath)
	if err != nil {
		return Decrypted{}, fmt.Errorf("read encrypted backup %s: %w", inputPath, err)
	}
	var opened Decrypted
	if verify {
		opened, err = OpenFor(key, identity, payload)
	} else {
		opened, err = Open(key, payload)
	}
	if err != nil {
		return Decrypted{}, err
	}
	if err := writePrivateFile(outputPath, opened.Plaintext); err != nil {
		return Decrypted{}, fmt.Errorf("write decrypted backup %s: %w", outputPath, err)
	}
	return Decrypted{Identity: opened.Identity, Legacy: opened.Legacy}, nil
}

func writePrivateFile(path string, contents []byte) error {
	directory := filepath.Dir(path)
	temporary, err := os.CreateTemp(directory, ".sessions-decrypt-*.tmp")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	removeTemporary := true
	defer func() {
		_ = temporary.Close()
		if removeTemporary {
			_ = os.Remove(temporaryPath)
		}
	}()
	if err := temporary.Chmod(0o600); err != nil {
		return err
	}
	if _, err := temporary.Write(contents); err != nil {
		return err
	}
	if err := temporary.Sync(); err != nil {
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	if err := os.Rename(temporaryPath, path); err != nil {
		return err
	}
	removeTemporary = false
	return os.Chmod(path, 0o600)
}
