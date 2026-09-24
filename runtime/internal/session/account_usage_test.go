package session

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/somewhere-tech/sessions/runtime/internal/codexapp"
)

type usageClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *usageClock) read() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *usageClock) advance(by time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(by)
	c.mu.Unlock()
}

func usageManager(t *testing.T, reader accountUsageReader) (*Manager, *usageClock) {
	t.Helper()
	manager, _, _ := newWorktreeTestManager(t, t.TempDir())
	clock := &usageClock{now: time.UnixMilli(1_800_000_000_000)}
	manager.accountUsage.reader, manager.accountUsage.now = reader, clock.read
	return manager, clock
}

func availableUsage(percent int) AccountUsage {
	return AccountUsage{
		State: AccountUsageAvailable, CheckedAt: 1, ReadAt: 1,
		Identity: &AccountIdentity{Email: "a@example.test", CheckedAt: 1},
		Buckets:  []AccountUsageBucket{{LimitID: "codex", Windows: []AccountUsageWindow{{Kind: "primary", UsedPercent: percent}}}},
	}
}

// Concurrent requests share one provider read, and a repeat inside the TTL is
// answered from it. Only after the TTL, or an explicit refresh past the floor,
// is the provider asked again.
func TestAccountUsageCoalescesAndCachesProviderReads(t *testing.T) {
	var reads atomic.Int32
	release := make(chan struct{})
	manager, clock := usageManager(t, func(ctx context.Context, tool, home string) AccountUsage {
		reads.Add(1)
		<-release
		return availableUsage(10)
	})
	if _, err := manager.CreateAccount("codex", "work", "Work"); err != nil {
		t.Fatal(err)
	}
	var wait sync.WaitGroup
	for range 4 {
		wait.Add(1)
		go func() {
			defer wait.Done()
			usage, err := manager.AccountUsage(context.Background(), "", "", false)
			if err != nil || len(usage) != 1 || usage[0].State != AccountUsageAvailable || usage[0].Label != "Work" {
				t.Errorf("usage = %#v, %v", usage, err)
			}
		}()
	}
	time.Sleep(50 * time.Millisecond)
	close(release)
	wait.Wait()
	if reads.Load() != 1 {
		t.Fatalf("provider read %d times for concurrent requests, want 1", reads.Load())
	}
	clock.advance(accountUsageMinRefresh / 2)
	if _, err := manager.AccountUsage(context.Background(), "codex", "work", true); err != nil || reads.Load() != 1 {
		t.Fatalf("refresh inside the floor read the provider again: reads=%d err=%v", reads.Load(), err)
	}
	clock.advance(accountUsageMinRefresh)
	if _, err := manager.AccountUsage(context.Background(), "codex", "work", false); err != nil || reads.Load() != 1 {
		t.Fatalf("read inside the TTL asked the provider again: reads=%d err=%v", reads.Load(), err)
	}
	if _, err := manager.AccountUsage(context.Background(), "codex", "work", true); err != nil || reads.Load() != 2 {
		t.Fatalf("explicit refresh past the floor did not read: reads=%d err=%v", reads.Load(), err)
	}
	clock.advance(accountUsageTTL)
	if _, err := manager.AccountUsage(context.Background(), "", "", false); err != nil || reads.Load() != 3 {
		t.Fatalf("expired reading was reused: reads=%d err=%v", reads.Load(), err)
	}
}

