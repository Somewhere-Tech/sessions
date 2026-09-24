package session

import (
	"context"
	"errors"
	"sort"
	"strings"
	"sync"
	"time"
)

// Account usage is the provider's own reading of a subscription's allowance:
// how much of each rolling window is used and when it resets. Sessions asks the
// provider, through the same private provider home and environment sign-in
// uses; it never opens a credential, calls a provider endpoint itself, or
// starts a model turn.
//
// A reading is a fact about one moment. A read that fails, times out, or is not
// offered by a provider says so. It keeps the last good reading beside it as
// stale only when the same read showed the same account still signed in;
// otherwise the answer is unknown rather than another account's quota. A
// sign-in, recheck or removal through Sessions discards every earlier reading
// of that home, including one still in flight.

const (
	// accountUsageTTL is how long a reading answers repeat requests before the
	// provider is asked again.
	accountUsageTTL = 60 * time.Second
	// accountUsageMinRefresh bounds an explicit refresh, so a hurried button
	// or script cannot start one provider process per click.
	accountUsageMinRefresh = 10 * time.Second
	// accountUsageDeadline bounds one provider read from process start to the
	// last answer. The read continues past a caller who stops waiting, so the
	// next request finds it.
	accountUsageDeadline = 25 * time.Second
	// accountUsageParallel is how many provider processes may read at once.
	accountUsageParallel = 2
)

// Usage states. Only AccountUsageAvailable carries a fresh reading.
const (
	AccountUsageAvailable   = "available"
	AccountUsageSignedOut   = "signed_out"
	AccountUsageUnsupported = "unsupported"
	AccountUsageUnavailable = "unavailable"
)

type AccountUsageWindow struct {
	// Kind is primary or secondary, as the provider orders its windows.
	Kind        string `json:"kind"`
	UsedPercent int    `json:"used_percent"`
	// WindowMinutes and ResetsAt are absent when the provider did not say.
	WindowMinutes *int64 `json:"window_minutes,omitempty"`
	ResetsAt      *int64 `json:"resets_at,omitempty"`
}

type AccountUsageCredits struct {
	HasCredits bool   `json:"has_credits"`
	Unlimited  bool   `json:"unlimited"`
	Balance    string `json:"balance,omitempty"`
}

// AccountUsageBucket is one metered allowance. Buckets are separate limits and
// are never added together.
type AccountUsageBucket struct {
	LimitID   string               `json:"limit_id,omitempty"`
	LimitName string               `json:"limit_name,omitempty"`
	Plan      string               `json:"plan,omitempty"`
	Reached   string               `json:"reached,omitempty"`
	Windows   []AccountUsageWindow `json:"windows"`
	Credits   *AccountUsageCredits `json:"credits,omitempty"`
}

type AccountUsage struct {
	Tool  string `json:"tool"`
	Name  string `json:"name"`
	Label string `json:"label,omitempty"`
	State string `json:"state"`
	// Message explains any state other than available, and what to do next.
	Message string `json:"message,omitempty"`
	// CheckedAt is when the provider was last asked; zero when it never was.
	CheckedAt int64 `json:"checked_at,omitempty"`
	// Identity is what the provider reported during that read, not a copy of
	// the account's saved identity.
	Identity *AccountIdentity     `json:"identity,omitempty"`
	Buckets  []AccountUsageBucket `json:"buckets,omitempty"`
	// ReadAt is when Buckets were read. Stale means they come from an earlier
	// read than CheckedAt because the latest one did not produce a reading.
	ReadAt int64 `json:"read_at,omitempty"`
	Stale  bool  `json:"stale,omitempty"`
}

// accountUsageReader asks one provider home for its reading.
type accountUsageReader func(ctx context.Context, tool, home string) AccountUsage

type accountUsageEntry struct {
	result    AccountUsage
	lastGood  *AccountUsage
	fetchedAt time.Time
	inflight  chan struct{}
	// invalidated fences a read that started before the home's sign-in
	// changed: its answer reaches only the callers already waiting on it, as
	// unknown, and is never cached.
	invalidated bool
}

type accountUsageCache struct {
	mu      sync.Mutex
	entries map[string]*accountUsageEntry
	slots   chan struct{}
	// reader replaces the provider process in tests.
	reader accountUsageReader
	now    func() time.Time
}

func (c *accountUsageCache) clock() time.Time {
	if c.now != nil {
		return c.now()
	}
	return time.Now()
}

// AccountUsage reads every listed account on this computer, or the one named by
// tool and name. Accounts whose provider has not answered by the time ctx ends
// are reported as unavailable with any earlier reading, never omitted.
func (m *Manager) AccountUsage(ctx context.Context, tool, name string, refresh bool) ([]AccountUsage, error) {
	profiles, err := m.Profiles(ctx)
	if err != nil {
		return nil, err
	}
	selected := make([]ProfileStatus, 0, len(profiles))
	for _, profile := range profiles {
		if (tool == "" || profile.Tool == tool) && (name == "" || profile.Name == name) {
			selected = append(selected, profile)
		}
	}
	if name != "" && len(selected) == 0 {
		return nil, errors.New("unknown account; `sessions accounts` lists the accounts on this computer")
	}
	m.accountUsage.forget(profiles)
	results := make([]AccountUsage, len(selected))
	var wait sync.WaitGroup
	for index, profile := range selected {
		wait.Add(1)
		go func() {
			defer wait.Done()
			results[index] = m.accountUsage.get(ctx, m.ctx, profile, refresh)
		}()
	}
	wait.Wait()
	sort.SliceStable(results, func(i, j int) bool {
		if results[i].Tool != results[j].Tool {
			return results[i].Tool < results[j].Tool
		}
		return results[i].Name < results[j].Name
	})
	return results, nil
}

