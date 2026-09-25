package billing

import (
	"testing"
	"time"
)

func TestUsageBillingMultiplierAndServiceTierEligibility(t *testing.T) {
	for _, test := range []struct {
		name, provider, authType, tier string
		fastDisabled                   bool
		wantService                    float64
	}{
		{"ordinary", "codex", "oauth", "auto", false, 1},
		{"fast by default", "codex", "oauth", "priority", false, 2.5},
		{"fast case normalization", "CoDeX", "OAUTH", " Priority ", false, 2.5},
		{"explicit opt out", "codex", "oauth", "priority", true, 1},
		{"codex API key", "codex", "apikey", "priority", false, 1},
		{"other provider", "claude", "oauth", "priority", false, 1},
		{"flex Codex OAuth", "codex", "oauth", "flex", false, 0.5},
		{"flex OpenAI API key", "openai", "apikey", "flex", false, 0.5},
		{"flex compatible provider", "openai-compatible-test", "apikey", "flex", false, 0.5},
		{"flex case normalization", "CoDeX", "OAUTH", " Flex ", false, 0.5},
		{"flex with fast billing disabled", "codex", "oauth", "flex", true, 0.5},
		{"unknown tier", "codex", "oauth", "unknown", false, 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			now := time.Date(2026, 9, 20, 0, 0, 0, 0, time.UTC)
			store := newAccountStore(t, now)
			cfg := store.Config()
			cfg.BillingMultiplier = 0.2
			if test.fastDisabled {
				cfg.CodexFastModeBilling = false
			}
			if err := store.Configure(cfg); err != nil {
				t.Fatal(err)
			}
			store.ReplaceAll(func(state *State) {
				state.Plans = []Plan{{ID: "p", Windows: []QuotaWindow{{ID: "day", AmountUSD: 10, PeriodSeconds: 86400}}}}
				state.Keys["scope-a"] = &KeyState{PlanID: "p"}
			})
			event := admittedEvent(store, "scope-a", now)
			event.Provider, event.AuthType, event.ServiceTier = test.provider, test.authType, test.tier
			store.RecordUsage(event)
			entries := mustRequestEvents(t, store, RequestEventQuery{}).Entries
			if len(entries) != 1 {
				t.Fatalf("entries = %d", len(entries))
			}
			cost := entries[0].Cost
			factor := 0.2 * test.wantService
			wantService := test.wantService
			if apiTierEligible(event) {
				wantService = 1
			}
			if cost.BillingMultiplier != 0.2 || cost.ServiceTierMultiplier != wantService || cost.Multiplier != 0.2*wantService {
				t.Fatalf("cost multipliers = %+v", cost)
			}
			assertClose(t, "total", cost.TotalUSD, wantSubsetCost*factor)
			assertClose(t, "input", cost.UncachedInputUSD, 0.0005*factor)
			assertClose(t, "cache read", cost.CacheReadUSD, 0.00004*factor)
			assertClose(t, "cache write", cost.CacheWriteUSD, 0.000125*factor)
			assertClose(t, "output", cost.OutputUSD, 0.001*factor)
			assertClose(t, "input rate", cost.AppliedInputPer1M, factor)
			assertClose(t, "output rate", cost.AppliedOutputPer1M, 2*factor)
			assertClose(t, "cache read rate", cost.AppliedCacheReadPer1M, 0.1*factor)
			assertClose(t, "cache write rate", cost.AppliedCacheWritePer1M, 1.25*factor)
			if cost.UncachedInputTokens != 500 || cost.CacheReadTokens != 400 || cost.CacheWriteTokens != 100 || cost.BilledOutputTokens != 500 {
				t.Fatalf("multiplier altered token counts: %+v", cost)
			}
			store.Read(func(state *State) {
				cycle := state.Keys["scope-a"].Cycles["day"]
				assertClose(t, "quota spent", cycle.SpentUSD, cost.TotalUSD)
				if cycle.UsedTokens != 1500 || cycle.UsedRequests != 1 || state.Plans[0].Windows[0].AmountUSD != 10 || state.Prices["gpt-5.5"].InputPer1M != 1 {
					t.Fatalf("multiplier changed base prices or quota dimensions: %+v", cycle)
				}
			})
		})
	}
}

func TestBillingReconfigurationPreservesPastEvents(t *testing.T) {
	now := time.Date(2026, 9, 20, 0, 0, 0, 0, time.UTC)
	store := newAccountStore(t, now)
	cfg := store.Config()
	cfg.BillingMultiplier = 0.2
	if err := store.Configure(cfg); err != nil {
		t.Fatal(err)
	}
	store.RecordUsage(subsetEvent("scope-a", now))
	original := mustRequestEvents(t, store, RequestEventQuery{}).Entries[0]
	cfg.BillingMultiplier = 0.3
	if err := store.Configure(cfg); err != nil {
		t.Fatal(err)
	}
	store.RecordUsage(subsetEvent("scope-a", now.Add(time.Minute)))
	entries := mustRequestEvents(t, store, RequestEventQuery{}).Entries
	if len(entries) != 2 || entries[1].Cost != original.Cost {
		t.Fatalf("configuration repriced existing bill: %+v", entries)
	}
	assertClose(t, "new cost", entries[0].Cost.TotalUSD, wantSubsetCost*0.3)
	if entries[0].Cost.BillingMultiplier != 0.3 || entries[1].Cost.BillingMultiplier != 0.2 {
		t.Fatalf("historical policy lost: %+v", entries)
	}
}

