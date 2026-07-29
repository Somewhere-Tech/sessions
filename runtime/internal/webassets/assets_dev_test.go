//go:build !embedui

package webassets

import (
	"net/http/httptest"
	"testing"
)

func TestDevelopmentBuildHasNoEmbeddedAssets(t *testing.T) {
	if _, ok := FS(); ok {
		t.Fatal("FS reported embedded assets without the embedui build tag")
	}
	response := httptest.NewRecorder()
	request := httptest.NewRequest("GET", "http://example.test/", nil)
	if ServeHTTP(response, request) {
		t.Fatal("ServeHTTP reported an embedded response without the embedui build tag")
	}
}
