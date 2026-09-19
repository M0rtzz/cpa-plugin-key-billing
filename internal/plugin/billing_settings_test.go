package plugin

import (
	"encoding/json"
	"net/http"
	"testing"

	"cpa-key-billing/internal/billing"
)

func TestCurrentBillingSettingsDoNotRewriteRecordedCosts(t *testing.T) {
	app := configuredAccountApp(t)
	quotaTestHost(t, app, nil)
	before := requestEventEntries(t, app)
	for _, factor := range []float64{0.2, 0.3} {
		cfg := app.store.Config()
		cfg.BillingMultiplier = factor
		cfg.CodexFastModeBilling = true
		if err := app.store.Configure(cfg); err != nil {
			t.Fatal(err)
		}
		for _, key := range []string{accountTestKeyA, "sk-valid-but-untracked-0003"} {
			for _, route := range []string{routeProfile, routeSubscription, routeAnalysis, routeQuotaSummary} {
				response := callAccount(t, app, route, key, nil)
				assertBillingSettings(t, response, factor)
			}
		}
		assertBillingSettings(t, callManagement(t, app, http.MethodGet, routeAnalysis, nil, nil), factor)
	}
	after := requestEventEntries(t, app)
	if len(after) != len(before) {
		t.Fatal("configuration changes rewrote history")
	}
	for i := range before {
		if before[i].Cost != after[i].Cost {
			t.Fatalf("event %d changed from %+v to %+v", i, before[i].Cost, after[i].Cost)
		}
	}
	response := callAccount(t, app, routeEvents, accountTestKeyA, nil)
	var events billing.RequestEventView
	if err := json.Unmarshal(response.Body, &events); err != nil {
		t.Fatal(err)
	}
	if len(events.Entries) != 1 || events.Entries[0].Cost.BillingMultiplier != 1 {
		t.Fatalf("old event did not retain its own multiplier: %+v", events)
	}
}

func assertBillingSettings(t *testing.T, response ManagementResponse, factor float64) {
	t.Helper()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("response = %d %s", response.StatusCode, response.Body)
	}
	var settings billingSettingsResponse
	if err := json.Unmarshal(response.Body, &settings); err != nil {
		t.Fatal(err)
	}
	if settings.BillingMultiplier != factor || !settings.CodexFastModeBilling {
		t.Fatalf("settings = %+v, want multiplier %g with fast billing enabled", settings, factor)
	}
}
