package codexapp

import (
	"context"
	"errors"
	"strings"
)

// RateLimitWindow is one rolling allowance window as the provider reports it.
// Every field except UsedPercent may be absent on a given version or plan.
type RateLimitWindow struct {
	UsedPercent        int    `json:"usedPercent"`
	WindowDurationMins *int64 `json:"windowDurationMins"`
	ResetsAt           *int64 `json:"resetsAt"`
}

type RateLimitCredits struct {
	HasCredits bool    `json:"hasCredits"`
	Unlimited  bool    `json:"unlimited"`
	Balance    *string `json:"balance"`
}

// RateLimitSnapshot is one metered bucket. Its account metadata is nullable.
type RateLimitSnapshot struct {
	LimitID              *string           `json:"limitId"`
	LimitName            *string           `json:"limitName"`
	PlanType             *string           `json:"planType"`
	Primary              *RateLimitWindow  `json:"primary"`
	Secondary            *RateLimitWindow  `json:"secondary"`
	Credits              *RateLimitCredits `json:"credits"`
	RateLimitReachedType *string           `json:"rateLimitReachedType"`
}

// RateLimits is the account/rateLimits/read answer. ByLimitID is the
// multi-bucket view and is preferred; Legacy mirrors the historical single
// bucket for app-server versions that do not report buckets.
type RateLimits struct {
	Legacy    *RateLimitSnapshot           `json:"rateLimits"`
	ByLimitID map[string]RateLimitSnapshot `json:"rateLimitsByLimitId"`
}

// ReadRateLimits asks the provider for the signed-in account's allowance. It
// starts no thread and no model turn.
func (c *Client) ReadRateLimits(ctx context.Context) (RateLimits, error) {
	var result RateLimits
	err := c.call(ctx, "account/rateLimits/read", struct{}{}, &result)
	return result, err
}

// MethodUnsupported reports that the app-server did not recognise a request
// method, which is how an older Codex answers a method it predates.
func MethodUnsupported(err error) bool {
	var rpc interface {
		error
		RPCCode() int
	}
	if !errors.As(err, &rpc) {
		return false
	}
	if rpc.RPCCode() == -32601 {
		return true
	}
	message := strings.ToLower(rpc.Error())
	return strings.Contains(message, "unknown variant") || strings.Contains(message, "method not found")
}

// RPCCode is the JSON-RPC error code the app-server answered with.
func (e rpcError) RPCCode() int { return e.Code }
