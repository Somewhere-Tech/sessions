package backup

import (
	"bytes"
	"crypto/rand"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/crypto/chacha20poly1305"
)

// sealLegacy reproduces exactly what Encrypt did before payloads carried an
// identity: nonce, then ciphertext sealed with nil additional data. It is
// written out here rather than called through the package so the compatibility
// test is a test against the old format itself, not against a helper that could
// drift with the code it is meant to outlive.
func sealLegacy(t *testing.T, key, plaintext []byte) []byte {
	t.Helper()
	aead, err := chacha20poly1305.NewX(key)
	if err != nil {
		t.Fatal(err)
	}
	nonce := make([]byte, aead.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		t.Fatal(err)
	}
	return aead.Seal(nonce, nonce, plaintext, nil)
}

func fixtureKey(t *testing.T) []byte {
	t.Helper()
	key := make([]byte, keySize)
	for index := range key {
		key[index] = byte(index * 7)
	}
	return key
}

// A payload sealed for one session must not decrypt as another. This is the
// rename attack: whoever can write to the backup destination copies session A's
// object over session B's, and before identity binding B restored A's
// conversation with no sign anything was wrong.
func TestSealedPayloadDoesNotOpenUnderAnotherSessionsPath(t *testing.T) {
	key := fixtureKey(t)
	pathA := BackupIdentity("fixture-project", "sessions/mac/claude/session-a.jsonl.enc")
	pathB := BackupIdentity("fixture-project", "sessions/mac/claude/session-b.jsonl.enc")
	conversationA := []byte(`{"type":"user","message":"session A private text"}` + "\n")
	conversationB := []byte(`{"type":"user","message":"session B private text"}` + "\n")

	sealedA, err := EncryptFor(key, pathA, conversationA)
	if err != nil {
		t.Fatal(err)
	}
	sealedB, err := EncryptFor(key, pathB, conversationB)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(sealedA, conversationA) {
		t.Fatal("sealed payload contains plaintext")
	}

	openedA, err := OpenFor(key, pathA, sealedA)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(openedA.Plaintext, conversationA) || openedA.Identity != pathA || openedA.Legacy {
		t.Fatalf("opened = %#v", openedA)
	}

	// The swap: A's bytes are now stored at B's path.
	opened, err := OpenFor(key, pathB, sealedA)
	if !errors.Is(err, ErrIdentityMismatch) {
		t.Fatalf("swapped payload error = %v, want ErrIdentityMismatch", err)
	}
	if opened.Plaintext != nil {
		t.Fatal("swapped payload returned plaintext")
	}
	if openedB, err := OpenFor(key, pathB, sealedB); err != nil || !bytes.Equal(openedB.Plaintext, conversationB) {
		t.Fatalf("unswapped payload = %#v, %v", openedB, err)
	}

	// Editing the stored identity to the one being attacked does not help: the
	// whole header is the AEAD's additional data, so the ciphertext no longer
	// authenticates and the payload fails closed.
	forged := bytes.Replace(sealedA, []byte("session-a.jsonl.enc"), []byte("session-b.jsonl.enc"), 1)
	if len(forged) != len(sealedA) || bytes.Equal(forged, sealedA) {
		t.Fatal("forged fixture was not built")
	}
	if _, err := OpenFor(key, pathB, forged); !errors.Is(err, ErrWrongKeyOrCorruptedFile) {
		t.Fatalf("forged identity error = %v, want ErrWrongKeyOrCorruptedFile", err)
	}

	// A wrong key still reports the same non-actionable error it always did.
	wrongKey := bytes.Repeat([]byte{0xfe}, keySize)
	if _, err := Open(wrongKey, sealedA); !errors.Is(err, ErrWrongKeyOrCorruptedFile) {
		t.Fatalf("wrong-key error = %v", err)
	}
}

