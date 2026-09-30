package main

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/somewhere-tech/sessions/runtime/internal/proto"
	"github.com/somewhere-tech/sessions/runtime/internal/providerfault"
)

type manualRetryTimer struct{ stopped bool }

func (t *manualRetryTimer) Stop() bool {
	t.stopped = true
	return true
}

type retryHarness struct {
	now       time.Time
	delay     time.Duration
	fire      func()
	timer     *manualRetryTimer
	runs      []string
	attempts  []int
	history   []json.RawMessage
	published []*proto.ProviderRetry
}

func newRetryHarness() (*retryHarness, *structuredRetryController) {
	h := &retryHarness{now: time.Unix(100, 0)}
	c := newStructuredRetryController(
		func(text string, attempt int) bool {
			h.runs = append(h.runs, text)
			h.attempts = append(h.attempts, attempt)
			return true
		},
		func(raw json.RawMessage) { h.history = append(h.history, raw) },
		func(retry *proto.ProviderRetry) {
			if retry == nil {
				h.published = append(h.published, nil)
				return
			}
			copy := *retry
			h.published = append(h.published, &copy)
		},
	)
	c.now = func() time.Time { return h.now }
	c.after = func(delay time.Duration, fire func()) retryTimer {
		h.delay, h.fire, h.timer = delay, fire, &manualRetryTimer{}
		return h.timer
	}
	return h, c
}

func TestStructuredRetrySchedulesAndAdvancesFailedTurn(t *testing.T) {
	h, controller := newRetryHarness()
	fault := providerfault.Fault{Kind: providerfault.KindUnavailable}
	controller.Failed("keep this exact prompt", 0, fault, "503 overloaded")
	if h.delay != 30*time.Second || controller.Current().Attempt != 1 || len(h.history) != 1 {
		t.Fatalf("initial schedule delay=%s retry=%+v history=%d", h.delay, controller.Current(), len(h.history))
	}
	h.fire()
	if len(h.runs) != 1 || h.runs[0] != "keep this exact prompt" || h.attempts[0] != 1 {
		t.Fatalf("first retry runs=%v attempts=%v", h.runs, h.attempts)
	}
	controller.Failed("keep this exact prompt", 1, fault, "503 overloaded")
	if h.delay != time.Minute || controller.Current().Attempt != 2 || len(h.history) != 2 {
		t.Fatalf("second schedule delay=%s retry=%+v history=%d", h.delay, controller.Current(), len(h.history))
	}
}

func TestStructuredRetryCancellationPathsDoNotRunTimer(t *testing.T) {
	for _, cancel := range []struct {
		name string
		run  func(*structuredRetryController) error
	}{
		{name: "stop", run: func(c *structuredRetryController) error { return c.Stop() }},
		{name: "interrupt", run: func(c *structuredRetryController) error { c.Interrupt(); return nil }},
		{name: "new input", run: func(c *structuredRetryController) error { c.Replace(); return nil }},
		{name: "success", run: func(c *structuredRetryController) error { c.Succeeded(); return nil }},
	} {
		t.Run(cancel.name, func(t *testing.T) {
			h, controller := newRetryHarness()
			controller.Failed("prompt", 0, providerfault.Fault{Kind: providerfault.KindUnavailable}, "overloaded")
			fire, timer := h.fire, h.timer
			if err := cancel.run(controller); err != nil {
				t.Fatal(err)
			}
			fire()
			if !timer.stopped || len(h.runs) != 0 || controller.Current() != nil {
				t.Fatalf("cancel left timer=%+v runs=%v retry=%+v", timer, h.runs, controller.Current())
			}
		})
	}
}

func TestStructuredRetryExhaustionAndRateLimitHint(t *testing.T) {
	h, controller := newRetryHarness()
	rate := providerfault.Fault{Kind: providerfault.KindRateLimited}
	controller.Failed("prompt", 0, rate, "try again in 42s")
	if h.delay != 42*time.Second {
		t.Fatalf("hint delay = %s, want 42s", h.delay)
	}
	controller.Failed("prompt", 4, rate, "try again in 20m")
	if h.delay != 5*time.Minute || controller.Current().Attempt != 5 {
		t.Fatalf("capped delay=%s retry=%+v", h.delay, controller.Current())
	}
	controller.Failed("prompt", 5, rate, "try again in 20m")
	if controller.Current() != nil {
		t.Fatalf("exhausted retry = %+v", controller.Current())
	}
}

func TestStructuredRetryDoesNotScheduleAuthenticationFailure(t *testing.T) {
	h, controller := newRetryHarness()
	controller.Failed("prompt", 0, providerfault.Fault{Kind: providerfault.KindAuth}, "not logged in")
	if h.timer != nil || controller.Current() != nil || len(h.history) != 0 || len(h.runs) != 0 {
		t.Fatalf("authentication failure scheduled retry: timer=%+v retry=%+v history=%d runs=%v", h.timer, controller.Current(), len(h.history), h.runs)
	}
}
