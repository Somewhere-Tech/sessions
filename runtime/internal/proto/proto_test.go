package proto

import (
	"bytes"
	"encoding/binary"
	"errors"
	"testing"
)

func TestFrameRoundTripWithFragmentedReader(t *testing.T) {
	wire, err := Encode(Input, []byte("hello"))
	if err != nil {
		t.Fatal(err)
	}
	r := &oneByteReader{data: wire}
	f, err := Read(r)
	if err != nil {
		t.Fatal(err)
	}
	if f.Type != Input || !bytes.Equal(f.Payload, []byte("hello")) {
		t.Fatalf("unexpected frame: %#v", f)
	}
}

func TestOutputEncodingMatchesTypeScriptLayout(t *testing.T) {
	wire, err := EncodeOutput(0x01020304, []byte("x"))
	if err != nil {
		t.Fatal(err)
	}
	want := []byte{0, 0, 0, 6, byte(Output), 1, 2, 3, 4, 'x'}
	if !bytes.Equal(wire, want) {
		t.Fatalf("wire = %v, want %v", wire, want)
	}
	seq, data, err := DecodeOutput(wire[5:])
	if err != nil || seq != 0x01020304 || string(data) != "x" {
		t.Fatalf("decoded seq=%x data=%q err=%v", seq, data, err)
	}
}

func TestRejectsInvalidLengths(t *testing.T) {
	for _, n := range []uint32{0, MaxFrameLen + 1} {
		var b [4]byte
		binary.BigEndian.PutUint32(b[:], n)
		_, err := Read(bytes.NewReader(b[:]))
		if !errors.Is(err, ErrBadFrameLength) {
			t.Fatalf("length %d: err=%v", n, err)
		}
	}
}

func TestRunnerProtocolCompatibilityRangeKeepsLegacyWithoutAcceptingFutureFrames(t *testing.T) {
	for _, version := range []int{MinimumCompatibleVersion, ProtocolVersion} {
		if !IsCompatibleVersion(version) {
			t.Fatalf("version %d should be compatible", version)
		}
	}
	for _, version := range []int{-1, MaximumCompatibleVersion + 1} {
		if IsCompatibleVersion(version) {
			t.Fatalf("version %d should be incompatible", version)
		}
		if err := IncompatibleVersionError(version); err == nil {
			t.Fatalf("version %d produced no instructional error", version)
		}
	}
}

type oneByteReader struct {
	data []byte
}

func (r *oneByteReader) Read(p []byte) (int, error) {
	if len(r.data) == 0 {
		return 0, errors.New("unexpected EOF")
	}
	p[0] = r.data[0]
	r.data = r.data[1:]
	return 1, nil
}
