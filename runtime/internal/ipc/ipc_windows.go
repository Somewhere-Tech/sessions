//go:build windows

package ipc

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net"
	"time"

	"github.com/Microsoft/go-winio"
	"golang.org/x/sys/windows"
)

func RunnerEndpoint(stateDirectory, id string) string {
	return `\\.\pipe\somewhere-sessions-` + endpointNamespace(stateDirectory) + "-" + id
}

func Listen(endpoint string) (net.Listener, error) {
	sid, err := currentUserSID()
	if err != nil {
		return nil, fmt.Errorf("resolve current Windows user for runner pipe: %w", err)
	}
	listener, err := winio.ListenPipe(endpoint, &winio.PipeConfig{
		// Protected DACL: only the signed-in owner receives access. go-winio
		// also requests FILE_PIPE_REJECT_REMOTE_CLIENTS and creates the first
		// instance atomically, closing the remote and name-squatting seams.
		SecurityDescriptor: "D:P(A;;GA;;;" + sid.String() + ")",
		InputBufferSize:    64 * 1024,
		OutputBufferSize:   64 * 1024,
	})
	if err != nil {
		return nil, err
	}
	return &sameUserListener{Listener: listener, sid: sid}, nil
}

func DialContext(ctx context.Context, endpoint string) (net.Conn, error) {
	connection, err := winio.DialPipeContext(ctx, endpoint)
	if err != nil {
		return nil, err
	}
	if err := validatePipePeer(connection, false, nil); err != nil {
		_ = connection.Close()
		return nil, err
	}
	return connection, nil
}

func DialTimeout(endpoint string, timeout time.Duration) (net.Conn, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	return DialContext(ctx, endpoint)
}

func Remove(string) error {
	// Named pipes are kernel objects and disappear when the listener closes.
	return nil
}

func MayExist(string) bool {
	// Unlike a Unix-domain socket, a named pipe has no filesystem marker.
	// Callers must attempt a bounded authenticated dial.
	return true
}

func HasFilesystemMarker() bool {
	return false
}

type sameUserListener struct {
	net.Listener
	sid *windows.SID
}

func (l *sameUserListener) Accept() (net.Conn, error) {
	for {
		connection, err := l.Listener.Accept()
		if err != nil {
			return nil, err
		}
		if err := validatePipePeer(connection, true, l.sid); err != nil {
			_ = connection.Close()
			continue
		}
		return connection, nil
	}
}

func validatePipePeer(connection net.Conn, client bool, expected *windows.SID) error {
	fileDescriptor, ok := connection.(interface{ Fd() uintptr })
	if !ok {
		return errorsNewPeer("named-pipe transport does not expose its handle")
	}
	var pid uint32
	var err error
	if client {
		err = windows.GetNamedPipeClientProcessId(windows.Handle(fileDescriptor.Fd()), &pid)
	} else {
		err = windows.GetNamedPipeServerProcessId(windows.Handle(fileDescriptor.Fd()), &pid)
	}
	if err != nil {
		return fmt.Errorf("resolve named-pipe peer process: %w", err)
	}
	peerSID, err := processUserSID(pid)
	if err != nil {
		return fmt.Errorf("resolve named-pipe peer identity: %w", err)
	}
	if expected == nil {
		expected, err = currentUserSID()
		if err != nil {
			return err
		}
	}
	if !peerSID.Equals(expected) {
		return fmt.Errorf("named-pipe peer pid %d belongs to a different Windows user", pid)
	}
	return nil
}

func currentUserSID() (*windows.SID, error) {
	token := windows.GetCurrentProcessToken()
	user, err := token.GetTokenUser()
	if err != nil {
		return nil, err
	}
	return user.User.Sid.Copy()
}

func processUserSID(pid uint32) (*windows.SID, error) {
	process, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, pid)
	if err != nil {
		return nil, err
	}
	defer windows.CloseHandle(process)
	var token windows.Token
	if err := windows.OpenProcessToken(process, windows.TOKEN_QUERY, &token); err != nil {
		return nil, err
	}
	defer token.Close()
	user, err := token.GetTokenUser()
	if err != nil {
		return nil, err
	}
	return user.User.Sid.Copy()
}

func endpointNamespace(stateDirectory string) string {
	sum := sha256.Sum256([]byte(stateDirectory))
	return hex.EncodeToString(sum[:6])
}

type pipePeerError string

func (e pipePeerError) Error() string { return string(e) }

func errorsNewPeer(message string) error { return pipePeerError(message) }
