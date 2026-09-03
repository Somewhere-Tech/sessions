package main

import (
	"encoding/json"
	"errors"
	"sync"
	"time"

	"github.com/somewhere-tech/sessions/runtime/internal/proto"
	"github.com/somewhere-tech/sessions/runtime/internal/providerfault"
)

const structuredRetryMax = 5

var structuredRetryBackoff = [...]time.Duration{
	30 * time.Second,
	time.Minute,
	2 * time.Minute,
	5 * time.Minute,
	5 * time.Minute,
}

type retryTimer interface{ Stop() bool }

type structuredRetryController struct {
	mu         sync.Mutex
	timer      retryTimer
	serial     uint64
	input      string
	kind       string
	suppressed bool
	retry      *proto.ProviderRetry
	now        func() time.Time
	after      func(time.Duration, func()) retryTimer
	run        func(string, int) bool
	history    func(json.RawMessage)
	publish    func(*proto.ProviderRetry)
}

func newStructuredRetryController(
	run func(string, int) bool,
	history func(json.RawMessage),
	publish func(*proto.ProviderRetry),
) *structuredRetryController {
	return &structuredRetryController{
		now: time.Now,
		after: func(delay time.Duration, fire func()) retryTimer {
			return time.AfterFunc(delay, fire)
		},
		run: run, history: history, publish: publish,
	}
}

func (c *structuredRetryController) Failed(input string, attempt int, fault providerfault.Fault, raw string) {
	delay, scheduled := retryDelay(attempt, fault, raw)
	c.mu.Lock()
	c.cancelTimerLocked()
	c.input = input
	c.kind = fault.Kind
	if c.suppressed || !scheduled {
		c.retry = nil
		c.mu.Unlock()
		c.publish(nil)
		return
	}
	next := attempt + 1
	nextAt := c.now().Add(delay)
	retry := &proto.ProviderRetry{Attempt: next, Max: structuredRetryMax, NextAt: nextAt.UnixMilli(), Kind: fault.Kind}
	c.retry = retry
	c.suppressed = false
	c.serial++
	serial := c.serial
	c.timer = c.after(delay, func() { c.fire(serial) })
	c.mu.Unlock()
	c.appendHistory(*retry)
	c.publish(retry)
}

func retryDelay(attempt int, fault providerfault.Fault, raw string) (time.Duration, bool) {
	if fault.Kind != providerfault.KindUnavailable && fault.Kind != providerfault.KindRateLimited {
		return 0, false
	}
	if attempt >= structuredRetryMax {
		return 0, false
	}
	delay := structuredRetryBackoff[attempt]
	if fault.Kind == providerfault.KindRateLimited {
		hint := min(providerfault.RetryAfter(raw), 5*time.Minute)
		if hint > delay {
			delay = hint
		}
	}
	return delay, true
}

func (c *structuredRetryController) appendHistory(retry proto.ProviderRetry) {
	raw, err := providerfault.RetryHistoryEvent(retry.Attempt, retry.Max, time.UnixMilli(retry.NextAt))
	if err == nil {
		c.history(raw)
	}
}

func (c *structuredRetryController) fire(serial uint64) {
	c.mu.Lock()
	if c.serial != serial || c.retry == nil || c.input == "" {
		c.mu.Unlock()
		return
	}
	input, attempt := c.input, c.retry.Attempt
	c.timer = nil
	c.mu.Unlock()
	c.run(input, attempt)
}

func (c *structuredRetryController) RunNow() error {
	c.mu.Lock()
	if c.input == "" {
		c.mu.Unlock()
		return errors.New("the runner has no failed turn to retry")
	}
	hadSchedule := c.retry != nil
	attempt := 1
	if hadSchedule {
		attempt = c.retry.Attempt
	}
	c.cancelTimerLocked()
	retry := &proto.ProviderRetry{
		Attempt: attempt, Max: structuredRetryMax, NextAt: c.now().UnixMilli(), Kind: c.kind,
	}
	c.retry = retry
	input := c.input
	c.mu.Unlock()
	if !hadSchedule {
		c.appendHistory(*retry)
	}
	c.publish(retry)
	if !c.run(input, attempt) {
		return errors.New("provider turn is already active")
	}
	return nil
}

func (c *structuredRetryController) Stop() error {
	c.mu.Lock()
	if c.retry == nil {
		c.mu.Unlock()
		return errors.New("no automatic provider retry is scheduled")
	}
	c.cancelTimerLocked()
	c.retry = nil
	c.suppressed = true
	c.mu.Unlock()
	c.publish(nil)
	return nil
}

func (c *structuredRetryController) Replace() {
	c.clear(true)
}

func (c *structuredRetryController) Succeeded() {
	c.clear(true)
}

func (c *structuredRetryController) Interrupt() {
	c.clear(false)
}

func (c *structuredRetryController) Close() {
	c.mu.Lock()
	c.cancelTimerLocked()
	c.retry = nil
	c.mu.Unlock()
}

func (c *structuredRetryController) clear(forget bool) {
	c.mu.Lock()
	hadRetry := c.retry != nil
	c.cancelTimerLocked()
	c.retry = nil
	if forget {
		c.input = ""
		c.kind = ""
		c.suppressed = false
	} else {
		c.suppressed = true
	}
	c.mu.Unlock()
	if hadRetry {
		c.publish(nil)
	}
}

func (c *structuredRetryController) Current() *proto.ProviderRetry {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.retry == nil {
		return nil
	}
	cloned := *c.retry
	return &cloned
}

func (c *structuredRetryController) cancelTimerLocked() {
	c.serial++
	if c.timer != nil {
		c.timer.Stop()
		c.timer = nil
	}
}
