package billing

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"testing"
	"time"
)

func TestAPIServiceTierPricing(t *testing.T) {
	rates := PriceRates{InputPer1M: 2, OutputPer1M: 10, CacheReadPer1M: float64Ptr(0.2), CacheWritePer1M: float64Ptr(2.5)}
	usage := completeBreakdown(1000, 400, 0, 200, 50)
	for _, tt := range []struct {
		name, request, response, model, tier, source, method, tierFallback, priceFallback string
		want                                                                              float64
	}{
		{"standard", "auto", "default", "gpt-6-sol", "default", "response", "base", "", "", .000816},
		{"priority", "priority", "priority", "gpt-6-sol", "priority", "response", "model_ratio", "", "", .001632},
		{"fast alias", "fast", " FAST ", "route/gpt-6-sol(high)", "priority", "response", "model_ratio", "", "", .001632},
		{"flex", "flex", "flex", "gpt-6-sol", "flex", "response", "model_ratio", "", "", .000408},
		{"priority downgrade", "priority", "default", "gpt-6-sol", "default", "response", "base", "", "", .000816},
		{"flex downgrade", "flex", "default", "gpt-6-sol", "default", "response", "base", "", "", .000816},
		{"auto priority", "auto", "priority", "gpt-6-sol", "priority", "response", "model_ratio", "", "", .001632},
		{"missing response", "priority", "", "gpt-6-sol", "priority", "request", "model_ratio", "missing_response_tier", "", .001632},
		{"missing both", "", "", "gpt-6-sol", "default", "default", "base", "missing_response_tier", "", .000816},
		{"auto missing", "auto", "", "gpt-6-sol", "default", "default", "base", "missing_response_tier", "", .000816},
		{"unknown response", "priority", "future-tier", "gpt-6-sol", "default", "response", "base", "unknown_response_tier", "", .000816},
		{"unknown request", "future-tier", "", "gpt-6-sol", "default", "request", "base", "unknown_request_tier", "", .000816},
		{"unknown model", "priority", "priority", "unknown", "priority", "response", "base_fallback", "", "missing_priority_price", .000816},
		{"missing model and response", "priority", "", "unknown", "priority", "request", "base_fallback", "missing_response_tier", "missing_priority_price", .000816},
		{"GPT 5.5 API card", "priority", "priority", "gpt-5.5", "priority", "response", "model_ratio", "", "", .00204},
		{"GPT 4o ratio", "priority", "priority", "gpt-4o", "priority", "response", "model_ratio", "", "", .0013872},
	} {
		t.Run(tt.name, func(t *testing.T) {
			price, meta := resolveAPIServicePrice(rates.resolve(PriceSourceCustom), tt.model, UsageEvent{ServiceTier: tt.request, ResponseServiceTier: tt.response, Breakdown: usage})
			cost := ComputeCost(price, usage)
			cost.applyMultipliers(.2, 1)
			assertClose(t, "cost", cost.TotalUSD, tt.want)
			if meta.ServiceTier != tt.tier || meta.TierSource != tt.source || meta.Method != tt.method || meta.TierFallback != tt.tierFallback || meta.PriceFallback != tt.priceFallback {
				t.Fatalf("metadata = %+v", meta)
			}
			if cost.Multiplier != .2 || cost.ServiceTierMultiplier != 1 || cost.BilledOutputTokens != 200 {
				t.Fatalf("double adjustment: %+v", cost)
			}
		})
	}
}

