package api

import (
	"math"
	"net/http"
	"strconv"
	"strings"
	"testing"
)

func TestRetentionRouteRejectsOverflowBeforeDurationConversion(t *testing.T) {
	daemon := newTestDaemon(t)
	response := serve(
		t,
		daemon.handler,
		http.MethodPost,
		"/api/retention/gc",
		strings.NewReader(`{"older_than_ms":`+formatInt64(math.MaxInt64)+`,"dry_run":true}`),
		"127.0.0.1:1234",
		nil,
	)
	if response.Code != http.StatusBadRequest ||
		!strings.Contains(response.Body.String(), "between one hour and ten years") {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
}

func formatInt64(value int64) string {
	return strconv.FormatInt(value, 10)
}