// invalidate discards every reading of one home after its sign-in changed. A
// read still in flight is fenced off; the next request starts a new one, so the
// refresh floor cannot answer with the previous sign-in's quota.
func (c *accountUsageCache) invalidate(tool, name string) {
	key := tool + "/" + name
	c.mu.Lock()
	defer c.mu.Unlock()
	if entry := c.entries[key]; entry != nil {
		entry.invalidated = true
		delete(c.entries, key)
	}
}

// forget drops readings for accounts no longer listed, so the cache is bounded
// by the accounts this computer actually has.
func (c *accountUsageCache) forget(listed []ProfileStatus) {
	keep := make(map[string]bool, len(listed))
	for _, profile := range listed {
		keep[profile.Tool+"/"+profile.Name] = true
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	for key := range c.entries {
		if !keep[key] {
			delete(c.entries, key)
		}
	}
}

func (c *accountUsageCache) get(ctx, parent context.Context, profile ProfileStatus, refresh bool) AccountUsage {
	key := profile.Tool + "/" + profile.Name
	c.mu.Lock()
	if c.entries == nil {
		c.entries = make(map[string]*accountUsageEntry)
	}
	if c.slots == nil {
		c.slots = make(chan struct{}, accountUsageParallel)
	}
	entry := c.entries[key]
	if entry == nil {
		entry = &accountUsageEntry{}
		c.entries[key] = entry
	}
	age := c.clock().Sub(entry.fetchedAt)
	fresh := !entry.fetchedAt.IsZero() && ((age < accountUsageTTL && !refresh) || age < accountUsageMinRefresh)
	if fresh && entry.inflight == nil {
		result := entry.result
		c.mu.Unlock()
		return labelled(result, profile)
	}
	if entry.inflight == nil {
		entry.inflight = make(chan struct{})
		go c.fetch(parent, entry, profile)
	}
	done := entry.inflight
	c.mu.Unlock()
	select {
	case <-done:
		c.mu.Lock()
		result := entry.result
		c.mu.Unlock()
		return labelled(result, profile)
	case <-ctx.Done():
		c.mu.Lock()
		result := pendingUsage(entry, profile)
		c.mu.Unlock()
		return labelled(result, profile)
	}
}

// fetch runs one provider read for every caller waiting on this account, on a
// context of its own so a caller who stops waiting does not cancel the others.
func (c *accountUsageCache) fetch(parent context.Context, entry *accountUsageEntry, profile ProfileStatus) {
	if parent == nil {
		parent = context.Background()
	}
	ctx, cancel := context.WithTimeout(parent, accountUsageDeadline)
	defer cancel()
	var result AccountUsage
	select {
	case c.slots <- struct{}{}:
		reader := c.reader
		if reader == nil {
			reader = readProviderUsage
		}
		result = reader(ctx, profile.Tool, profile.Path)
		<-c.slots
	case <-ctx.Done():
		result = AccountUsage{State: AccountUsageUnavailable, Message: "Sessions was busy reading other accounts; refresh to try again."}
	}
	result.Tool, result.Name = profile.Tool, profile.Name
	if result.CheckedAt == 0 && result.State != AccountUsageUnsupported {
		result.CheckedAt = c.clock().UnixMilli()
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	entry.result, entry.fetchedAt = settleUsage(entry, result), c.clock()
	close(entry.inflight)
	entry.inflight = nil
}

// settleUsage decides what one finished read means for this home's history.
func settleUsage(entry *accountUsageEntry, result AccountUsage) AccountUsage {
	if entry.invalidated {
		return AccountUsage{
			Tool: result.Tool, Name: result.Name, State: AccountUsageUnavailable, CheckedAt: result.CheckedAt,
			Message: "This account's sign-in changed while it was being read. Refresh to read it again.",
		}
	}
	switch result.State {
	case AccountUsageAvailable:
		good := result
		entry.lastGood = &good
	case AccountUsageSignedOut:
		// A provider that reports no sign-in has no allowance to show; an
		// earlier reading belonged to a sign-in that is gone.
		entry.lastGood = nil
	default:
		if entry.lastGood != nil && result.Identity != nil && !sameAccount(result.Identity, entry.lastGood.Identity) {
			// Another account is signed in here now: the old quota is not its.
			entry.lastGood = nil
		}
		result = withLastReading(result, entry.lastGood)
	}
	return result
}

// pendingUsage answers a caller whose deadline arrived first.
func pendingUsage(entry *accountUsageEntry, profile ProfileStatus) AccountUsage {
	result := AccountUsage{
		Tool: profile.Tool, Name: profile.Name, State: AccountUsageUnavailable,
		Message: "The provider has not answered yet; refresh in a moment.",
	}
	return withLastReading(result, entry.lastGood)
}

// withLastReading keeps an earlier reading beside a failed one, marked stale,
// only when this attempt showed the same account the reading was taken for.
// Without that proof the answer stays unknown. The reading keeps its own
// read_at; Identity is the matching account this attempt reported.
func withLastReading(result AccountUsage, good *AccountUsage) AccountUsage {
	if good == nil || !sameAccount(result.Identity, good.Identity) {
		return result
	}
	result.Buckets, result.ReadAt, result.Stale = good.Buckets, good.ReadAt, true
	return result
}

// sameAccount is true only when both identities are known and agree on every
// field that tells accounts apart.
func sameAccount(left, right *AccountIdentity) bool {
	return left != nil && right != nil && left.Email != "" &&
		strings.EqualFold(left.Email, right.Email) &&
		left.AccountID == right.AccountID && left.Organization == right.Organization
}

func labelled(result AccountUsage, profile ProfileStatus) AccountUsage {
	result.Tool, result.Name, result.Label = profile.Tool, profile.Name, profile.Label
	return result
}