// An archive sealed before identity binding existed must still open, or the fix
// has destroyed the user's backups. A legacy payload is recognised by not
// carrying the v1 magic, and is reported as unverified rather than as verified.
func TestLegacyPayloadSealedWithNilAADStillDecrypts(t *testing.T) {
	key := fixtureKey(t)
	plaintext := []byte("conversation sealed by an older Sessions build\n")
	legacy := sealLegacy(t, key, plaintext)

	if bytes.HasPrefix(legacy, []byte(identityMagic)) {
		t.Fatal("legacy fixture must not carry the v1 magic")
	}
	decrypted, err := Decrypt(key, legacy)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(decrypted, plaintext) {
		t.Fatalf("legacy decrypt = %q, want %q", decrypted, plaintext)
	}
	opened, err := Open(key, legacy)
	if err != nil {
		t.Fatal(err)
	}
	if !opened.Legacy || opened.Identity != "" {
		t.Fatalf("legacy open = %#v, want Legacy with no identity", opened)
	}
	// A legacy payload carries no proof of which session it is, so OpenFor
	// returns it rather than refusing it -- refusing would take the existing
	// archive away -- and says so through Legacy.
	forPath, err := OpenFor(key, BackupIdentity("fixture-project", "sessions/mac/claude/any.jsonl.enc"), legacy)
	if err != nil || !forPath.Legacy || !bytes.Equal(forPath.Plaintext, plaintext) {
		t.Fatalf("legacy OpenFor = %#v, %v", forPath, err)
	}

	// A truncated legacy payload is still a wrong-key-or-corrupt answer, not a
	// panic and not a silent empty plaintext.
	if _, err := Open(key, legacy[:8]); !errors.Is(err, ErrWrongKeyOrCorruptedFile) {
		t.Fatalf("truncated legacy error = %v", err)
	}
}

// DecryptFile is the CLI entry point and keeps working for both formats without
// being told anything about where the file came from; DecryptFileFor is the
// verifying form for callers that do know.
func TestDecryptFileReadsBothFormatsAndVerifiesWhenAsked(t *testing.T) {
	key := fixtureKey(t)
	root := t.TempDir()
	identity := BackupIdentity("fixture-project", "sessions/mac/claude/session-a.jsonl.enc")
	plaintext := []byte("{\"type\":\"user\"}\n")

	sealed, err := EncryptFor(key, identity, plaintext)
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name    string
		payload []byte
		legacy  bool
	}{
		{name: "v1", payload: sealed},
		{name: "legacy", payload: sealLegacy(t, key, plaintext), legacy: true},
	}
	for _, testCase := range cases {
		inputPath := filepath.Join(root, testCase.name+".jsonl.enc")
		outputPath := filepath.Join(root, testCase.name+".jsonl")
		if err := os.WriteFile(inputPath, testCase.payload, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := DecryptFile(inputPath, outputPath, key); err != nil {
			t.Fatalf("%s DecryptFile: %v", testCase.name, err)
		}
		decrypted, err := os.ReadFile(outputPath)
		if err != nil || !bytes.Equal(decrypted, plaintext) {
			t.Fatalf("%s decrypted = %q, %v", testCase.name, decrypted, err)
		}
		opened, err := DecryptFileFor(inputPath, outputPath, identity, key)
		if err != nil {
			t.Fatalf("%s DecryptFileFor: %v", testCase.name, err)
		}
		if opened.Legacy != testCase.legacy {
			t.Fatalf("%s legacy = %v, want %v", testCase.name, opened.Legacy, testCase.legacy)
		}
	}

	// The verifying form refuses to write the wrong conversation out.
	swappedPath := filepath.Join(root, "swapped.jsonl.enc")
	outputPath := filepath.Join(root, "swapped.jsonl")
	if err := os.WriteFile(swappedPath, sealed, 0o600); err != nil {
		t.Fatal(err)
	}
	other := BackupIdentity("fixture-project", "sessions/mac/claude/session-b.jsonl.enc")
	if _, err := DecryptFileFor(swappedPath, outputPath, other, key); !errors.Is(err, ErrIdentityMismatch) {
		t.Fatalf("swapped DecryptFileFor error = %v, want ErrIdentityMismatch", err)
	}
	if _, err := os.Stat(outputPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("swapped payload wrote %s", outputPath)
	}
}
