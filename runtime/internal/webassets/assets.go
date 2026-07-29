// Package webassets exposes the optional frontend compiled into sessionsd.
package webassets

import (
	"bytes"
	"encoding/json"
	"io/fs"
	"net/http"
	"net/url"
	"path"
	"strings"
	"time"
)

// FS returns the embedded frontend when the embedui build tag is enabled.
// Plain development builds intentionally return no filesystem.
func FS() (fs.FS, bool) {
	return embeddedFS()
}

// ServeHTTP serves an embedded asset, falling back to index.html for SPA
// routes. It reports false when this binary was built without embedded assets.
func ServeHTTP(response http.ResponseWriter, request *http.Request) bool {
	assets, ok := FS()
	if !ok {
		return false
	}

	decoded, err := url.PathUnescape(request.URL.EscapedPath())
	if err != nil {
		sendInvalidPath(response)
		return true
	}
	relative := strings.TrimLeft(decoded, "/")
	normalized := path.Clean(relative)
	if normalized == "." {
		normalized = ""
	}
	if normalized == ".." || strings.HasPrefix(normalized, "../") || path.IsAbs(normalized) {
		sendInvalidPath(response)
		return true
	}

	name := normalized
	if name == "" {
		name = "index.html"
	} else if info, statErr := fs.Stat(assets, name); statErr == nil && info.IsDir() {
		name = path.Join(name, "index.html")
	}
	data, readErr := fs.ReadFile(assets, name)
	if readErr != nil {
		name = "index.html"
		data, readErr = fs.ReadFile(assets, name)
	}
	if readErr != nil {
		return false
	}

	http.ServeContent(response, request, path.Base(name), time.Time{}, bytes.NewReader(data))
	return true
}

func sendInvalidPath(response http.ResponseWriter) {
	response.Header().Set("Content-Type", "application/json; charset=utf-8")
	response.WriteHeader(http.StatusBadRequest)
	_ = json.NewEncoder(response).Encode(map[string]string{"error": "invalid path"})
}
