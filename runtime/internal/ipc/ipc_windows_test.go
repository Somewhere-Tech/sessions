//go:build windows

package ipc

import (
	"context"
	"fmt"
	"net"
	"os"
	"testing"
	"time"
	"unsafe"

	"github.com/Microsoft/go-winio"
	"golang.org/x/sys/windows"
)

const filePipeLocalInformationClass = 24

type filePipeLocalInformation struct {
	NamedPipeType       uint32
	NamedPipeConfig     uint32
	MaximumInstances    uint32
	CurrentInstances    uint32
	InboundQuota        uint32
	ReadDataAvailable   uint32
	OutboundQuota       uint32
	WriteQuotaAvailable uint32
	NamedPipeState      uint32
	NamedPipeEnd        uint32
}

func TestWindowsNamedPipeSecurityContract(t *testing.T) {
	endpoint := windowsTestEndpoint(t, "security")
	listener, err := Listen(endpoint)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()

	accepted := make(chan struct {
		connection net.Conn
		err        error
	}, 1)
	go func() {
		connection, err := listener.Accept()
		accepted <- struct {
			connection net.Conn
			err        error
		}{connection: connection, err: err}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	client, err := DialContext(ctx, endpoint)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	clientFile, ok := client.(interface{ Fd() uintptr })
	if !ok {
		t.Fatal("dialed named-pipe connection has no Windows handle")
	}

	var server net.Conn
	select {
	case result := <-accepted:
		if result.err != nil {
			t.Fatal(result.err)
		}
		server = result.connection
	case <-time.After(5 * time.Second):
		t.Fatal("timed out accepting authenticated named-pipe connection")
	}
	defer server.Close()
	serverFile, ok := server.(interface{ Fd() uintptr })
	if !ok {
		t.Fatal("accepted named-pipe connection has no Windows handle")
	}

	ownerSID, err := currentUserSID()
	if err != nil {
		t.Fatal(err)
	}
	serverHandle := windows.Handle(serverFile.Fd())
	assertPipeOwnerDACL(t, serverHandle, ownerSID)
	assertPipeLocalOnly(t, serverHandle)
	assertPipePeers(t, serverHandle, windows.Handle(clientFile.Fd()), ownerSID)
}

func TestWindowsNamedPipeRejectsSquattedName(t *testing.T) {
	endpoint := windowsTestEndpoint(t, "squat")
	ownerSID, err := currentUserSID()
	if err != nil {
		t.Fatal(err)
	}
	squatter, err := winio.ListenPipe(endpoint, &winio.PipeConfig{
		SecurityDescriptor: "D:P(A;;GA;;;" + ownerSID.String() + ")",
	})
	if err != nil {
		t.Fatal(err)
	}

	if listener, err := Listen(endpoint); err == nil {
		_ = listener.Close()
		_ = squatter.Close()
		t.Fatal("Sessions opened a listener after another process claimed the pipe name")
	}
	if err := squatter.Close(); err != nil {
		t.Fatal(err)
	}

	listener, err := Listen(endpoint)
	if err != nil {
		t.Fatalf("pipe name did not become available after the squatter closed: %v", err)
	}
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
}

func windowsTestEndpoint(t *testing.T, purpose string) string {
	t.Helper()
	return RunnerEndpoint(
		t.TempDir(),
		fmt.Sprintf("ipc-%s-%d-%d", purpose, os.Getpid(), time.Now().UnixNano()),
	)
}

func assertPipeOwnerDACL(t *testing.T, handle windows.Handle, ownerSID *windows.SID) {
	t.Helper()
	descriptor, err := windows.GetSecurityInfo(
		handle,
		windows.SE_KERNEL_OBJECT,
		windows.DACL_SECURITY_INFORMATION,
	)
	if err != nil {
		t.Fatalf("read named-pipe DACL: %v", err)
	}
	control, _, err := descriptor.Control()
	if err != nil {
		t.Fatalf("read named-pipe security control: %v", err)
	}
	if control&windows.SE_DACL_PROTECTED == 0 {
		t.Fatal("named-pipe DACL inherits access instead of remaining protected")
	}
	dacl, _, err := descriptor.DACL()
	if err != nil {
		t.Fatalf("read named-pipe access list: %v", err)
	}
	if dacl == nil {
		t.Fatal("named-pipe DACL is empty; want exactly one owner entry")
	}
	if dacl.AceCount != 1 {
		t.Fatalf("named-pipe DACL has %d entries; want exactly one owner entry", dacl.AceCount)
	}
	var ace *windows.ACCESS_ALLOWED_ACE
	if err := windows.GetAce(dacl, 0, &ace); err != nil {
		t.Fatalf("read named-pipe owner access entry: %v", err)
	}
	if ace.Header.AceType != windows.ACCESS_ALLOWED_ACE_TYPE {
		t.Fatalf("named-pipe owner entry type = %d; want allow", ace.Header.AceType)
	}
	// GetSecurityInfo returns the object-specific mask after Windows maps the
	// GA generic right from the SDDL onto named-pipe/file rights.
	const fileAllAccess = windows.STANDARD_RIGHTS_REQUIRED | windows.SYNCHRONIZE | 0x1ff
	if ace.Mask != fileAllAccess {
		t.Fatalf("named-pipe owner access = %#x; want mapped FILE_ALL_ACCESS %#x", ace.Mask, fileAllAccess)
	}
	aceSID := (*windows.SID)(unsafe.Pointer(&ace.SidStart))
	if !aceSID.Equals(ownerSID) {
		t.Fatalf("named-pipe access belongs to %s; want current user %s", aceSID, ownerSID)
	}
}

func assertPipeLocalOnly(t *testing.T, handle windows.Handle) {
	t.Helper()
	var status windows.IO_STATUS_BLOCK
	var information filePipeLocalInformation
	if err := windows.NtQueryInformationFile(
		handle,
		&status,
		(*byte)(unsafe.Pointer(&information)),
		uint32(unsafe.Sizeof(information)),
		filePipeLocalInformationClass,
	); err != nil {
		t.Fatalf("query named-pipe local policy: %v", err)
	}
	if information.NamedPipeType&windows.FILE_PIPE_REJECT_REMOTE_CLIENTS == 0 {
		t.Fatalf(
			"named-pipe type %#x accepts remote clients; want FILE_PIPE_REJECT_REMOTE_CLIENTS",
			information.NamedPipeType,
		)
	}
	if information.NamedPipeEnd != windows.FILE_PIPE_SERVER_END {
		t.Fatalf("inspected named-pipe end = %d; want server end", information.NamedPipeEnd)
	}
}

func assertPipePeers(
	t *testing.T,
	serverHandle windows.Handle,
	clientHandle windows.Handle,
	ownerSID *windows.SID,
) {
	t.Helper()
	var clientPID uint32
	if err := windows.GetNamedPipeClientProcessId(serverHandle, &clientPID); err != nil {
		t.Fatalf("read named-pipe client pid: %v", err)
	}
	var serverPID uint32
	if err := windows.GetNamedPipeServerProcessId(clientHandle, &serverPID); err != nil {
		t.Fatalf("read named-pipe server pid: %v", err)
	}
	currentPID := uint32(os.Getpid())
	if clientPID != currentPID || serverPID != currentPID {
		t.Fatalf(
			"named-pipe peers = client %d, server %d; want test process %d",
			clientPID,
			serverPID,
			currentPID,
		)
	}
	for role, pid := range map[string]uint32{"client": clientPID, "server": serverPID} {
		sid, err := processUserSID(pid)
		if err != nil {
			t.Fatalf("read named-pipe %s SID: %v", role, err)
		}
		if !sid.Equals(ownerSID) {
			t.Fatalf("named-pipe %s SID = %s; want current user %s", role, sid, ownerSID)
		}
	}
}
