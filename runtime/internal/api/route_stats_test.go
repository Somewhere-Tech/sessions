package api

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"
)

// From the Mini, 11 September: 150 s at 100-170% CPU after startup, with the
// named passes accounting for a tenth of it. "The UI hammered /api/sessions"
// was the leading theory and there was no way to check it.
func TestDeepHealthNamesTheBusiestRoutes(t *testing.T) {
	daemon, manager := newTimingDaemon(t)
	defer manager.Close()

	for range 7 {
		if response := serve(t, daemon, http.MethodGet, "/api/sessions", nil, "127.0.0.1:4321", nil); response.Code != http.StatusOK {
			t.Fatalf("sessions listing status=%d", response.Code)
		}
	}
	if response := serve(t, daemon, http.MethodGet, "/api/health", nil, "127.0.0.1:4321", nil); response.Code != http.StatusOK {
		t.Fatalf("health status=%d", response.Code)
	}

	busiest := busiestRoutes(t, daemon)
	sessions, ok := busiest["GET /api/sessions"]
	if !ok {
		t.Fatalf("the busiest routes do not include the listing: %#v", busiest)
	}
	if count := int64(sessions["count"].(float64)); count != 7 {
		t.Fatalf("GET /api/sessions counted %d times, want 7", count)
	}
	for _, field := range []string{"ms", "max_ms", "cpu_ms"} {
		if _, present := sessions[field]; !present {
			t.Fatalf("the route row has no %s: %#v", field, sessions)
		}
	}
	if _, counted := busiest["GET /api/health"]; !counted {
		t.Fatalf("health was served but not counted: %#v", busiest)
	}
}

// A thousand requests for a thousand sessions are one busy route, not a
// thousand routes — and the report carries no session id.
func TestRoutesAreCountedByShapeNotByIdentifier(t *testing.T) {
	daemon, manager := newTimingDaemon(t)
	defer manager.Close()

	for _, id := range []string{
		"11111111-1111-4111-8111-111111111111",
		"22222222-2222-4222-8222-222222222222",
		"33333333-3333-4333-8333-333333333333",
	} {
		serve(t, daemon, http.MethodGet, "/api/sessions/"+id+"/transcript", nil, "127.0.0.1:4321", nil)
	}

	busiest := busiestRoutes(t, daemon)
	row, ok := busiest["GET /api/sessions/:id/transcript"]
	if !ok {
		t.Fatalf("per-session requests were not collapsed to their shape: %#v", busiest)
	}
	if count := int64(row["count"].(float64)); count != 3 {
		t.Fatalf("the collapsed route counted %d requests, want 3", count)
	}
	// No session id reaches the report, so it can be pasted into a bug report.
	for route := range busiest {
		if strings.Contains(route, "1111") || strings.Contains(route, "2222") || strings.Contains(route, "3333") {
			t.Fatalf("a route name carries a session id: %q", route)
		}
	}
}

func TestRouteWindowForgetsWhatIsOlderThanItsWindow(t *testing.T) {
	stats := newRouteStats()
	now := time.Now()
	stats.now = func() time.Time { return now }
	stats.record("GET /api/sessions", 5*time.Millisecond, time.Millisecond)
	now = now.Add(routeWindow + time.Minute)
	stats.record("GET /api/history", 7*time.Millisecond, time.Millisecond)

	report := stats.report()
	rows, _ := report["busiest"].([]map[string]any)
	if len(rows) != 1 || rows[0]["route"] != "GET /api/history" {
		t.Fatalf("the window kept %#v; only the recent route belongs in it", rows)
	}
	if window := report["window_sec"]; window != int64(300) {
		t.Fatalf("window_sec = %#v", window)
	}
}

func busiestRoutes(t *testing.T, daemon *Server) map[string]map[string]any {
	t.Helper()
	response := serve(t, daemon, http.MethodGet, "/api/health/deep", nil, "127.0.0.1:4321", nil)
	if response.Code != http.StatusOK {
		t.Fatalf("deep health status=%d", response.Code)
	}
	var body struct {
		Routes struct {
			Busiest []map[string]any `json:"busiest"`
		} `json:"routes"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	byRoute := make(map[string]map[string]any, len(body.Routes.Busiest))
	for _, row := range body.Routes.Busiest {
		name, _ := row["route"].(string)
		byRoute[name] = row
	}
	return byRoute
}
