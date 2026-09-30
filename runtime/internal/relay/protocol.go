package relay

import (
	"encoding/binary"
	"errors"
)

const (
	MaxPayload = 64 * 1024
	headerSize = 5
	frameOpen  = byte(1)
	frameData  = byte(2)
	frameEOF   = byte(3)
	frameClose = byte(4)
)

type frame struct {
	kind     byte
	streamID uint32
	payload  []byte
}

func encodeFrame(kind byte, streamID uint32, payload []byte) ([]byte, error) {
	if len(payload) > MaxPayload {
		return nil, errors.New("relay frame exceeds 64 KiB")
	}
	encoded := make([]byte, headerSize+len(payload))
	encoded[0] = kind
	binary.BigEndian.PutUint32(encoded[1:headerSize], streamID)
	copy(encoded[headerSize:], payload)
	return encoded, nil
}

func decodeFrame(encoded []byte) (frame, error) {
	if len(encoded) < headerSize || len(encoded)-headerSize > MaxPayload {
		return frame{}, errors.New("invalid relay frame size")
	}
	kind := encoded[0]
	if kind < frameOpen || kind > frameClose {
		return frame{}, errors.New("invalid relay frame type")
	}
	streamID := binary.BigEndian.Uint32(encoded[1:headerSize])
	if streamID == 0 || kind != frameData && len(encoded) != headerSize {
		return frame{}, errors.New("invalid relay control frame")
	}
	return frame{kind: kind, streamID: streamID, payload: encoded[headerSize:]}, nil
}
