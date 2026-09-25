package plugin

import (
	"encoding/json"
	"net/http"
	"net/url"
	"testing"
	"time"

	"cpa-key-billing/internal/billing"
	"cpa-key-billing/internal/sqlite"
)

func TestUsageHandlePersistsStreamAndUnpricedTokenDetails(t *testing.T) {
	app, path := newAppWithPriceAndState(t, true)
	yes, no := true, false
	for i, stream := range []*bool{nil, &no, &yes} {
		publishUsageRecord(t, app, UsageRecord{
			Provider: "codex", ExecutorType: "CodexExecutor", AuthType: "oauth", APIKey: testAPIKey,
			Model: "unpriced-model", Alias: "unpriced-model", Stream: stream, Failed: i == 1,
			Failure: UsageFailure{StatusCode: 502, Body: "dummy failure"}, RequestedAt: app.store.Now().Add(time.Duration(i) * time.Second),
			Detail: UsageDetail{OutputTokens: 20, TotalTokens: 120},
		})
	}
	app.Shutdown()
	db, err := sqlite.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	view, err := db.RequestEvents(billing.RequestEventQuery{}, time.Time{})
	if err != nil || len(view.Entries) != 3 {
		t.Fatal(view, err)
	}
	for i, kind := range []string{"stream", "sync", "unknown"} {
		row := view.Entries[i]
		if row.RequestType != kind || row.Cost.TotalUSD != 0 || row.TokenUsage == nil || row.TokenUsage.TotalTokens != 120 || row.TokenUsage.UnclassifiedTokens != 100 || row.TokenUsage.Output.TotalTokens != 20 {
			t.Fatal("usage lost independently of pricing", row)
		}
	}
	if !view.Entries[1].Failed || view.Entries[1].ErrorBody != "dummy failure" {
		t.Fatal(view.Entries[1])
	}
}

func TestSelfServiceRequestTypeFiltersAndScope(t *testing.T) {
	app, _ := selfServiceAuthority(t)
	if _, err := app.store.SyncKeys([]string{validationTestKey}, false); err != nil {
		t.Fatal(err)
	}
	yes, no := true, false
	for i, stream := range []*bool{&yes, &no, nil} {
		app.store.RecordUsage(billing.UsageEvent{Scope: billing.CallerScope(validationTestKey), UpstreamModel: "own-model", Stream: stream, RequestedAt: app.store.Now().Add(time.Duration(i) * time.Second)})
	}
	app.store.RecordUsage(billing.UsageEvent{Scope: billing.CallerScope("sk-foreign-dummy"), UpstreamModel: "private-model", Stream: &yes})
	for _, kind := range []string{"stream", "sync", "unknown"} {
		response := callSelfService(t, app, routeEvents, validationTestKey, url.Values{"request_type": {kind}, "api_key": {billing.CallerScope("sk-foreign-dummy")}})
		if response.StatusCode != http.StatusOK {
			t.Fatal(response.StatusCode, string(response.Body))
		}
		var view billing.RequestEventView
		if err := json.Unmarshal(response.Body, &view); err != nil {
			t.Fatal(err)
		}
		if view.Total != 1 || view.Entries[0].RequestType != kind || view.Entries[0].Scope != "" || len(view.Filters.Models) != 1 || view.Filters.Models[0] != "own-model" {
			t.Fatal(view)
		}
	}
	if response := callSelfService(t, app, routeEvents, validationTestKey, url.Values{"request_type": {"invalid"}}); response.StatusCode != http.StatusBadRequest {
		t.Fatal(response)
	}
}
