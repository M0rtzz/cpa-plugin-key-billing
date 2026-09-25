package plugin

import (
	"testing"
	"time"

	"cpa-key-billing/internal/billing"
	"cpa-key-billing/internal/sqlite"
)

// Exercise the usage.handle boundary. Host E2E checks v7.3.17 response tiers
// and the request-tier fallback for older SDKs that omit ResponseServiceTier.
func TestUsageHandleAPIServiceTierMatrix(t *testing.T) {
	for _, upstream := range []struct{ provider, executor string }{
		{"openai", "OpenAICompatExecutor"}, {"codex", "CodexExecutor"}, {"custom", "OpenAICompatExecutor"},
	} {
		t.Run(upstream.provider, func(t *testing.T) {
			app, statePath := newAppWithPriceAndState(t, true)
			cfg := app.store.Config()
			cfg.BillingMultiplier = .2
			if err := app.store.Configure(cfg); err != nil {
				t.Fatal(err)
			}
			read, write := .2, 2.5
			if _, err := app.store.UpsertPrice(billing.CustomPrice{ModelID: "gpt-6-sol", PriceRates: billing.PriceRates{InputPer1M: 2, OutputPer1M: 10, CacheReadPer1M: &read, CacheWritePer1M: &write}}); err != nil {
				t.Fatal(err)
			}
			var snapshots []billing.Cost
			for i, test := range []struct {
				request, response, tier, source, fallback string
				amount                                    float64
			}{
				{"auto", "default", "default", "response", "", .000816},
				{"priority", "priority", "priority", "response", "", .001632},
				{"fast", "fast", "priority", "response", "", .001632},
				{"flex", "flex", "flex", "response", "", .000408},
				{"priority", "default", "default", "response", "", .000816},
				{"flex", "default", "default", "response", "", .000816},
				{"flex", "", "flex", "request", "missing_response_tier", .000408},
				{"auto", "priority", "priority", "response", "", .001632},
				{"priority", "", "priority", "request", "missing_response_tier", .001632},
				{"auto", "", "default", "default", "missing_response_tier", .000816},
				{"priority", "future", "default", "response", "unknown_response_tier", .000816},
				{"future", "", "default", "request", "unknown_request_tier", .000816},
			} {
				if upstream.provider == "codex" && test.request == "flex" && test.response == "" {
					test.tier, test.source, test.fallback, test.amount = "default", "default", "unconfirmed_flex_tier", .000816
				}
				publishUsageRecord(t, app, UsageRecord{
					Provider: upstream.provider, ExecutorType: upstream.executor, AuthType: "apikey",
					APIKey: testAPIKey, Model: "gpt-6-sol", Alias: "gpt-6-sol", Generate: true,
					ServiceTier: test.request, ResponseServiceTier: test.response, RequestedAt: app.store.Now().Add(time.Duration(i) * time.Second),
					Detail: UsageDetail{InputTokens: 1400, CacheReadTokens: 400, OutputTokens: 200, ReasoningTokens: 50, TotalTokens: 1600},
				})
				entry := requestEventEntries(t, app)[0]
				assertCostClose(t, entry.Cost.TotalUSD, test.amount)
				meta := entry.Cost.Pricing
				if meta.ServiceTier != test.tier || meta.TierSource != test.source || meta.TierFallback != test.fallback || entry.Cost.ServiceTierMultiplier != 1 {
					t.Fatalf("case %d: %+v", i, entry)
				}
				snapshots = append(snapshots, entry.Cost)
			}
			in, out := 9.0, 20.0
			if _, err := app.store.UpsertPrice(billing.CustomPrice{ModelID: "gpt-6-sol", PriceRates: billing.PriceRates{InputPer1M: 2, OutputPer1M: 10, ServiceTiers: map[string]billing.TierPriceRates{"priority": {InputPer1M: &in, OutputPer1M: &out}}}}); err != nil {
				t.Fatal(err)
			}
			app.Shutdown()
			db, err := sqlite.Open(statePath)
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			view, err := db.RequestEvents(billing.RequestEventQuery{Scope: flowScope()}, time.Time{})
			if err != nil || len(view.Entries) != len(snapshots) {
				t.Fatal(view, err)
			}
			for i, entry := range view.Entries {
				if entry.Cost != snapshots[len(snapshots)-1-i] {
					t.Fatal("tier change repriced history", entry)
				}
			}
		})
	}
}

