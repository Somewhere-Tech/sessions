package session

import (
	"context"
	"errors"
	"sort"
	"time"

	"github.com/somewhere-tech/sessions/runtime/internal/agentcall"
	"github.com/somewhere-tech/sessions/runtime/internal/codexapp"
)

// readProviderUsage is the supported reading for each provider. Only Codex
// offers one: its app-server answers account/rateLimits/read for the signed-in
// ChatGPT account. Claude usage is not connected in Sessions yet; Sessions does
// not scrape it from Claude's files, endpoints or status line in the meantime.
func readProviderUsage(ctx context.Context, tool, home string) AccountUsage {
	if tool != "codex" {
		return AccountUsage{
			State:   AccountUsageUnsupported,
			Message: "Claude usage is not connected in Sessions yet. Check Claude for your current limits.",
		}
	}
	executable, err := agentcall.Executable("codex")
	if err != nil {
		return AccountUsage{State: AccountUsageUnavailable, Message: "Codex is not installed on this computer; install it to read this account's usage."}
	}
	// The same private environment sign-in uses: this home, nothing ambient.
	client, err := codexapp.NewAccountClient(ctx, executable, accountLoginEnvironment("codex", home), home)
	if err != nil {
		return AccountUsage{State: AccountUsageUnavailable, Message: usageFailure(ctx, "Codex did not start")}
	}
	defer client.Close()
	return readCodexUsage(ctx, client)
}

// codexUsageClient is the part of the app-server a usage read needs.
type codexUsageClient interface {
	PeekAccount(context.Context) (*codexapp.Account, error)
	ReadRateLimits(context.Context) (codexapp.RateLimits, error)
}

func readCodexUsage(ctx context.Context, client codexUsageClient) AccountUsage {
	account, err := client.PeekAccount(ctx)
	checked := time.Now().UnixMilli()
	if err != nil {
		return AccountUsage{State: AccountUsageUnavailable, CheckedAt: checked, Message: usageFailure(ctx, "Codex could not report which account is signed in")}
	}
	if account == nil {
		return AccountUsage{State: AccountUsageSignedOut, CheckedAt: checked, Message: "Codex reports no sign-in for this account. Sign in to see its usage."}
	}
	if account.Type != "chatgpt" {
		return AccountUsage{State: AccountUsageUnsupported, CheckedAt: checked, Message: "This account uses an API key, which has no ChatGPT plan allowance to show."}
	}
	identity := &AccountIdentity{Email: account.Email, Plan: account.PlanType, CheckedAt: checked}
	limits, err := client.ReadRateLimits(ctx)
	checked = time.Now().UnixMilli()
	if codexapp.MethodUnsupported(err) {
		return AccountUsage{
			State: AccountUsageUnsupported, CheckedAt: checked, Identity: identity,
			Message: "This version of Codex cannot report usage. Update Codex on this computer to see it.",
		}
	}
	if err != nil {
		return AccountUsage{
			State: AccountUsageUnavailable, CheckedAt: checked, Identity: identity,
			Message: usageFailure(ctx, "Codex could not read this account's usage; if this continues, sign in again"),
		}
	}
	return AccountUsage{
		State: AccountUsageAvailable, CheckedAt: checked, ReadAt: checked, Identity: identity,
		Buckets: codexUsageBuckets(limits),
	}
}

// usageFailure names the failed step and the next action without repeating the
// provider's own error text, which may carry authentication details.
func usageFailure(ctx context.Context, step string) string {
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return step + " before the time limit. Refresh to try again."
	}
	return step + ". Refresh to try again."
}

// codexUsageBuckets prefers the multi-bucket view and falls back to the legacy
// single bucket. Buckets stay separate, in a stable order with codex first.
func codexUsageBuckets(limits codexapp.RateLimits) []AccountUsageBucket {
	buckets := make([]AccountUsageBucket, 0, len(limits.ByLimitID)+1)
	if len(limits.ByLimitID) > 0 {
		ids := make([]string, 0, len(limits.ByLimitID))
		for id := range limits.ByLimitID {
			ids = append(ids, id)
		}
		sort.Slice(ids, func(i, j int) bool {
			if (ids[i] == "codex") != (ids[j] == "codex") {
				return ids[i] == "codex"
			}
			return ids[i] < ids[j]
		})
		for _, id := range ids {
			bucket := codexUsageBucket(limits.ByLimitID[id])
			if bucket.LimitID == "" {
				bucket.LimitID = id
			}
			buckets = append(buckets, bucket)
		}
		return buckets
	}
	if limits.Legacy != nil {
		if bucket := codexUsageBucket(*limits.Legacy); len(bucket.Windows) > 0 || bucket.Credits != nil {
			buckets = append(buckets, bucket)
		}
	}
	return buckets
}

func codexUsageBucket(snapshot codexapp.RateLimitSnapshot) AccountUsageBucket {
	bucket := AccountUsageBucket{
		LimitID: usageText(snapshot.LimitID), LimitName: usageText(snapshot.LimitName),
		Plan: usageText(snapshot.PlanType), Reached: usageText(snapshot.RateLimitReachedType),
		Windows: make([]AccountUsageWindow, 0, 2),
	}
	for _, window := range []struct {
		kind  string
		value *codexapp.RateLimitWindow
	}{{"primary", snapshot.Primary}, {"secondary", snapshot.Secondary}} {
		if window.value == nil {
			continue
		}
		reading := AccountUsageWindow{Kind: window.kind, UsedPercent: window.value.UsedPercent, WindowMinutes: window.value.WindowDurationMins}
		if window.value.ResetsAt != nil {
			// The provider reports seconds; every Sessions timestamp is milliseconds.
			resets := *window.value.ResetsAt * 1000
			reading.ResetsAt = &resets
		}
		bucket.Windows = append(bucket.Windows, reading)
	}
	if credits := snapshot.Credits; credits != nil {
		bucket.Credits = &AccountUsageCredits{HasCredits: credits.HasCredits, Unlimited: credits.Unlimited, Balance: usageText(credits.Balance)}
	}
	return bucket
}

func usageText(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}
