//go:build embedui

package webassets

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestEmbeddedIndexAndSPAFallback(t *testing.T) {
	for _, target := range []string{"/", "/sessions/embed-test"} {
		t.Run(target, func(t *testing.T) {
			response := httptest.NewRecorder()
			request := httptest.NewRequest(http.MethodGet, "http://example.test"+target, nil)
			if !ServeHTTP(response, request) {
				t.Fatal("ServeHTTP reported no embedded frontend")
			}
			if response.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200", response.Code)
			}
			if !strings.Contains(response.Body.String(), "<title>") {
				t.Fatalf("embedded response does not contain a title: %q", response.Body.String())
			}
		})
	}
}