func TestUsageHandleOAuthTiersPersistWithoutRepricing(t *testing.T) {
	app, statePath := newAppWithPriceAndState(t, true)
	cfg := app.store.Config()
	cfg.BillingMultiplier = .2
	if err := app.store.Configure(cfg); err != nil {
		t.Fatal(err)
	}
	var snapshots []billing.Cost
	for i, tt := range []struct {
		model, request, response, source, fallback string
		factor                                     float64
	}{
		{"gpt-5.6-sol", "priority", "priority", "request_policy", "", 2},
		{"gpt-6-astra", "auto", "fast", "response", "", 2},
		{"gpt-6-luna", "priority", "default", "request_policy", "", 2},
		{"gpt-6-sol", "fast", "auto", "request_policy", "", 2},
		{"gpt-5.6-sol", "fast", "default", "request_policy", "", 2},
		{"gpt-5.6-terra", "priority", "auto", "request_policy", "", 2},
		{"gpt-6-sol", "priority", "flex", "response", "", .5},
		{"gpt-5.6-sol", "fast", "standard", "response", "", 1},
		{"gpt-6-sol", "priority", "standard", "response", "", 1},
		{"gpt-6-sol", "flex", "flex", "response", "", .5},
		{"gpt-6-sol", "flex", "auto", "response", "unknown_response_tier", 1},
		{"gpt-5.6-sol", "flex", "future", "response", "unknown_response_tier", 1},
		{"gpt-6-sol", "flex", "priority", "response", "", 2},
		{"gpt-5.6-sol", "flex", "", "default", "unconfirmed_flex_tier", 1},
		{"gpt-6-sol", "flex", "default", "response", "", 1},
		{"gpt-5.6-sol", "flex", "standard", "response", "", 1},
		{"gpt-5.5", "flex", "default", "response", "", 1},
		{"gpt-6-sol", "auto", "default", "response", "", 1},
		{"gpt-5.5", "priority", "priority", "request_policy", "", 2.5},
		{"gpt-5.5", "priority", "default", "request_policy", "", 2.5},
		{"gpt-5.5", "fast", "auto", "request_policy", "", 2.5},
		{"gpt-5.5", "fast", "", "request_policy", "", 2.5},
		{"gpt-5.5", "priority", "standard", "response", "", 1},
		{"gpt-5.5", "priority", "flex", "response", "", .5},
		{"gpt-5.5", "auto", "default", "response", "", 1},
		{"gpt-6-sol", "priority", "", "request_policy", "", 2},
		{"gpt-6-sol", "auto", "", "default", "missing_response_tier", 1},
		{"gpt-6-sol", "priority", "future", "request_policy", "", 2},
		{"gpt-5.6-sol", "fast", "future", "request_policy", "", 2},
		{"gpt-5.5", "priority", "future", "request_policy", "", 2.5},
	} {
		publishUsageRecord(t, app, UsageRecord{
			Provider: "codex", ExecutorType: "CodexExecutor", AuthType: "oauth", APIKey: testAPIKey,
			Model: flowModel, Alias: flowModel, ResponseModel: tt.model, Generate: true,
			ServiceTier: tt.request, ResponseServiceTier: tt.response,
			RequestedAt: app.store.Now().Add(time.Duration(i) * time.Second),
			Detail:      UsageDetail{InputTokens: 1000, CacheReadTokens: 400, CacheCreationTokens: 100, OutputTokens: 500, ReasoningTokens: 200, TotalTokens: 1500},
		})
		entry := requestEventEntries(t, app)[0]
		assertCostClose(t, entry.Cost.TotalUSD, .001665*.2*tt.factor)
		if entry.Cost.ServiceTierMultiplier != tt.factor || entry.Cost.Pricing.Method != "oauth_multiplier" ||
			entry.Cost.Pricing.RuleVersion != "codex-oauth-family-v8" || entry.Cost.Pricing.TierSource != tt.source || entry.Cost.Pricing.TierFallback != tt.fallback {
			t.Fatal(entry.Cost)
		}
		if entry.ServiceTier != tt.request || entry.ResponseServiceTier != tt.response {
			t.Fatal("request policy changed the recorded host tiers", entry)
		}
		snapshots = append(snapshots, entry.Cost)
	}
	cfg.BillingMultiplier, cfg.CodexFastModeBilling = .3, false
	if err := app.store.Configure(cfg); err != nil {
		t.Fatal(err)
	}
	app.Shutdown()
	db, err := sqlite.Open(statePath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	view, err := db.RequestEvents(billing.RequestEventQuery{}, time.Time{})
	if err != nil || len(view.Entries) != len(snapshots) {
		t.Fatal(view, err)
	}
	for i, entry := range view.Entries {
		if entry.Cost != snapshots[len(snapshots)-1-i] {
			t.Fatal("OAuth policy change or restart repriced historical bill", entry)
		}
	}
}
