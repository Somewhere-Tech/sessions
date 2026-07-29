package smartsearch

import (
	"context"
	"crypto/sha256"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/somewhere-tech/sessions/runtime/internal/state"
)

func TestPlanUsesSelectedProviderAndOnlyNaturalQuery(t *testing.T) {
	var provider, prompt string
	service := NewServiceWithRunner(func(_ context.Context, gotProvider, gotPrompt string) (string, error) {
		provider, prompt = gotProvider, gotPrompt
		return "result:\n{\"query\":\"apple AND notarization\"}\n", nil
	})
	plan, err := service.Plan(context.Background(), state.AISettings{Provider: " CLAUDE "}, "the session where I discussed Apple signing")
	if err != nil {
		t.Fatal(err)
	}
	if provider != state.AIProviderClaude || plan.Provider != state.AIProviderClaude || plan.Query != "apple AND notarization" {
		t.Fatalf("provider=%q plan=%#v", provider, plan)
	}
	if !strings.Contains(prompt, "the session where I discussed Apple signing") || strings.Contains(prompt, "transcript") {
		t.Fatalf("unexpected prompt: %s", prompt)
	}
}

func TestPlanRejectsInvalidInputAndOutput(t *testing.T) {
	service := NewServiceWithRunner(func(context.Context, string, string) (string, error) { return "not json", nil })
	if _, err := service.Plan(context.Background(), state.DefaultAISettings(), ""); err == nil {
		t.Fatal("empty query was accepted")
	}
	if _, err := service.Plan(context.Background(), state.DefaultAISettings(), "remember this"); err == nil {
		t.Fatal("non-JSON plan was accepted")
	}
}

func TestPlanCachesIdenticalProviderQuery(t *testing.T) {
	var calls atomic.Int32
	service := NewServiceWithRunner(func(context.Context, string, string) (string, error) {
		calls.Add(1)
		return `{"query":"cached result"}`, nil
	})
	for range 2 {
		plan, err := service.Plan(context.Background(), state.DefaultAISettings(), "find the cached result")
		if err != nil || plan.Query != "cached result" {
			t.Fatalf("plan=%#v err=%v", plan, err)
		}
	}
	if calls.Load() != 1 {
		t.Fatalf("runner calls=%d, want 1", calls.Load())
	}
}

func TestPlanAllowsOnlyOneActiveModelCall(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	service := NewServiceWithRunner(func(context.Context, string, string) (string, error) {
		close(started)
		<-release
		return `{"query":"first"}`, nil
	})
	done := make(chan error, 1)
	go func() {
		_, err := service.Plan(context.Background(), state.DefaultAISettings(), "first query")
		done <- err
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("first planner did not start")
	}
	if _, err := service.Plan(context.Background(), state.DefaultAISettings(), "second query"); !IsBusy(err) {
		t.Fatalf("second planner error=%v, want ErrBusy", err)
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatalf("first planner: %v", err)
	}
}

func TestPlanCacheIsHashedAndBounded(t *testing.T) {
	service := NewServiceWithRunner(func(_ context.Context, _ string, prompt string) (string, error) {
		return `{"query":"` + strings.Repeat("q", len(prompt)%20+1) + `"}`, nil
	})
	for index := range maxCachedPlans + 20 {
		query := fmt.Sprintf("sensitive query %d", index)
		if _, err := service.Plan(context.Background(), state.DefaultAISettings(), query); err != nil {
			t.Fatal(err)
		}
	}
	if len(service.cache) != maxCachedPlans {
		t.Fatalf("cache entries=%d, want %d", len(service.cache), maxCachedPlans)
	}
	for key := range service.cache {
		if strings.Contains(key, "sensitive query") || len(key) != sha256.Size*2 {
			t.Fatalf("cache key exposes input or is not a SHA-256 digest: %q", key)
		}
	}
}