func TestUsageMultiplierPreservesLongContextTierAndBasePrice(t *testing.T) {
	now := time.Date(2026, 9, 20, 0, 0, 0, 0, time.UTC)
	store := newAccountStore(t, now)
	cfg := store.Config()
	cfg.BillingMultiplier = 0.2
	if err := store.Configure(cfg); err != nil {
		t.Fatal(err)
	}
	store.ReplaceAll(func(state *State) {
		price := state.Prices["gpt-5.5"]
		price.LongContext = &LongContextPrice{ThresholdInputTokens: 1000, InputPer1M: 5, OutputPer1M: 10, CacheReadPer1M: floatPtr(0.5), CacheWritePer1M: floatPtr(6.25)}
		state.Prices["gpt-5.5"] = price
	})
	event := subsetEvent("scope-a", now)
	event.Provider, event.AuthType, event.ServiceTier = "codex", "oauth", "priority"
	event.Breakdown = completeBreakdown(501, 400, 100, 500, 200)
	store.RecordUsage(event)
	cost := mustRequestEvents(t, store, RequestEventQuery{}).Entries[0].Cost
	if !cost.Tiered || !cost.LongContext || cost.ThresholdInputTokens != 1000 || cost.Multiplier != 0.5 {
		t.Fatalf("tier data changed: %+v", cost)
	}
	assertClose(t, "long input rate", cost.AppliedInputPer1M, 2.5)
	assertClose(t, "long output rate", cost.AppliedOutputPer1M, 5)
	assertClose(t, "long cache read rate", cost.AppliedCacheReadPer1M, 0.25)
	assertClose(t, "long cache write rate", cost.AppliedCacheWritePer1M, 3.125)
	assertClose(t, "long total", cost.TotalUSD, (501*5+400*0.5+100*6.25+500*10)/1_000_000*0.5)
	if price := store.state.Prices["gpt-5.5"].LongContext; price.InputPer1M != 5 || price.ThresholdInputTokens != 1000 {
		t.Fatalf("base price table changed: %+v", price)
	}
}

func TestUsageMultiplierKeepsFailedAndUnbillableRecords(t *testing.T) {
	for _, test := range []struct {
		name, tier   string
		breakdown    TokenBreakdown
		unknownModel bool
		wantCost     float64
		wantService  float64
	}{
		{"reported usage on failure", "priority", completeBreakdown(500, 400, 100, 500, 200), false, wantSubsetCost * 0.5, 2.5},
		{"zero usage", "priority", completeBreakdown(0, 0, 0, 0, 0), false, 0, 2.5},
		{"unclassified usage", "priority", TokenBreakdown{Quality: TokenAccountingUnclassified, TotalTokens: 10, UnclassifiedTokens: 10}, false, 0, 1},
		{"invalid usage", "priority", TokenBreakdown{Quality: TokenAccountingComplete, TotalTokens: -1}, false, 0, 1},
		{"missing price", "priority", completeBreakdown(500, 400, 100, 500, 200), true, 0, 1},
		{"flex reported usage on failure", "flex", completeBreakdown(500, 400, 100, 500, 200), false, wantSubsetCost * 0.1, 0.5},
		{"flex zero usage", "flex", completeBreakdown(0, 0, 0, 0, 0), false, 0, 0.5},
		{"flex unclassified usage", "flex", TokenBreakdown{Quality: TokenAccountingUnclassified, TotalTokens: 10, UnclassifiedTokens: 10}, false, 0, 1},
		{"flex invalid usage", "flex", TokenBreakdown{Quality: TokenAccountingComplete, TotalTokens: -1}, false, 0, 1},
		{"flex missing price", "flex", completeBreakdown(500, 400, 100, 500, 200), true, 0, 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			now := time.Date(2026, 9, 20, 0, 0, 0, 0, time.UTC)
			store := newAccountStore(t, now)
			cfg := store.Config()
			cfg.BillingMultiplier = 0.2
			if err := store.Configure(cfg); err != nil {
				t.Fatal(err)
			}
			event := subsetEvent("scope-a", now)
			event.Provider, event.AuthType, event.ServiceTier = "codex", "oauth", test.tier
			event.Breakdown = test.breakdown
			if test.unknownModel {
				event.UpstreamModel, event.RouteModel = "unknown-model", "unknown-model"
			}
			store.RecordUsageError(event, RequestError{StatusCode: 502})
			entries := mustRequestEvents(t, store, RequestEventQuery{}).Entries
			if len(entries) != 1 || !entries[0].Failed || entries[0].Cost.BillingMultiplier != 0.2 || entries[0].Cost.ServiceTierMultiplier != test.wantService {
				t.Fatalf("failure event or billing policy lost: %+v", entries)
			}
			assertClose(t, "failed cost", entries[0].Cost.TotalUSD, test.wantCost)
		})
	}
}