func TestExplicitTierPricesAndIndependentRatios(t *testing.T) {
	rates := PriceRates{InputPer1M: 2, OutputPer1M: 10, CacheReadPer1M: float64Ptr(.2), CacheWritePer1M: float64Ptr(2.5), ServiceTiers: map[string]TierPriceRates{
		"priority": {InputPer1M: float64Ptr(3), OutputPer1M: float64Ptr(40), CacheReadPer1M: float64Ptr(0), CacheWritePer1M: float64Ptr(7)},
		"flex":     {InputPer1M: float64Ptr(.9), OutputPer1M: float64Ptr(3), CacheReadPer1M: float64Ptr(.1)},
	}}
	usage := completeBreakdown(1000, 400, 100, 200, 50)
	for _, tier := range []string{"priority", "flex"} {
		price, meta := resolveAPIServicePrice(rates.resolve(PriceSourceCustom), "gpt-6-sol", UsageEvent{ResponseServiceTier: tier, Breakdown: usage})
		cost := ComputeCost(price, usage)
		cost.applyMultipliers(.2, 1)
		want := .00234 // (1000*3 + 400*0 + 100*7 + 200*40)/1M * .2
		if tier == "flex" {
			want = .000326
		} // cache write inherits the selected input price
		assertClose(t, "explicit tier", cost.TotalUSD, want)
		if meta.Method != "explicit_tier" {
			t.Fatal(meta)
		}
	}
	scaled := scaleServicePrice(rates.resolve(PriceSourceCustom), tokenRateRatio{3, 4, 5, 6})
	assertClose(t, "independent input", scaled.InputPer1M, 6)
	assertClose(t, "independent output", scaled.OutputPer1M, 40)
	assertClose(t, "independent cache read", scaled.CacheReadPer1M, 1)
	assertClose(t, "independent cache write", scaled.CacheWritePer1M, 15)
	if rates.InputPer1M != 2 {
		t.Fatal("mutated base")
	}
}

func TestCodexFlexRequiresResponseConfirmation(t *testing.T) {
	rates := PriceRates{InputPer1M: 2, OutputPer1M: 10, ServiceTiers: map[string]TierPriceRates{
		"flex": {InputPer1M: float64Ptr(.1), OutputPer1M: float64Ptr(.2)},
	}}
	usage := completeBreakdown(1000, 0, 0, 200, 0)
	for _, upstream := range []struct{ provider, executor string }{
		{" CoDeX ", ""}, {"codex", "CodexExecutor"}, {"codex", "CodexWebsocketsExecutor"},
		{"custom", "CodexExecutor"}, {"custom", "CodexWebsocketsExecutor"},
	} {
		for _, response := range []string{"", " ", "auto", "future", "default", "standard", " FLEX "} {
			t.Run(upstream.provider+"/"+upstream.executor+"/"+response, func(t *testing.T) {
				event := UsageEvent{Provider: upstream.provider, ExecutorType: upstream.executor, AuthType: "apikey", ServiceTier: " FLEX ", ResponseServiceTier: response, Breakdown: usage}
				price, meta := resolveAPIServicePrice(rates.resolve(PriceSourceCustom), "gpt-6-sol", event)
				want, tier, method := .004, "default", "base"
				if response == " FLEX " {
					want, tier, method = .00014, "flex", "explicit_tier"
				}
				assertClose(t, "Flex price requires confirmation", ComputeCost(price, usage).TotalUSD, want)
				if meta.ServiceTier != tier || meta.Method != method {
					t.Fatal(meta)
				}
				if (response == "" || response == " ") && (meta.TierSource != "default" || meta.TierFallback != "unconfirmed_flex_tier") {
					t.Fatal("missing confirmation must explain the standard estimate", meta)
				}
			})
		}
	}
}

