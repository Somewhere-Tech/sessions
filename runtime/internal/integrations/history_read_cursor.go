package integrations

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
)

const readRecordLimit = 8 << 20

var ErrReadCursor = errors.New("invalid conversation cursor; use the cursor returned by this conversation's read endpoint")
var ErrReadChanged = errors.New("conversation changed at the read position; keep the old cursor and explicitly read again without it to reconcile history")
var ErrReadRecordLarge = errors.New("conversation record exceeds 8 MiB; cursor was not advanced; use the raw transcript to inspect it")

// A cursor belongs to a provider conversation, not its replaceable runner.
// Anchors detect replacement at the boundary; they are not a whole-file audit.
type readCursor struct {
	Version      int    `json:"v"`
	Conversation string `json:"c"`
	Offset       int64  `json:"o"`
	Line         int    `json:"l"`
	Message      int    `json:"m"`
	Text         int    `json:"t"`
	AnchorOffset int64  `json:"a"`
	AnchorLength int    `json:"n"`
	AnchorHash   string `json:"h"`
	PrefixLength int    `json:"p"`
	PrefixHash   string `json:"f"`
}

func decodeReadCursor(value, conversation string) (readCursor, error) {
	cursor := readCursor{Version: 1, Conversation: conversation}
	if value == "" {
		return cursor, nil
	}
	if len(value) > 4096 {
		return cursor, ErrReadCursor
	}
	encoded, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil || json.Unmarshal(encoded, &cursor) != nil {
		return cursor, ErrReadCursor
	}
	if cursor.Version != 1 || cursor.Conversation != conversation || cursor.Offset < 0 ||
		cursor.Line < 0 || cursor.Message < 0 || cursor.Text < 0 || cursor.Text > readRecordLimit ||
		cursor.AnchorOffset < 0 || cursor.AnchorOffset > cursor.Offset || cursor.AnchorLength < 0 ||
		cursor.AnchorLength > readRecordLimit || cursor.PrefixLength < 0 || cursor.PrefixLength > 256 {
		return cursor, ErrReadCursor
	}
	return cursor, nil
}

func (c readCursor) encode() string {
	encoded, _ := json.Marshal(c)
	return base64.RawURLEncoding.EncodeToString(encoded)
}

func readHash(value []byte) string { return fmt.Sprintf("%x", sha256.Sum256(value)) }

func (c *readCursor) validate(file *os.File) error {
	info, err := file.Stat()
	if err != nil {
		return err
	}
	if c.Offset > info.Size() {
		return ErrReadChanged
	}
	for _, anchor := range []struct {
		offset int64
		length int
		hash   string
	}{
		{0, c.PrefixLength, c.PrefixHash}, {c.AnchorOffset, c.AnchorLength, c.AnchorHash},
	} {
		if anchor.length == 0 {
			continue
		}
		value := make([]byte, anchor.length)
		if _, err := file.ReadAt(value, anchor.offset); err != nil {
			return ErrReadChanged
		}
		if readHash(value) != anchor.hash {
			return ErrReadChanged
		}
	}
	if c.PrefixLength == 0 && info.Size() > 0 {
		c.PrefixLength = int(min(info.Size(), 256))
		value := make([]byte, c.PrefixLength)
		if _, err := io.ReadFull(io.NewSectionReader(file, 0, int64(c.PrefixLength)), value); err != nil {
			return err
		}
		c.PrefixHash = readHash(value)
	}
	return nil
}

func (c *readCursor) anchor(line []byte) {
	c.AnchorOffset, c.AnchorLength, c.AnchorHash = c.Offset, len(line), readHash(line)
}
