//go:build windows

package tokenstore

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"unsafe"

	"golang.org/x/sys/windows"
)

const (
	protectedTokenVersion = 1
	maxProtectedTokenSize = 64 * 1024
)

type protectedToken struct {
	Version   int    `json:"version"`
	Protected string `json:"protected"`
}

func read(path string) (string, error) {
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		return "", nil
	} else if err != nil {
		return "", protectedReadError(path, err)
	}
	if err := protectPath(path); err != nil {
		return "", protectedReadError(path, err)
	}
	encoded, err := os.ReadFile(path)
	if err != nil {
		return "", protectedReadError(path, err)
	}
	if len(encoded) > maxProtectedTokenSize {
		return "", protectedReadError(path, errors.New("the file is unexpectedly large"))
	}

	legacy := strings.TrimSpace(string(encoded))
	if Valid(legacy) {
		if err := writeProtected(path, legacy); err != nil {
			return "", fmt.Errorf(
				"protect the legacy local daemon token at %s: %w; Sessions left the plaintext token unchanged",
				path,
				err,
			)
		}
		return legacy, nil
	}
	value, err := decodeProtected(path, encoded)
	if err != nil {
		return "", protectedReadError(path, err)
	}
	return value, nil
}

func readOrCreate(path string) (string, error) {
	value, err := read(path)
	if err != nil {
		return "", err
	}
	if value != "" {
		return value, nil
	}
	value, err = generate()
	if err != nil {
		return "", fmt.Errorf("generate the local daemon token: %w", err)
	}
	if err := writeProtected(path, value); err != nil {
		return "", fmt.Errorf("protect the new local daemon token: %w", err)
	}
	return value, nil
}

func protectedReadError(path string, err error) error {
	return fmt.Errorf(
		"unlock the protected local daemon token at %s for this signed-in Windows user: %w; reopen Sessions as the Windows user that created it, or preserve the file and remove it explicitly only if you intend to rotate master-token access",
		path,
		err,
	)
}

func writeProtected(path, value string) error {
	if !Valid(value) {
		return errors.New("refuse to protect an invalid local daemon token")
	}
	parent := filepath.Dir(path)
	if err := os.MkdirAll(parent, 0o700); err != nil {
		return fmt.Errorf("create the token directory: %w", err)
	}
	if err := applyOwnerACL(parent, true); err != nil {
		return err
	}

	protected, err := dpapiProtect(path, []byte(value))
	if err != nil {
		return err
	}
	envelope, err := json.Marshal(protectedToken{
		Version:   protectedTokenVersion,
		Protected: base64.StdEncoding.EncodeToString(protected),
	})
	if err != nil {
		return fmt.Errorf("encode the protected token: %w", err)
	}
	envelope = append(envelope, '\n')
	if len(envelope) > maxProtectedTokenSize {
		return errors.New("the protected token is unexpectedly large")
	}

	temporary, err := os.CreateTemp(parent, ".daemon-token-*.tmp")
	if err != nil {
		return fmt.Errorf("stage the protected token: %w", err)
	}
	temporaryPath := temporary.Name()
	keepTemporary := true
	defer func() {
		if keepTemporary {
			_ = os.Remove(temporaryPath)
		}
	}()
	if _, err := temporary.Write(envelope); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("write the staged protected token: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("flush the staged protected token: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("close the staged protected token: %w", err)
	}
	if err := applyOwnerACL(temporaryPath, false); err != nil {
		return err
	}
	staged, err := os.ReadFile(temporaryPath)
	if err != nil {
		return fmt.Errorf("read back the staged protected token: %w", err)
	}
	verified, err := decodeProtected(path, staged)
	if err != nil {
		return fmt.Errorf("verify the staged protected token: %w", err)
	}
	if verified != value {
		return errors.New("the staged protected token did not read back exactly")
	}
	if err := replaceFile(temporaryPath, path); err != nil {
		return fmt.Errorf("replace the local daemon token: %w", err)
	}
	keepTemporary = false
	return nil
}

func decodeProtected(entropyPath string, encoded []byte) (string, error) {
	var envelope protectedToken
	if err := json.Unmarshal(encoded, &envelope); err != nil {
		return "", fmt.Errorf("parse the protected token envelope: %w", err)
	}
	if envelope.Version != protectedTokenVersion {
		return "", fmt.Errorf("unsupported protected token version %d", envelope.Version)
	}
	protected, err := base64.StdEncoding.DecodeString(envelope.Protected)
	if err != nil || len(protected) == 0 {
		return "", errors.New("the protected token envelope contains invalid ciphertext")
	}
	plaintext, err := dpapiUnprotect(entropyPath, protected)
	if err != nil {
		return "", err
	}
	value := strings.TrimSpace(string(plaintext))
	if !Valid(value) {
		return "", errors.New("the unlocked local daemon token has an invalid shape")
	}
	return value, nil
}