func TestServiceTierLongContextBoundaries(t *testing.T) {
	rates := PriceRates{InputPer1M: 2, OutputPer1M: 10, LongContext: &LongContextPrice{ThresholdInputTokens: 1000, InputPer1M: 4, OutputPer1M: 15}}
	for _, tokens := range []int64{999, 1000, 1001} {
		usage := completeBreakdown(tokens, 0, 0, 200, 0)
		for _, tier := range []string{"priority", "flex"} {
			price, _ := resolveAPIServicePrice(rates.resolve(PriceSourceCustom), "gpt-6-sol", UsageEvent{ResponseServiceTier: tier, Breakdown: usage})
			factor := 2.0
			if tier == "flex" {
				factor = .5
			}
			in, out := 2.0, 10.0
			if tokens > 1000 {
				in, out = 4, 15
			}
			cost := ComputeCost(price, usage)
			assertClose(t, "context cost", cost.TotalUSD, (float64(tokens)*in+200*out)*factor/1e6)
			if cost.LongContext != (tokens > 1000) {
				t.Fatal(cost)
			}
		}
	}
	if rates.LongContext.InputPer1M != 4 {
		t.Fatal("mutated shared long context")
	}
	rates.ServiceTiers = map[string]TierPriceRates{"priority": {InputPer1M: float64Ptr(3), OutputPer1M: float64Ptr(7), LongContext: &LongContextPrice{ThresholdInputTokens: 500, InputPer1M: 9, OutputPer1M: 12}}}
	usage := completeBreakdown(501, 0, 0, 200, 0)
	price, _ := resolveAPIServicePrice(rates.resolve(PriceSourceCustom), "gpt-6-sol", UsageEvent{ResponseServiceTier: "priority", Breakdown: usage})
	assertClose(t, "explicit context", ComputeCost(price, usage).TotalUSD, (501*9+200*12)/1e6)
	// A short-context-only official card must not be extrapolated.
	_, meta := resolveAPIServicePrice(rates.resolve(PriceSourceCustom), "gpt-5.5", UsageEvent{ResponseServiceTier: "priority", Breakdown: completeBreakdown(272001, 0, 0, 1, 0)})
	if meta.Method != "explicit_tier" {
		t.Fatal(meta)
	}
	rates.ServiceTiers = nil
	_, meta = resolveAPIServicePrice(rates.resolve(PriceSourceCustom), "gpt-5.5", UsageEvent{ResponseServiceTier: "priority", Breakdown: completeBreakdown(272001, 0, 0, 1, 0)})
	if meta.PriceFallback != "missing_priority_price" {
		t.Fatal(meta)
	}
}

func TestAPIServiceTierAdmissionAndHistory(t *testing.T) {
	now := time.Date(2026, 9, 25, 0, 0, 0, 0, time.UTC)
	store := newAccountStore(t, now)
	cfg := store.Config()
	cfg.BillingMultiplier = .2
	if err := store.Configure(cfg); err != nil {
		t.Fatal(err)
	}
	_, err := store.UpsertPrice(CustomPrice{ModelID: "gpt-6-sol", PriceRates: PriceRates{InputPer1M: 2, OutputPer1M: 10, CacheReadPer1M: float64Ptr(.2), CacheWritePer1M: float64Ptr(2.5)}})
	if err != nil {
		t.Fatal(err)
	}
	store.ReplaceAll(func(state *State) {
		state.Plans = []Plan{{ID: "p", Windows: []QuotaWindow{{ID: "day", AmountUSD: 10, PeriodSeconds: 86400}}}}
		state.Keys["scope-a"] = &KeyState{PlanID: "p"}
	})
	event := admittedEvent(store, "scope-a", now)
	event.Provider, event.AuthType, event.ServiceTier, event.ResponseServiceTier = "openai", "apikey", "auto", "priority"
	event.UpstreamModel, event.RouteModel = "gpt-6-sol", "gpt-6-sol"
	event.Breakdown = completeBreakdown(1000, 400, 0, 200, 50)
	store.RecordUsage(event)
	before := mustRequestEvents(t, store, RequestEventQuery{}).Entries[0]
	assertClose(t, "recorded cost", before.Cost.TotalUSD, .001632)
	store.Read(func(state *State) {
		cycle := state.Keys["scope-a"].Cycles["day"]
		assertClose(t, "quota spent", cycle.SpentUSD, .001632)
		if cycle.UsedTokens != 1600 || cycle.UsedRequests != 1 {
			t.Fatal(cycle)
		}
	})
	cfg.BillingMultiplier = .3
	if err := store.Configure(cfg); err != nil {
		t.Fatal(err)
	}
	event.At = event.At.Add(time.Second)
	event.RequestedAt = event.At
	store.RecordUsage(event)
	entries := mustRequestEvents(t, store, RequestEventQuery{}).Entries
	if entries[1].Cost != before.Cost {
		t.Fatal("repriced history")
	}
	assertClose(t, "new policy", entries[0].Cost.TotalUSD, .002448)
}

