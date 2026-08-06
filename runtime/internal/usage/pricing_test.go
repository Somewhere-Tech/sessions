package usage

import (
	"math"
	"testing"
)

func TestPriceMatchesCCUsageFastAndLongContextSemantics(t *testing.T) {
	tests := []struct {
		name   string
		model  string
		tokens Tokens
		fast   bool
		want   float64
	}{
		{
			name:   "gpt-5.6-sol short fast",
			model:  "gpt-5.6-sol",
			tokens: Tokens{Input: 1_000, Output: 100, CacheRead: 10_000},
			fast:   true,
			want:   0.026,
		},
		{
			name:   "gpt-5.6-sol long fast",
			model:  "gpt-5.6-sol",
			tokens: Tokens{Input: 300_000, Output: 10_000},
			fast:   true,
			want:   6.9,
		},
		{
			name:   "gpt-5.5 distinct fast multiplier",
			model:  "gpt-5.5",
			tokens: Tokens{Input: 1_000, Output: 100},
			fast:   true,
			want:   0.02,
		},
		{
			name:   "claude sonnet cache buckets",
			model:  "claude-sonnet-4-6",
			tokens: Tokens{Input: 1_000, Output: 100, CacheCreation: 2_000, CacheRead: 10_000, Reasoning: 50},
			want:   0.015,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, found := price(test.model, test.tokens, test.fast)
			if !found {
				t.Fatalf("price(%q) was not found", test.model)
			}
			if math.Abs(got-test.want) > 1e-12 {
				t.Fatalf("price(%q) = %.12f, want %.12f", test.model, got, test.want)
			}
		})
	}
}
