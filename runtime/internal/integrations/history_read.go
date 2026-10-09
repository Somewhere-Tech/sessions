package integrations

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"unicode/utf8"

	"github.com/somewhere-tech/sessions/runtime/internal/state"
	"github.com/somewhere-tech/sessions/runtime/internal/watch"
)

// ReadMessage may be a fragment of a large message. byte_offset and continued
// make this explicit; the cursor never consumes the part not returned.
type ReadMessage struct {
	Role       string  `json:"role"`
	Text       string  `json:"text"`
	Timestamp  *string `json:"timestamp"`
	ByteOffset int     `json:"byte_offset,omitempty"`
	Continued  bool    `json:"continued,omitempty"`
}

type ReadResponse struct {
	Conversation   string        `json:"conversation"`
	Messages       []ReadMessage `json:"messages"`
	NextCursor     string        `json:"next_cursor"`
	HasMore        bool          `json:"has_more"`
	PendingRecord  bool          `json:"pending_record,omitempty"`
	SkippedRecords int           `json:"skipped_records,omitempty"`
	MirrorDamaged  bool          `json:"mirror_damaged,omitempty"`
	MirrorDetail   string        `json:"mirror_detail,omitempty"`
}

// ReadConversation is deliberately stateless. Reading, retrying, and two
// concurrent readers cannot acknowledge or consume one another's messages.
func (s *Service) ReadConversation(ctx context.Context, live []state.SessionInfo, id, token string, limit int) (ReadResponse, error) {
	source, err := s.history.Source(live, id)
	if err != nil {
		return ReadResponse{}, err
	}
	if source.SourcePath == "" || !source.TextAvailable {
		return ReadResponse{}, ErrHistoryNotFound
	}
	identity := source.Session.ProviderSessionID
	if identity == "" {
		identity = source.Session.ID
	}
	identity = source.Session.Tool + ":" + identity
	page, err := readConversationFile(ctx, source.SourcePath, source.Session.Tool, identity, token, limit)
	page.MirrorDamaged, page.MirrorDetail = source.MirrorDamaged, source.MirrorDetail
	return page, err
}

func readConversationFile(ctx context.Context, path, tool, identity, token string, limit int) (ReadResponse, error) {
	page := ReadResponse{Conversation: identity, Messages: []ReadMessage{}}
	if limit < 1 || limit > 100 {
		return page, errors.New("read limit must be between 1 and 100")
	}
	cursor, err := decodeReadCursor(token, identity)
	if err != nil {
		return page, err
	}
	file, err := os.Open(path)
	if err != nil {
		return page, err
	}
	defer file.Close()
	if err := cursor.validate(file); err != nil {
		return page, err
	}
	// One page never follows a continuously growing writer indefinitely.
	info, err := file.Stat()
	if err != nil {
		return page, err
	}
	reader := bufio.NewReader(io.NewSectionReader(file, cursor.Offset, info.Size()-cursor.Offset))
	bytesRead, textBytes := 0, 0
	for len(page.Messages) < limit && bytesRead < readRecordLimit && textBytes <= (64<<10)-4 {
		if err := ctx.Err(); err != nil {
			return page, err
		}
		line, err := readBoundedRecord(reader)
		if errors.Is(err, io.EOF) {
			page.PendingRecord = len(line) > 0
			break // Do not consume a provider record still being written.
		}
		if err != nil {
			return page, err
		}
		bytesRead += len(line)
		cursor.anchor(line)
		messages, valid := readableRecord(line, path, tool, cursor.Line)
		if !valid {
			page.SkippedRecords++
		}
		if cursor.Message > len(messages) {
			return page, ErrReadChanged
		}
		for cursor.Message < len(messages) && len(page.Messages) < limit && textBytes <= (64<<10)-4 {
			part, err := readMessagePart(messages[cursor.Message], &cursor, (64<<10)-textBytes)
			if err != nil {
				return page, err
			}
			page.Messages = append(page.Messages, part)
			textBytes += len(part.Text)
		}
		if cursor.Message < len(messages) {
			break
		}
		cursor.Offset += int64(len(line))
		cursor.Line++
		cursor.Message, cursor.Text = 0, 0
	}
	page.HasMore = cursor.Offset < info.Size() && !page.PendingRecord
	page.NextCursor = cursor.encode()
	return page, nil
}

func readBoundedRecord(reader *bufio.Reader) ([]byte, error) {
	line := make([]byte, 0)
	for {
		part, err := reader.ReadSlice('\n')
		if len(line)+len(part) > readRecordLimit {
			return nil, ErrReadRecordLarge
		}
		line = append(line, part...)
		if !errors.Is(err, bufio.ErrBufferFull) {
			return line, err
		}
	}
}

func readableRecord(line []byte, path, tool string, lineIndex int) ([]TranscriptMessage, bool) {
	var event map[string]any
	if json.Unmarshal(line, &event) != nil {
		return nil, false
	}
	events := []map[string]any{event}
	if tool == "codex" {
		events = nil
		normalized := watch.NormalizeCodexRolloutLine(event, watch.CodexNormalizeContext{
			RolloutBasename: filepath.Base(path), LineIndex: lineIndex,
		})
		for _, value := range normalized.Events {
			events = append(events, value)
		}
	}
	var messages []TranscriptMessage
	for _, event := range events {
		for _, message := range transcriptMessages(event, map[string]string{}) {
			if message.Role == "user" || message.Role == "assistant" || message.Role == "error" {
				messages = append(messages, message)
			}
		}
	}
	return messages, true
}

func readMessagePart(message TranscriptMessage, cursor *readCursor, budget int) (ReadMessage, error) {
	if cursor.Text > len(message.Text) {
		return ReadMessage{}, ErrReadChanged
	}
	end := min(len(message.Text), cursor.Text+budget)
	for end < len(message.Text) && !utf8.RuneStart(message.Text[end]) {
		end--
	}
	// Leave the code point for the next page rather than emit invalid UTF-8.
	if end == cursor.Text {
		return ReadMessage{}, errors.New("page byte budget cannot hold the next character")
	}
	part := ReadMessage{Role: message.Role, Text: message.Text[cursor.Text:end],
		Timestamp: message.Timestamp, ByteOffset: cursor.Text, Continued: end < len(message.Text)}
	cursor.Text = end
	if !part.Continued {
		cursor.Message++
		cursor.Text = 0
	}
	return part, nil
}