func TestServiceTierEligibilityAndValidation(t *testing.T) {
	for _, tt := range []struct {
		event UsageEvent
		want  bool
	}{
		{UsageEvent{Provider: "openai", AuthType: "apikey"}, true},
		{UsageEvent{Provider: "codex", AuthType: " APIKEY "}, true},
		{UsageEvent{Provider: "custom", AuthType: "apikey", ExecutorType: "OpenAICompatExecutor"}, true},
		{UsageEvent{Provider: "openai-compatible-demo", AuthType: "apikey"}, true},
		{UsageEvent{Provider: "codex", AuthType: "oauth"}, false},
		{UsageEvent{Provider: "openai"}, false},
		{UsageEvent{Provider: "claude", AuthType: "apikey", ExecutorType: "ClaudeExecutor"}, false},
	} {
		if apiTierEligible(tt.event) != tt.want {
			t.Fatal(tt)
		}
	}
	for _, raw := range []string{
		`{"priority":{"input_per_1m":-1,"output_per_1m":1}}`,
		`{"priority":{"output_per_1m":1}}`,
		`{"priority":null}`, `{"future":{"input_per_1m":1,"output_per_1m":1}}`,
		`{"priority":{"input_per_1m":1,"output_per_1m":1,"long_context":{"threshold_input_tokens":0}}}`,
	} {
		var tiers map[string]TierPriceRates
		if err := json.Unmarshal([]byte(raw), &tiers); err != nil {
			continue
		}
		if (PriceRates{ServiceTiers: tiers}).validate("test") == nil {
			t.Fatal("accepted", raw)
		}
	}
	for _, v := range []float64{math.NaN(), math.Inf(1)} {
		if (PriceRates{ServiceTiers: map[string]TierPriceRates{"priority": {InputPer1M: &v, OutputPer1M: float64Ptr(0)}}}).validate("test") == nil {
			t.Fatal("nonfinite")
		}
	}
}

func TestServiceTierPricePublicationAndIsolation(t *testing.T) {
	store, repo := newStoreWithRepository(t)
	card := CustomPrice{ModelID: "gpt-6-sol", PriceRates: PriceRates{InputPer1M: 2, OutputPer1M: 10, ServiceTiers: map[string]TierPriceRates{
		"priority": {InputPer1M: float64Ptr(3), OutputPer1M: float64Ptr(7), LongContext: &LongContextPrice{ThresholdInputTokens: 1000, InputPer1M: 8, OutputPer1M: 12}},
	}}}
	saved, err := store.UpsertPrice(card)
	if err != nil {
		t.Fatal(err)
	}
	*card.ServiceTiers["priority"].InputPer1M = 100
	*saved.ServiceTiers["priority"].OutputPer1M = 100
	saved.ServiceTiers["priority"].LongContext.InputPer1M = 100
	rows, err := store.ModelPriceRows([]string{"gpt-6-sol"}, false)
	if err != nil {
		t.Fatal(err)
	}
	*rows[0].ServiceTiers["priority"].InputPer1M = 200
	rows[0].ServiceTierPrices["priority"].Price.LongContext.OutputPer1M = 200
	repo.fail = errors.New("dummy disk failure")
	if _, err := store.UpsertPrice(card); !errors.Is(err, repo.fail) {
		t.Fatal("failed storage update was published", err)
	}
	repo.fail = nil
	price, _, err := store.ResolveModelPrice("gpt-6-sol", "gpt-6-sol", false)
	if err != nil {
		t.Fatal(err)
	}
	selected, meta := resolveAPIServicePrice(price, "gpt-6-sol", UsageEvent{ResponseServiceTier: "priority"})
	if selected.InputPer1M != 3 || selected.OutputPer1M != 7 || selected.LongContext.InputPer1M != 8 || selected.LongContext.OutputPer1M != 12 || meta.Method != "explicit_tier" {
		t.Fatalf("published card mutated: %+v", selected)
	}
}