func tokenEntropy(path string) []byte {
	normalized := strings.ToLower(filepath.Clean(path))
	value := append([]byte("Sessions local daemon token v1\x00"), []byte(normalized)...)
	return value
}

func dataBlob(value []byte) windows.DataBlob {
	if len(value) == 0 {
		return windows.DataBlob{}
	}
	return windows.DataBlob{Size: uint32(len(value)), Data: &value[0]}
}

func dpapiProtect(path string, plaintext []byte) ([]byte, error) {
	input := dataBlob(plaintext)
	entropyBytes := tokenEntropy(path)
	entropy := dataBlob(entropyBytes)
	var output windows.DataBlob
	if err := windows.CryptProtectData(
		&input,
		nil,
		&entropy,
		0,
		nil,
		windows.CRYPTPROTECT_UI_FORBIDDEN,
		&output,
	); err != nil {
		return nil, fmt.Errorf("protect the token with user-scope DPAPI: %w", err)
	}
	return copyAndFreeBlob(&output)
}

func dpapiUnprotect(path string, protected []byte) ([]byte, error) {
	input := dataBlob(protected)
	entropyBytes := tokenEntropy(path)
	entropy := dataBlob(entropyBytes)
	var output windows.DataBlob
	if err := windows.CryptUnprotectData(
		&input,
		nil,
		&entropy,
		0,
		nil,
		windows.CRYPTPROTECT_UI_FORBIDDEN,
		&output,
	); err != nil {
		return nil, fmt.Errorf("unlock the token with user-scope DPAPI: %w", err)
	}
	return copyAndFreeBlob(&output)
}

func copyAndFreeBlob(blob *windows.DataBlob) ([]byte, error) {
	if blob.Data == nil || blob.Size == 0 {
		if blob.Data != nil {
			_, _ = windows.LocalFree(windows.Handle(uintptr(unsafe.Pointer(blob.Data))))
		}
		return nil, errors.New("user-scope DPAPI returned empty data")
	}
	value := append([]byte(nil), unsafe.Slice(blob.Data, int(blob.Size))...)
	_, freeErr := windows.LocalFree(windows.Handle(uintptr(unsafe.Pointer(blob.Data))))
	if freeErr != nil {
		return nil, fmt.Errorf("release user-scope DPAPI output: %w", freeErr)
	}
	return value, nil
}

func applyOwnerACL(path string, directory bool) error {
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return fmt.Errorf("read the signed-in Windows user identity: %w", err)
	}
	inheritance := ""
	if directory {
		inheritance = "OICI"
	}
	descriptor, err := windows.SecurityDescriptorFromString(fmt.Sprintf(
		"D:P(A;%s;FA;;;SY)(A;%s;FA;;;%s)",
		inheritance,
		inheritance,
		user.User.Sid.String(),
	))
	if err != nil {
		return fmt.Errorf("build the local daemon-token access policy: %w", err)
	}
	dacl, _, err := descriptor.DACL()
	if err != nil {
		return fmt.Errorf("read the local daemon-token access policy: %w", err)
	}
	if dacl == nil {
		return errors.New("read the local daemon-token access policy: empty DACL")
	}
	if err := windows.SetNamedSecurityInfo(
		path,
		windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION,
		nil,
		nil,
		dacl,
		nil,
	); err != nil {
		return fmt.Errorf("restrict the local daemon-token access policy: %w", err)
	}
	return nil
}

func protectPath(path string) error {
	parent := filepath.Dir(path)
	if err := applyOwnerACL(parent, true); err != nil {
		return err
	}
	return applyOwnerACL(path, false)
}

func replaceFile(source, destination string) error {
	from, err := windows.UTF16PtrFromString(source)
	if err != nil {
		return err
	}
	to, err := windows.UTF16PtrFromString(destination)
	if err != nil {
		return err
	}
	return windows.MoveFileEx(
		from,
		to,
		windows.MOVEFILE_REPLACE_EXISTING|windows.MOVEFILE_WRITE_THROUGH,
	)
}
