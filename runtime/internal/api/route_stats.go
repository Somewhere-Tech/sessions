package api

import (
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/somewhere-tech/sessions/runtime/internal/background"
)

// Which routes the daemon spent its time answering.
//
// From the Mini, 11 September: 150 seconds at 100-170% CPU after startup, of
// which the named background passes accounted for about a tenth. The rest was
// request handling — the app's own startup asking for sessions, previews and
// transcripts, and another machine's fleet relay polling every five seconds —
// and "the UI hammered /api/sessions" was a guess nobody could check. It is a
// counter now.
//
// Paths are collapsed to their shape (`/api/sessions/:id/transcript`), so this
// counts routes rather than sessions and carries no id, path or title.

const (
	// routeWindow is how far back the report looks. Long enough to cover a
	// restart's burst, short enough that it describes now.
	routeWindow = 5 * time.Minute
	// routeBucket is the granularity the window is trimmed at.
	routeBucket = 30 * time.Second
	// routeReportLimit is how many routes the report names, busiest first.
	routeReportLimit = 12
	// routeTrackLimit bounds memory when something invents paths. Everything
	// past it is counted together, which is still true, just less specific.
	routeTrackLimit = 200
)

type routeCounter struct {
	count int64
	total time.Duration
	max   time.Duration
	cpu   time.Duration
}

type routeStats struct {
	mu      sync.Mutex
	now     func() time.Time
	cpu     func() (time.Duration, bool)
	buckets map[string]map[int64]*routeCounter
}

func newRouteStats() *routeStats {
	return &routeStats{
		now: time.Now, cpu: background.ProcessCPU,
		buckets: make(map[string]map[int64]*routeCounter, 32),
	}
}

// begin starts timing one request and returns the function that records it.
// Callers defer the result, so a panicking handler is still counted.
func (r *routeStats) begin(method, path string) func() {
	if r == nil {
		return func() {}
	}
	started := r.now()
	startCPU, hasCPU := r.cpu()
	return func() {
		took := r.now().Sub(started)
		var used time.Duration
		if endCPU, ok := r.cpu(); ok && hasCPU {
			used = endCPU - startCPU
		}
		r.record(routeName(method, path), took, used)
	}
}

func (r *routeStats) record(route string, took, cpu time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, known := r.buckets[route]; !known && len(r.buckets) >= routeTrackLimit {
		route = "other"
	}
	buckets := r.buckets[route]
	if buckets == nil {
		buckets = make(map[int64]*routeCounter, 4)
		r.buckets[route] = buckets
	}
	index := r.now().UnixNano() / int64(routeBucket)
	counter := buckets[index]
	if counter == nil {
		counter = &routeCounter{}
		buckets[index] = counter
	}
	counter.count++
	counter.total += took
	counter.cpu += cpu
	if took > counter.max {
		counter.max = took
	}
	oldest := index - int64(routeWindow/routeBucket)
	for existing := range buckets {
		if existing < oldest {
			delete(buckets, existing)
		}
	}
}

// report is the busiest routes of the last window, for /api/health/deep.
//
// cpu_ms is this process's CPU over each request's own window, not that
// request's alone — Go has no per-goroutine CPU clock — so concurrent requests
// each count the same CPU and the figure is an upper bound. It answers "what
// was being served while the machine was hot", which is the question.
func (r *routeStats) report() map[string]any {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	oldest := r.now().UnixNano()/int64(routeBucket) - int64(routeWindow/routeBucket)
	type row struct {
		route string
		routeCounter
	}
	rows := make([]row, 0, len(r.buckets))
	for route, buckets := range r.buckets {
		summed := routeCounter{}
		for index, counter := range buckets {
			if index < oldest {
				delete(buckets, index)
				continue
			}
			summed.count += counter.count
			summed.total += counter.total
			summed.cpu += counter.cpu
			if counter.max > summed.max {
				summed.max = counter.max
			}
		}
		if summed.count == 0 {
			delete(r.buckets, route)
			continue
		}
		rows = append(rows, row{route: route, routeCounter: summed})
	}
	sort.Slice(rows, func(left, right int) bool {
		if rows[left].total != rows[right].total {
			return rows[left].total > rows[right].total
		}
		return rows[left].count > rows[right].count
	})
	busiest := make([]map[string]any, 0, min(routeReportLimit, len(rows)))
	for _, entry := range rows[:min(routeReportLimit, len(rows))] {
		busiest = append(busiest, map[string]any{
			"route": entry.route, "count": entry.count,
			"ms": entry.total.Milliseconds(), "max_ms": entry.max.Milliseconds(),
			"cpu_ms": entry.cpu.Milliseconds(),
		})
	}
	return map[string]any{"window_sec": int64(routeWindow / time.Second), "busiest": busiest}
}

// routeName is the shape of a request: its method and its path with the
// identifiers taken out, because a thousand requests for a thousand sessions
// are one route being busy, not a thousand routes.
func routeName(method, path string) string {
	parts := strings.Split(path, "/")
	for index, part := range parts {
		if looksLikeIdentifier(part) {
			parts[index] = ":id"
		}
	}
	return method + " " + strings.Join(parts, "/")
}

func looksLikeIdentifier(part string) bool {
	if len(part) < 8 {
		return false
	}
	digits, hyphens := 0, 0
	for _, symbol := range part {
		switch {
		case symbol >= '0' && symbol <= '9':
			digits++
		case symbol == '-':
			hyphens++
		}
	}
	return digits > 0 && (hyphens >= 4 || len(part) >= 16)
}
