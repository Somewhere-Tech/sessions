package api

import (
	"context"
	"crypto/sha256"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/somewhere-tech/sessions/runtime/internal/ledger"
)

func testRelay(atMS int64, name, text string) ledger.MessageRelayed {
	exact := sha256.Sum256([]byte(text))
	normalized := sha256.Sum256([]byte(text))
	return ledger.MessageRelayed{
		Meta: ledger.Meta{EventID: fmt.Sprintf("event-%d", atMS), LaneID: "target", AtMS: atMS},
		Author: ledger.MessageAuthor{
			Kind: ledger.CreatorSession, ID: "00000000-0000-4000-8000-000000000001",
			Name: name, Client: "sessions-cli",
		},
		ContentSHA256: fmt.Sprintf("%x", exact[:]), ContentBytes: len([]byte(text)),
		NormalizedSHA256: fmt.Sprintf("%x", normalized[:]), NormalizedBytes: len([]byte(text)),
	}
}

func TestRelayMatcherKeepsRapidDuplicateUnicodeAuthorsInOrder(t *testing.T) {
	const text = "Please review café 🚀"
	start := time.Now().UnixMilli()
	matcher := newRelayMatcher([]ledger.MessageRelayed{
		testRelay(start+20, "Second lane", text),
		testRelay(start, "First lane", text),
	})
	first := matcher.match(text, start+100)
	second := matcher.match(text, start+200)
	if first == nil || first.Name != "First lane" {
		t.Fatalf("first author = %#v", first)
	}
	if second == nil || second.Name != "Second lane" {
		t.Fatalf("second author = %#v", second)
	}
	if duplicate := matcher.match(text, start+300); duplicate != nil {
		t.Fatalf("one relay matched twice: %#v", duplicate)
	}
}

func TestRelayMatcherRejectsStaleAndDifferentContent(t *testing.T) {
	const text = "ship it"
	start := time.Now().UnixMilli()
	if got := newRelayMatcher([]ledger.MessageRelayed{
		testRelay(start-3*time.Minute.Milliseconds(), "Old lane", text),
	}).match(text, start); got != nil {
		t.Fatalf("stale relay matched: %#v", got)
	}
	if got := newRelayMatcher([]ledger.MessageRelayed{
		testRelay(start, "Other lane", "different"),
	}).match(text, start+100); got != nil {
		t.Fatalf("different relay matched: %#v", got)
	}
}

func TestCaptureInputAttributionRejectsAmbiguousOrForgedTransportShape(t *testing.T) {
	request, _ := http.NewRequest(http.MethodPost, "/api/sessions/target/input", nil)
	request.Header.Set(creatorSessionHeader, "00000000-0000-4000-8000-000000000001")
	request.Header.Set(creatorOwnerHeader, "owner:external")
	if _, _, err := captureInputAttribution(request); err == nil {
		t.Fatal("combined source headers were accepted")
	}

	request, _ = http.NewRequest(http.MethodPost, "/api/sessions/target/input", nil)
	request.Header.Set(creatorOwnerHeader, "owner:external")
	if _, _, err := captureInputAttribution(request); err == nil {
		t.Fatal("external owner attribution was accepted")
	}

	request, _ = http.NewRequest(http.MethodPost, "/api/sessions/target/input", nil)
	request.Header.Set(creatorSessionHeader, "00000000-0000-4000-8000-000000000001")
	request.Header.Set(endClientHeader, "sessions-cli")
	request = request.WithContext(context.WithValue(request.Context(), authPrincipalContextKey{}, authPrincipal{Local: true}))
	attribution, present, err := captureInputAttribution(request)
	if err != nil || !present || attribution.SourceSessionID == "" || attribution.Client != "sessions-cli" {
		t.Fatalf("valid attribution = %#v present=%v err=%v", attribution, present, err)
	}

	request, _ = http.NewRequest(http.MethodPost, "/api/sessions/target/input", nil)
	request.Header.Set(creatorSessionHeader, "00000000-0000-4000-8000-000000000001")
	request = request.WithContext(context.WithValue(request.Context(), authPrincipalContextKey{}, authPrincipal{Local: false}))
	if _, _, err := captureInputAttribution(request); err == nil {
		t.Fatal("remote source-session attribution was accepted")
	}
}
