package billing

import (
	"strings"
	"testing"
	"time"
)

func TestOAuthModelTierBillingAndQuota(t *testing.T) {
	for _, tt := range []struct {
		name, responseModel, upstream, route, request, response, fallback string
		disabled, longContext, failed, zero                               bool
		want                                                              float64
	}{
		{name: "5.6", responseModel: "gpt-5.6", response: "priority", want: 2},
		{name: "5.6 sol", responseModel: "gpt-5.6-sol", response: "priority", want: 2},
		{name: "5.6 terra snapshot", responseModel: "gpt-5.6-terra-2026-09-25", response: "priority", want: 2},
		{name: "5.6 luna", responseModel: "gpt-5.6-luna", response: "priority", want: 2},
		{name: "6", responseModel: "gpt-6", response: "priority", want: 2},
		{name: "6 astra", responseModel: "gpt-6-astra", response: "priority", want: 2},
		{name: "6 sol normalized", responseModel: " Vendor/GPT-6-SOL(high) ", response: " FAST ", want: 2},
		{name: "6 luna", responseModel: "gpt-6-luna", response: "priority", want: 2},
		{name: "response beats upstream", responseModel: "gpt-5.5", upstream: "gpt-6-sol", response: "priority", want: 2.5},
		{name: "upstream beats pricing alias", upstream: "gpt-6-sol", response: "priority", want: 2},
		{name: "billing model fallback", route: "gpt-6-sol", response: "priority", want: 2},
		{name: "60 is not 6", responseModel: "gpt-60", response: "priority", want: 2.5},
		{name: "5.60 is not 5.6", responseModel: "gpt-5.60", response: "priority", want: 2.5},
		{name: "unmatched model", responseModel: "unknown-model", response: "priority", want: 2.5},
		{name: "downgrade", responseModel: "gpt-6-sol", request: "priority", response: "default", want: 1},
		{name: "standard synonym", responseModel: "gpt-6-sol", request: "priority", response: "standard", want: 1},
		{name: "upgrade", responseModel: "gpt-6-sol", request: "auto", response: "priority", want: 2},
		{name: "unknown response", responseModel: "gpt-6-sol", request: "priority", response: "future", fallback: "unknown_response_tier_request_fallback", want: 2},
		{name: "unknown response 5.6 fast", responseModel: " Vendor/GPT-5.6-SOL(high) ", request: " FAST ", response: " FUTURE ", fallback: "unknown_response_tier_request_fallback", want: 2},
		{name: "unknown response upstream model", upstream: "gpt-6-sol", request: "priority", response: "future", fallback: "unknown_response_tier_request_fallback", want: 2},
		{name: "unknown response pricing model", route: "gpt-5.6-sol", request: "priority", response: "future", fallback: "unknown_response_tier_request_fallback", want: 2},
		{name: "unknown response disabled", responseModel: "gpt-6-sol", request: "priority", response: "future", disabled: true, fallback: "unknown_response_tier_request_fallback", want: 1},
		{name: "unknown response long context", responseModel: "gpt-6-sol", request: "priority", response: "future", longContext: true, fallback: "unknown_response_tier_request_fallback", want: 2},
		{name: "unknown response failure", responseModel: "gpt-6-sol", request: "priority", response: "future", failed: true, fallback: "unknown_response_tier_request_fallback", want: 2},
		{name: "unknown response zero usage", responseModel: "gpt-6-sol", request: "priority", response: "future", failed: true, zero: true, fallback: "unknown_response_tier_request_fallback", want: 2},
		{name: "unknown response legacy model", responseModel: "gpt-5.5", upstream: "gpt-6-sol", request: "priority", response: "future", fallback: "unknown_response_tier", want: 1},
		{name: "unknown response similar model", responseModel: "gpt-60", request: "priority", response: "future", fallback: "unknown_response_tier", want: 1},
		{name: "unknown response auto", responseModel: "gpt-6-sol", request: "auto", response: "future", fallback: "unknown_response_tier", want: 1},
		{name: "unknown response default", responseModel: "gpt-6-sol", request: "default", response: "future", fallback: "unknown_response_tier", want: 1},
		{name: "unknown response flex", responseModel: "gpt-6-sol", request: "flex", response: "future", fallback: "unknown_response_tier", want: 1},
		{name: "unknown response missing request", responseModel: "gpt-6-sol", response: "future", fallback: "unknown_response_tier", want: 1},
		{name: "missing response", responseModel: "gpt-6-sol", request: "fast", fallback: "missing_response_tier", want: 2},
		{name: "missing both", responseModel: "gpt-6-sol", fallback: "missing_response_tier", want: 1},
		{name: "unknown request", responseModel: "gpt-6-sol", request: "future", fallback: "unknown_request_tier", want: 1},
		{name: "disabled", responseModel: "gpt-6-sol", response: "priority", disabled: true, want: 1},
		{name: "flex while disabled", responseModel: "gpt-6-sol", request: "priority", response: "flex", disabled: true, want: .5},
		{name: "flex downgraded", responseModel: "gpt-6-sol", request: "flex", response: "default", want: 1},
		{name: "long context", responseModel: "gpt-6-sol", response: "priority", longContext: true, want: 2},
		{name: "failed with usage", responseModel: "gpt-6-sol", response: "priority", failed: true, want: 2},
		{name: "failed without usage", responseModel: "gpt-6-sol", response: "priority", failed: true, zero: true, want: 2},
	} {
		t.Run(tt.name, func(t *testing.T) {
			now := time.Date(2026, 9, 25, 0, 0, 0, 0, time.UTC)
			store := newAccountStore(t, now)
			cfg := store.Config()
			cfg.BillingMultiplier, cfg.CodexFastModeBilling = .2, !tt.disabled
			if err := store.Configure(cfg); err != nil {
				t.Fatal(err)
			}
			route := tt.route
			if route == "" {
				route = "gpt-5.5"
			}
			price := PriceRates{InputPer1M: 1, OutputPer1M: 2, CacheReadPer1M: floatPtr(.1), CacheWritePer1M: floatPtr(1.25),
				LongContext:  &LongContextPrice{ThresholdInputTokens: 1000, InputPer1M: 5, OutputPer1M: 10, CacheReadPer1M: floatPtr(.5), CacheWritePer1M: floatPtr(6.25)},
				ServiceTiers: map[string]TierPriceRates{"priority": {InputPer1M: floatPtr(99), OutputPer1M: floatPtr(99)}},
			}
			if _, err := store.UpsertPrice(CustomPrice{ModelID: route, PriceRates: price}); err != nil {
				t.Fatal(err)
			}
			store.ReplaceAll(func(state *State) {
				state.Plans = []Plan{{ID: "p", Windows: []QuotaWindow{{ID: "day", AmountUSD: 10, PeriodSeconds: 86400}}}}
				state.Keys["scope-a"] = &KeyState{PlanID: "p"}
			})
			event := admittedEvent(store, "scope-a", now)
			event.Provider, event.AuthType = " CoDeX ", " OAUTH "
			event.ResponseModel, event.UpstreamModel, event.RouteModel = tt.responseModel, tt.upstream, route
			event.ServiceTier, event.ResponseServiceTier = tt.request, tt.response
			baseAmount := wantSubsetCost
			if tt.longContext {
				event.Breakdown = completeBreakdown(501, 400, 100, 500, 200)
				baseAmount = (501*5 + 400*.5 + 100*6.25 + 500*10) / 1_000_000
			}
			if tt.zero {
				event.Breakdown = completeBreakdown(0, 0, 0, 0, 0)
				baseAmount = 0
			}
			if tt.failed {
				store.RecordUsageError(event, RequestError{StatusCode: 502, Body: "dummy failure"})
			} else {
				store.RecordUsage(event)
			}
			entry := mustRequestEvents(t, store, RequestEventQuery{}).Entries[0]
			cost := entry.Cost
			assertClose(t, "amount", cost.TotalUSD, baseAmount*.2*tt.want)
			assertClose(t, "multiplier", cost.Multiplier, .2*tt.want)
			if cost.BillingMultiplier != .2 || cost.ServiceTierMultiplier != tt.want || cost.LongContext != tt.longContext || entry.Failed != tt.failed {
				t.Fatalf("incorrect accounting: %+v", entry)
			}
			if cost.Pricing.Method != "oauth_multiplier" || cost.Pricing.RuleVersion != "codex-oauth-family-v2" || cost.Pricing.TierFallback != tt.fallback {
				t.Fatal(cost.Pricing)
			}
			if tt.fallback == "unknown_response_tier_request_fallback" && (cost.Pricing.ServiceTier != "priority" || cost.Pricing.TierSource != "request") {
				t.Fatal("unknown response must retain the requested Priority tier as an estimate", cost.Pricing)
			}
			if entry.ServiceTier != tt.request || entry.ResponseServiceTier != strings.TrimSpace(tt.response) {
				t.Fatal("billing resolution changed the host-reported tiers", entry)
			}
			store.Read(func(state *State) {
				cycle := state.Keys["scope-a"].Cycles["day"]
				assertClose(t, "quota spent", cycle.SpentUSD, cost.TotalUSD)
				wantRequests := int64(1)
				if tt.failed {
					wantRequests = 0
				}
				if cycle.UsedTokens != event.Breakdown.TotalTokens || cycle.UsedRequests != wantRequests {
					t.Fatal(cycle)
				}
			})
		})
	}
}
