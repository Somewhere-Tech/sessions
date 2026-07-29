package api

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/somewhere-tech/sessions/runtime/internal/integrations"
	"github.com/somewhere-tech/sessions/runtime/internal/ledger"
)

const (
	relayClockLead = 5 * time.Second
	relayMatchAge  = 2 * time.Minute
)

type relayMatcher struct {
	relays   []ledger.MessageRelayed
	consumed []bool
}

func newRelayMatcher(relays []ledger.MessageRelayed) *relayMatcher {
	values := append([]ledger.MessageRelayed(nil), relays...)
	sort.SliceStable(values, func(i, j int) bool {
		if values[i].AtMS != values[j].AtMS {
			return values[i].AtMS < values[j].AtMS
		}
		return values[i].EventID < values[j].EventID
	})
	return &relayMatcher{relays: values, consumed: make([]bool, len(values))}
}

func (m *relayMatcher) match(text string, atMS int64) *ledger.MessageAuthor {
	if atMS <= 0 || text == "" {
		return nil
	}
	exact := sha256.Sum256([]byte(text))
	normalizedText := strings.TrimSpace(text)
	normalized := sha256.Sum256([]byte(normalizedText))
	exactHex := fmt.Sprintf("%x", exact[:])
	normalizedHex := fmt.Sprintf("%x", normalized[:])
	for index, relay := range m.relays {
		if m.consumed[index] {
			continue
		}
		if relay.AtMS > atMS+relayClockLead.Milliseconds() ||
			atMS-relay.AtMS > relayMatchAge.Milliseconds() {
			continue
		}
		exactMatch := relay.ContentBytes == len([]byte(text)) && relay.ContentSHA256 == exactHex
		normalizedMatch := relay.NormalizedBytes == len([]byte(normalizedText)) &&
			relay.NormalizedSHA256 == normalizedHex
		if !exactMatch && !normalizedMatch {
			continue
		}
		m.consumed[index] = true
		author := relay.Author
		return &author
	}
	return nil
}

func (s *Server) messageRelayMatcher(ctx context.Context, laneID string) (*relayMatcher, error) {
	service, ok := s.registry.(messageAttributionService)
	if !ok {
		return newRelayMatcher(nil), nil
	}
	relays, err := service.MessageRelays(ctx, laneID)
	if err != nil {
		return nil, err
	}
	return newRelayMatcher(relays), nil
}

func (s *Server) annotateRawEvents(
	ctx context.Context,
	laneID string,
	events []json.RawMessage,
) ([]json.RawMessage, error) {
	matcher, err := s.messageRelayMatcher(ctx, laneID)
	if err != nil {
		return nil, err
	}
	annotated := make([]json.RawMessage, len(events))
	for index, encoded := range events {
		annotated[index] = append(json.RawMessage(nil), encoded...)
		var event map[string]any
		if json.Unmarshal(encoded, &event) != nil {
			continue
		}
		text, atMS := rawUserMessage(event)
		author := matcher.match(text, atMS)
		if author == nil {
			continue
		}
		event["author"] = author
		next, err := json.Marshal(event)
		if err != nil {
			return nil, fmt.Errorf("encode attributed event: %w", err)
		}
		annotated[index] = next
	}
	return annotated, nil
}

func (s *Server) annotateTranscript(ctx context.Context, transcript *integrations.TranscriptResponse) error {
	if transcript == nil || transcript.Session.ID == "" {
		return nil
	}
	service, ok := s.registry.(messageAttributionService)
	if !ok {
		return nil
	}
	relays, err := service.MessageRelays(ctx, transcript.Session.ID)
	if err != nil {
		return err
	}
	if len(relays) == 0 {
		return nil
	}
	messages := transcript.Messages
	if transcript.Truncated || transcript.HasMore {
		full, err := s.integrationEndpoints.Transcript(s.registry.List(true), transcript.Session.ID)
		if err != nil {
			return fmt.Errorf("read complete transcript for attribution: %w", err)
		}
		messages = full.Messages
	}
	matcher := newRelayMatcher(relays)
	authors := make(map[string]integrations.MessageAuthor)
	for index := range messages {
		message := &messages[index]
		if message.Role != "user" || message.Timestamp == nil {
			continue
		}
		at, err := time.Parse(time.RFC3339Nano, *message.Timestamp)
		if err != nil {
			continue
		}
		author := matcher.match(message.Text, at.UnixMilli())
		if author == nil {
			continue
		}
		authors[message.ID] = integrations.MessageAuthor{
			Kind: string(author.Kind), ID: author.ID,
			Name: author.Name, Client: author.Client,
		}
	}
	for index := range transcript.Messages {
		if author, ok := authors[transcript.Messages[index].ID]; ok {
			value := author
			transcript.Messages[index].Author = &value
		}
	}
	return nil
}

func rawUserMessage(event map[string]any) (string, int64) {
	if event["type"] != "user" {
		return "", 0
	}
	message, ok := event["message"].(map[string]any)
	if !ok || message["role"] != "user" {
		return "", 0
	}
	text := rawContentText(message["content"])
	timestamp, _ := event["timestamp"].(string)
	at, err := time.Parse(time.RFC3339Nano, timestamp)
	if err != nil {
		return "", 0
	}
	return text, at.UnixMilli()
}

func rawContentText(content any) string {
	switch value := content.(type) {
	case string:
		return value
	case []any:
		parts := make([]string, 0, len(value))
		for _, item := range value {
			block, ok := item.(map[string]any)
			if !ok || block["type"] != "text" {
				continue
			}
			if text, ok := block["text"].(string); ok {
				parts = append(parts, text)
			}
		}
		return strings.Join(parts, "\n")
	default:
		return ""
	}
}