// A slow provider does not hold the answer: the caller's deadline reports it as
// not answered yet, the read finishes on its own, and the next request has it.
func TestAccountUsageDeadlineReportsPendingAndKeepsReading(t *testing.T) {
	var reads atomic.Int32
	release := make(chan struct{})
	manager, _ := usageManager(t, func(ctx context.Context, tool, home string) AccountUsage {
		reads.Add(1)
		<-release
		return availableUsage(55)
	})
	if _, err := manager.CreateAccount("codex", "slow", ""); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	usage, err := manager.AccountUsage(ctx, "", "", false)
	cancel()
	if err != nil || len(usage) != 1 || usage[0].State != AccountUsageUnavailable || !strings.Contains(usage[0].Message, "not answered") {
		t.Fatalf("usage = %#v, %v; want an explicit not-answered-yet reading", usage, err)
	}
	if len(usage[0].Buckets) != 0 {
		t.Fatalf("pending read invented a reading: %#v", usage[0])
	}
	close(release)
	deadline := time.Now().Add(2 * time.Second)
	for {
		usage, err = manager.AccountUsage(context.Background(), "", "", false)
		if err == nil && usage[0].State == AccountUsageAvailable {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("finished read never reached the cache: %#v %v", usage, err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if reads.Load() != 1 {
		t.Fatalf("provider read %d times, want the one abandoned read to be reused", reads.Load())
	}
}

func failedUsage(email string) AccountUsage {
	result := AccountUsage{State: AccountUsageUnavailable, Message: "Codex could not read this account's usage. Refresh to try again."}
	if email != "" {
		result.Identity = &AccountIdentity{Email: email, CheckedAt: 2}
	}
	return result
}

// A failed read keeps the last good reading beside it, marked stale, only when
// the same read showed the same account still signed in; it never becomes
// signed out or zero usage. Without that proof the answer is unknown, and a
// provider that reports no sign-in clears the reading altogether.
func TestAccountUsageFailureKeepsLastReadingOnlyForTheSameAccount(t *testing.T) {
	var next atomic.Value
	next.Store(availableUsage(42))
	manager, clock := usageManager(t, func(ctx context.Context, tool, home string) AccountUsage {
		return next.Load().(AccountUsage)
	})
	if _, err := manager.CreateAccount("codex", "work", ""); err != nil {
		t.Fatal(err)
	}
	read := func() AccountUsage {
		t.Helper()
		clock.advance(accountUsageTTL)
		usage, err := manager.AccountUsage(context.Background(), "", "", false)
		if err != nil || len(usage) != 1 {
			t.Fatalf("usage = %#v, %v", usage, err)
		}
		return usage[0]
	}
	if got := read(); got.State != AccountUsageAvailable || got.Stale {
		t.Fatalf("first reading = %#v", got)
	}
	next.Store(failedUsage("A@example.test"))
	got := read()
	if got.State != AccountUsageUnavailable || !got.Stale || got.ReadAt != 1 || len(got.Buckets) != 1 || got.Buckets[0].Windows[0].UsedPercent != 42 {
		t.Fatalf("failed read of the same account = %#v, want the stale reading", got)
	}
	if got.CheckedAt != clock.read().UnixMilli() || got.Identity == nil || got.Identity.CheckedAt != 2 {
		t.Fatalf("stale reading lost its attribution: %#v", got)
	}
	// A read that failed before the provider said who is signed in proves nothing.
	next.Store(failedUsage(""))
	if got = read(); got.Stale || len(got.Buckets) != 0 || got.Identity != nil {
		t.Fatalf("failed read without an identity = %#v, want unknown", got)
	}
	next.Store(AccountUsage{State: AccountUsageSignedOut, Message: "signed out"})
	if got = read(); got.State != AccountUsageSignedOut || len(got.Buckets) != 0 || got.Stale {
		t.Fatalf("signed-out read = %#v, want no leftover reading", got)
	}
}

// Another account signed into the same home must never inherit the previous
// account's quota, even when its own read fails; and the old reading does not
// come back afterwards.
func TestAccountUsageIdentitySwapClearsTheOldReading(t *testing.T) {
	var next atomic.Value
	next.Store(availableUsage(42))
	manager, clock := usageManager(t, func(ctx context.Context, tool, home string) AccountUsage {
		return next.Load().(AccountUsage)
	})
	if _, err := manager.CreateAccount("codex", "work", ""); err != nil {
		t.Fatal(err)
	}
	if usage, _ := manager.AccountUsage(context.Background(), "", "", false); usage[0].State != AccountUsageAvailable {
		t.Fatalf("first reading = %#v", usage[0])
	}
	next.Store(failedUsage("b@example.test"))
	clock.advance(accountUsageTTL)
	usage, _ := manager.AccountUsage(context.Background(), "", "", false)
	if usage[0].Stale || len(usage[0].Buckets) != 0 || usage[0].Identity.Email != "b@example.test" {
		t.Fatalf("failed read of another account = %#v, want no quota from the first account", usage[0])
	}
	next.Store(failedUsage("a@example.test"))
	clock.advance(accountUsageTTL)
	if usage, _ = manager.AccountUsage(context.Background(), "", "", false); usage[0].Stale || len(usage[0].Buckets) != 0 {
		t.Fatalf("cleared reading came back = %#v", usage[0])
	}
	for _, other := range []*AccountIdentity{
		{Email: "a@example.test", Organization: "Other org"},
		{Email: "a@example.test", AccountID: "ws-2"},
	} {
		good := availableUsage(1)
		failed := failedUsage("")
		failed.Identity = other
		if got := withLastReading(failed, &good); got.Stale {
			t.Fatalf("identity %#v inherited %#v", other, good.Identity)
		}
	}
}

// A sign-in, recheck or removal through Sessions discards every earlier
// reading of that home. A read that started before is fenced off: it answers
// its own waiters as unknown and never reaches the cache, so the refresh floor
// cannot hand the previous sign-in's quota to the next request.
func TestAccountUsageInvalidationFencesAnEarlierRead(t *testing.T) {
	var reads atomic.Int32
	release := make(chan struct{})
	manager, _ := usageManager(t, func(ctx context.Context, tool, home string) AccountUsage {
		if reads.Add(1) == 1 {
			<-release
			return availableUsage(42)
		}
		fresh := availableUsage(7)
		fresh.Identity = &AccountIdentity{Email: "b@example.test", CheckedAt: 3}
		return fresh
	})
	if _, err := manager.CreateAccount("codex", "work", ""); err != nil {
		t.Fatal(err)
	}
	early := make(chan AccountUsage, 1)
	go func() {
		usage, _ := manager.AccountUsage(context.Background(), "", "", false)
		early <- usage[0]
	}()
	for reads.Load() == 0 {
		time.Sleep(time.Millisecond)
	}
	// Re-adding the account (like a completed sign-in) invalidates this home.
	if _, err := manager.CreateAccount("codex", "work", ""); err != nil {
		t.Fatal(err)
	}
	usage, _ := manager.AccountUsage(context.Background(), "", "", true)
	if reads.Load() != 2 || usage[0].State != AccountUsageAvailable || usage[0].Buckets[0].Windows[0].UsedPercent != 7 {
		t.Fatalf("read after invalidation = %#v (reads %d), want a new read", usage[0], reads.Load())
	}
	close(release)
	if late := <-early; late.State != AccountUsageUnavailable || len(late.Buckets) != 0 || !strings.Contains(late.Message, "changed") {
		t.Fatalf("fenced read = %#v, want unknown", late)
	}
	if usage, _ = manager.AccountUsage(context.Background(), "", "", true); usage[0].Buckets[0].Windows[0].UsedPercent != 7 || reads.Load() != 2 {
		t.Fatalf("cache after the late read = %#v (reads %d)", usage[0], reads.Load())
	}
	// Removing the account discards its reading too.
	if err := manager.ForgetAccount("codex", "work"); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.CreateAccount("codex", "work", ""); err != nil {
		t.Fatal(err)
	}
	if manager.AccountUsage(context.Background(), "", "", false); reads.Load() != 3 {
		t.Fatalf("reads = %d after removal, want a new read", reads.Load())
	}
}

func TestAccountUsageListsOnlyRegisteredAccounts(t *testing.T) {
	manager, _ := usageManager(t, func(ctx context.Context, tool, home string) AccountUsage {
		return readProviderUsage(ctx, tool, home)
	})
	if _, err := manager.CreateAccount("claude", "personal", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.CreateAccount("codex", "gone", ""); err != nil {
		t.Fatal(err)
	}
	if err := manager.ForgetAccount("codex", "gone"); err != nil {
		t.Fatal(err)
	}
	usage, err := manager.AccountUsage(context.Background(), "", "", false)
	if err != nil || len(usage) != 1 || usage[0].Tool != "claude" {
		t.Fatalf("usage = %#v, %v; want only the listed Claude account", usage, err)
	}
	// Claude usage is not connected yet, and the answer says so plainly.
	if usage[0].State != AccountUsageUnsupported || usage[0].CheckedAt != 0 || !strings.Contains(usage[0].Message, "not connected in Sessions yet") {
		t.Fatalf("claude usage = %#v", usage[0])
	}
	if _, err := manager.AccountUsage(context.Background(), "codex", "gone", false); err == nil || !strings.Contains(err.Error(), "sessions accounts") {
		t.Fatalf("forgotten account err = %v, want an instructional unknown-account error", err)
	}
}

type fakeUsageClient struct {
	account    *codexapp.Account
	accountErr error
	limits     codexapp.RateLimits
	limitsErr  error
}

func (f fakeUsageClient) PeekAccount(context.Context) (*codexapp.Account, error) {
	return f.account, f.accountErr
}

func (f fakeUsageClient) ReadRateLimits(context.Context) (codexapp.RateLimits, error) {
	return f.limits, f.limitsErr
}

func pointer[T any](value T) *T { return &value }

func TestCodexUsageKeepsBucketsSeparateAndResetsInMilliseconds(t *testing.T) {
	chatgpt := &codexapp.Account{Type: "chatgpt", Email: "a@example.test", PlanType: "team"}
	usage := readCodexUsage(context.Background(), fakeUsageClient{account: chatgpt, limits: codexapp.RateLimits{
		Legacy: &codexapp.RateLimitSnapshot{Primary: &codexapp.RateLimitWindow{UsedPercent: 99}},
		ByLimitID: map[string]codexapp.RateLimitSnapshot{
			"other": {Primary: &codexapp.RateLimitWindow{UsedPercent: 5}},
			"codex": {
				Primary:   &codexapp.RateLimitWindow{UsedPercent: 20, WindowDurationMins: pointer[int64](300), ResetsAt: pointer[int64](1_790_000_000)},
				Secondary: &codexapp.RateLimitWindow{UsedPercent: 60},
			},
		},
	}})
	if usage.State != AccountUsageAvailable || usage.Identity == nil || usage.Identity.AccountID != "" || usage.Identity.Email != "a@example.test" || usage.ReadAt == 0 {
		t.Fatalf("usage = %#v", usage)
	}
	if len(usage.Buckets) != 2 || usage.Buckets[0].LimitID != "codex" || usage.Buckets[1].LimitID != "other" {
		t.Fatalf("buckets = %#v, want both multi-bucket readings, codex first, legacy ignored", usage.Buckets)
	}
	primary := usage.Buckets[0].Windows[0]
	if primary.UsedPercent != 20 || *primary.ResetsAt != 1_790_000_000_000 || *primary.WindowMinutes != 300 {
		t.Fatalf("primary = %#v", primary)
	}
	if secondary := usage.Buckets[0].Windows[1]; secondary.ResetsAt != nil || secondary.WindowMinutes != nil {
		t.Fatalf("absent reset became a value: %#v", secondary)
	}

	legacy := readCodexUsage(context.Background(), fakeUsageClient{account: chatgpt, limits: codexapp.RateLimits{
		Legacy: &codexapp.RateLimitSnapshot{LimitID: pointer("codex"), Primary: &codexapp.RateLimitWindow{UsedPercent: 7}},
	}})
	if len(legacy.Buckets) != 1 || legacy.Buckets[0].Windows[0].UsedPercent != 7 {
		t.Fatalf("legacy fallback = %#v", legacy.Buckets)
	}
}

func TestCodexUsageStatesAreExplicitAndInstructional(t *testing.T) {
	chatgpt := &codexapp.Account{Type: "chatgpt", Email: "a@example.test"}
	older := readCodexUsage(context.Background(), fakeUsageClient{account: chatgpt, limitsErr: codexUnknownMethod()})
	if older.State != AccountUsageUnsupported || !strings.Contains(older.Message, "Update Codex") || older.Identity == nil {
		t.Fatalf("older codex = %#v", older)
	}
	failed := readCodexUsage(context.Background(), fakeUsageClient{account: chatgpt, limitsErr: errors.New("token=secret-value")})
	if failed.State != AccountUsageUnavailable || strings.Contains(failed.Message, "secret") || !strings.Contains(failed.Message, "Refresh") {
		t.Fatalf("failed read = %#v, want an instructional message without provider text", failed)
	}
	if out := readCodexUsage(context.Background(), fakeUsageClient{}); out.State != AccountUsageSignedOut {
		t.Fatalf("no account = %#v", out)
	}
	if key := readCodexUsage(context.Background(), fakeUsageClient{account: &codexapp.Account{Type: "apiKey"}}); key.State != AccountUsageUnsupported || key.Identity != nil {
		t.Fatalf("api key = %#v", key)
	}
	if broken := readCodexUsage(context.Background(), fakeUsageClient{accountErr: errors.New("boom")}); broken.State != AccountUsageUnavailable {
		t.Fatalf("identity failure = %#v, want unavailable rather than signed out", broken)
	}
}

type unknownMethodError struct{}

func (unknownMethodError) Error() string {
	return "Invalid request: unknown variant `account/rateLimits/read`"
}
func (unknownMethodError) RPCCode() int { return -32600 }

func codexUnknownMethod() error { return unknownMethodError{} }
