// Package proto implements the binary protocol spoken between sessionsd and a
// per-session runner. The TypeScript implementation in
// runtime/testdata/node-runtime/src/runnerProtocol.ts preserves the original wire-format authority.
package proto

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

const (
	// MaxFrameLen includes the one-byte frame type, but not the four-byte
	// length prefix.
	MaxFrameLen = 4 * 1024 * 1024

	// MaxStructuredReplayEvents bounds the hot structured-event window held
	// by both runners and the daemon. Provider history remains durable on disk;
	// this only prevents an old or long-running runner replay from making a
	// newly updated daemon retain an unbounded duplicate in memory.
	MaxStructuredReplayEvents = 1200

	// ProtocolVersion is the current daemon/runner wire revision. Version zero
	// means the legacy HELLO omitted protocolVersion; it remains supported while
	// immutable pre-1 runners are still alive.
	ProtocolVersion          = 2
	MinimumCompatibleVersion = 0
	MaximumCompatibleVersion = ProtocolVersion
)

func IsCompatibleVersion(version int) bool {
	return version >= MinimumCompatibleVersion && version <= MaximumCompatibleVersion
}

func IncompatibleVersionError(version int) error {
	return fmt.Errorf(
		"runner protocol v%d is incompatible with this daemon (supported v%d-v%d); update Sessions on the host, but do not stop the runner",
		version, MinimumCompatibleVersion, MaximumCompatibleVersion,
	)
}

// Type is the byte following a frame's length prefix.
type Type byte

const (
	Input       Type = 0x10
	Resize      Type = 0x11
	SnapshotReq Type = 0x12
	ReplayReq   Type = 0x13
	Kill        Type = 0x14
	ModelReq    Type = 0x15

	Hello       Type = 0x20
	Output      Type = 0x21
	Exit        Type = 0x22
	SnapshotRes Type = 0x23
	ReplayDone  Type = 0x24
	// Structured is a Go-runner extension carrying one normalized provider
	// event as JSON. Unknown frames remain safely ignored by older daemons.
	Structured Type = 0x25
	ModelRes   Type = 0x26
)

var (
	ErrFrameTooLarge  = errors.New("runner frame too large")
	ErrBadFrameLength = errors.New("bad runner frame length")
	ErrOutputTooShort = errors.New("OUTPUT frame too short")
)

// Frame is one decoded wire frame.
type Frame struct {
	Type    Type
	Payload []byte
}

// Encode returns a complete length-prefixed frame.
func Encode(typ Type, payload []byte) ([]byte, error) {
	frameLen := 1 + len(payload)
	if frameLen > MaxFrameLen {
		return nil, fmt.Errorf("%w: %d > %d", ErrFrameTooLarge, frameLen, MaxFrameLen)
	}
	out := make([]byte, 4+frameLen)
	binary.BigEndian.PutUint32(out[:4], uint32(frameLen))
	out[4] = byte(typ)
	copy(out[5:], payload)
	return out, nil
}

// MustEncode is for frames whose statically bounded payload cannot exceed the
// protocol limit.
func MustEncode(typ Type, payload []byte) []byte {
	b, err := Encode(typ, payload)
	if err != nil {
		panic(err)
	}
	return b
}

// EncodeOutput prefixes the UTF-8 terminal chunk with its uint32 sequence.
func EncodeOutput(seq uint32, data []byte) ([]byte, error) {
	payload := make([]byte, 4+len(data))
	binary.BigEndian.PutUint32(payload[:4], seq)
	copy(payload[4:], data)
	return Encode(Output, payload)
}

// DecodeOutput splits an OUTPUT payload into its sequence and terminal bytes.
func DecodeOutput(payload []byte) (uint32, []byte, error) {
	if len(payload) < 4 {
		return 0, nil, ErrOutputTooShort
	}
	return binary.BigEndian.Uint32(payload[:4]), payload[4:], nil
}

// Read reads one frame from a stream. io.ReadFull deliberately makes no
// assumptions about Unix-socket packet boundaries.
func Read(r io.Reader) (Frame, error) {
	var header [4]byte
	if _, err := io.ReadFull(r, header[:]); err != nil {
		return Frame{}, err
	}
	n := binary.BigEndian.Uint32(header[:])
	if n < 1 || n > MaxFrameLen {
		return Frame{}, fmt.Errorf("%w: %d", ErrBadFrameLength, n)
	}
	body := make([]byte, int(n))
	if _, err := io.ReadFull(r, body); err != nil {
		return Frame{}, err
	}
	return Frame{Type: Type(body[0]), Payload: body[1:]}, nil
}

// Write writes a complete frame, retrying short writes.
func Write(w io.Writer, typ Type, payload []byte) error {
	b, err := Encode(typ, payload)
	if err != nil {
		return err
	}
	for len(b) > 0 {
		n, err := w.Write(b)
		if err != nil {
			return err
		}
		if n == 0 {
			return io.ErrShortWrite
		}
		b = b[n:]
	}
	return nil
}