func TestAPIServiceTierFailedAndZeroUsageQuota(t *testing.T) {
	for _, failed := range []bool{false, true} {
		for _, zero := range []bool{false, true} {
			t.Run(fmt.Sprintf("failed=%t/zero=%t", failed, zero), func(t *testing.T) {
				now := time.Date(2026, 9, 25, 0, 0, 0, 0, time.UTC)
				store := newAccountStore(t, now)
				if _, err := store.UpsertPrice(CustomPrice{ModelID: "gpt-6-sol", PriceRates: PriceRates{InputPer1M: 2, OutputPer1M: 10}}); err != nil {
					t.Fatal(err)
				}
				store.ReplaceAll(func(state *State) {
					state.Plans = []Plan{{ID: "p", Windows: []QuotaWindow{{ID: "day", AmountUSD: 10, PeriodSeconds: 86400}}}}
					state.Keys["scope-a"] = &KeyState{PlanID: "p"}
				})
				event := admittedEvent(store, "scope-a", now)
				event.Provider, event.AuthType, event.ServiceTier = "openai", "apikey", "priority"
				event.UpstreamModel, event.RouteModel = "gpt-6-sol", "gpt-6-sol"
				event.Breakdown = completeBreakdown(1000, 0, 0, 200, 50)
				wantAmount, wantTokens := .008, int64(1200)
				if zero {
					event.Breakdown = completeBreakdown(0, 0, 0, 0, 0)
					wantAmount, wantTokens = 0, 0
				}
				if failed {
					store.RecordUsageError(event, RequestError{StatusCode: 502, Body: "dummy upstream error"})
				} else {
					store.RecordUsage(event)
				}
				entries := mustRequestEvents(t, store, RequestEventQuery{}).Entries
				if len(entries) != 1 || entries[0].Failed != failed || entries[0].Cost.Pricing.TierFallback != "missing_response_tier" {
					t.Fatal(entries)
				}
				assertClose(t, "failed/zero bill", entries[0].Cost.TotalUSD, wantAmount)
				store.Read(func(state *State) {
					cycle := state.Keys["scope-a"].Cycles["day"]
					assertClose(t, "failed/zero quota", cycle.SpentUSD, wantAmount)
					wantRequests := int64(1)
					if failed {
						wantRequests = 0
					}
					if cycle.UsedTokens != wantTokens || cycle.UsedRequests != wantRequests {
						t.Fatal(cycle)
					}
				})
			})
		}
	}
}

// Published short-context unit prices are independent oracles for the ratio
// table. Columns are input, output, cache read, cache write (USD per 1M).
// Source: https://developers.openai.com/api/docs/pricing, checked 2026-09-25.
func TestPriorityPricesAgainstPublishedCards(t *testing.T) {
	for _, test := range []struct {
		model              string
		standard, priority [4]float64
	}{
		{"gpt-6-astra", [4]float64{10, 50, 1, 12.5}, [4]float64{20, 100, 2, 25}},
		{"gpt-6-sol", [4]float64{2, 10, .2, 2.5}, [4]float64{4, 20, .4, 5}},
		{"gpt-6-luna", [4]float64{.1, .5, .01, .125}, [4]float64{.2, 1, .02, .25}},
		{"gpt-5.6-sol", [4]float64{4, 20, .4, 5}, [4]float64{8, 40, .8, 10}},
		{"gpt-5.6-terra", [4]float64{2, 12, .2, 2.5}, [4]float64{4, 24, .4, 5}},
		{"gpt-5.6-luna", [4]float64{.2, 1.2, .02, .25}, [4]float64{.4, 2.4, .04, .5}},
		{"gpt-5.5", [4]float64{5, 30, .5, 0}, [4]float64{12.5, 75, 1.25, 0}},
		{"gpt-4o", [4]float64{2.5, 10, 1.25, 0}, [4]float64{4.25, 17, 2.125, 0}},
		{"gpt-4.1", [4]float64{2, 8, .5, 0}, [4]float64{3.5, 14, .875, 0}},
	} {
		t.Run(test.model, func(t *testing.T) {
			rates := PriceRates{InputPer1M: test.standard[0], OutputPer1M: test.standard[1], CacheReadPer1M: &test.standard[2], CacheWritePer1M: &test.standard[3]}
			cacheWrite := int64(100)
			if test.standard[3] == 0 {
				cacheWrite = 0
			}
			usage := completeBreakdown(1000, 400, cacheWrite, 200, 50)
			price, meta := resolveAPIServicePrice(rates.resolve(PriceSourceCustom), test.model, UsageEvent{ResponseServiceTier: "priority", Breakdown: usage})
			want := (1000*test.priority[0] + 200*test.priority[1] + 400*test.priority[2] + float64(cacheWrite)*test.priority[3]) / 1e6
			assertClose(t, "published priority total", ComputeCost(price, usage).TotalUSD, want)
			if meta.RuleVersion != "openai-2026-09-25" {
				t.Fatal(meta)
			}
		})
	}
}
