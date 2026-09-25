package sqlite

import (
	"fmt"
	"math"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"cpa-key-billing/internal/billing"
)

func TestDashboardBillingModelsPreserveMixedHistory(t *testing.T) {
	db := openTestDB(t)
	state := billing.NewState()
	var normal []billing.RequestEvent
	for i, fixture := range []struct{ scope, billed, upstream, requested, reported string }{
		{"a", "model-a", "model-a", "", ""},
		{"a", "model-b", "model-b", "", ""},
		{"b", "model-a", "model-a", "", ""},
		{"a", "model-a", "execution-model", "alias", "execution-model"},
		{"a", "model-b", "execution-model", "alias", "execution-model"},
		{"a", "", "model-a", "", ""},
		{"a", "", "", "", ""},
	} {
		event := requestEvent(fixture.scope, eventStart)
		event.BillingModel, event.UpstreamModel = fixture.billed, fixture.upstream
		event.RequestedModel, event.ReportedModel = fixture.requested, fixture.reported
		event.Cost.TotalUSD = float64(i+1) / 10
		event.Cost.BillingMultiplier = .2
		normal = append(normal, event)
	}
	zero := requestEvent("a", eventStart)
	zero.BillingModel, zero.UpstreamModel = "model-c", "model-c"
	zero.Cost = billing.Cost{BillingMultiplier: .2}
	normal = append(normal, zero)
	failed := zero
	failed.BillingModel, failed.UpstreamModel = "model-b", "model-b"
	mustSave(t, db, state, billing.Changes{NormalRequestEvents: normal, RequestErrorEvents: []billing.RequestErrorEvent{{Event: failed}}})
	before := mustQueryRequestEvents(t, db, billing.RequestEventQuery{})
	view, err := db.Analysis(billing.RequestEventQuery{From: eventStart.Add(-time.Hour), To: eventStart.Add(time.Hour)}, eventStart.Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if view.Summary.Requests != 9 || view.Summary.Failed != 1 || len(view.ModelGroups) != 7 {
		t.Fatalf("Lost historical, failed, or empty rows: %+v", view)
	}
	counts := map[string]int64{}
	var tokens, mapped int64
	var cost, beforeGlobal float64
	for _, group := range view.ModelGroups {
		counts[group.BillingModel] += group.Requests
		tokens += group.TotalTokens
		cost += group.CostUSD
		beforeGlobal += group.BeforeGlobalUSD
		if group.RequestedModel != "" || group.ReportedModel != "" {
			if group.RequestedModel != "alias" || group.ReportedModel != "execution-model" {
				t.Fatalf("Invented model mapping: %+v", group)
			}
			mapped += group.Requests
		}
	}
	if !reflect.DeepEqual(counts, map[string]int64{"model-a": 3, "model-b": 3, "model-c": 1, "": 2}) || mapped != 2 {
		t.Fatal("Billing models merged or inferred from legacy upstream fields", counts, mapped)
	}
	if tokens != 7000 || math.Abs(cost-2.8) > 1e-9 || math.Abs(beforeGlobal-14) > 1e-9 {
		t.Fatal("Aggregate amounts changed", tokens, cost, beforeGlobal)
	}
	if !reflect.DeepEqual(before, mustQueryRequestEvents(t, db, billing.RequestEventQuery{})) {
		t.Fatal("Analysis modified stored bills")
	}
}

func TestDashboardModelMappingCostsTop10AndScope(t *testing.T) {
	db := openTestDB(t)
	state := billing.NewState()
	var events []billing.RequestEvent
	for i := 0; i < 12; i++ {
		key := fmt.Sprintf("scope-%02d", i)
		state.Keys[key] = &billing.KeyState{Label: "Same alias", Preview: fmt.Sprintf("sk-dummy…%04d", i)}
		e := requestEvent(key, eventStart.Add(time.Duration(i)*time.Minute))
		e.RequestedModel = "alias"
		e.ReportedModel = "gpt-6-sol"
		e.Cost.BilledOutputTokens = int64(i + 1)
		e.Cost.UncachedInputTokens = 0
		e.Cost.CacheReadTokens = 0
		e.Cost.CacheWriteTokens = 0
		e.Cost.BillingMultiplier = .2
		e.Cost.TotalUSD = .5
		e.AccountingQuality = billing.TokenAccountingComplete
		events = append(events, e)
	}
	legacy := requestEvent("scope-00", eventStart)
	legacy.Cost.TotalUSD = .2
	legacy.Cost.BillingMultiplier = .5
	events = append(events, legacy)
	mustSave(t, db, state, billing.Changes{AllKeys: true, NormalRequestEvents: events})
	q := billing.RequestEventQuery{From: eventStart.Add(-time.Hour), To: eventStart.Add(2 * time.Hour), Granularity: "hour"}
	view, err := db.Analysis(q, eventStart.Add(-billing.RequestEventRetention))
	if err != nil {
		t.Fatal(err)
	}
	if len(view.KeyTrends) != 10 || view.SnapshotID != 13 {
		t.Fatalf("Top 10/snapshot %+v", view)
	}
	if len(view.ModelGroups) != 13 {
		t.Fatal("mapping groups lost", view.ModelGroups)
	}
	var cost, before float64
	var unknown int
	for _, g := range view.ModelGroups {
		cost += g.CostUSD
		before += g.BeforeGlobalUSD
		if g.RequestedModel == "" {
			unknown++
		}
		if g.RequestedModel != "" && g.ReportedModel != "gpt-6-sol" {
			t.Fatal(g)
		}
	}
	if math.Abs(cost-6.2) > 1e-9 || math.Abs(before-30.4) > 1e-9 || unknown != 1 {
		t.Fatal(cost, before, unknown)
	}
	for _, series := range view.KeyTrends {
		if len(series.Points) != 3 {
			t.Fatal(series)
		}
		if series.Label != "Same alias" {
			t.Fatal(series)
		}
	}
	q.Scope = "scope-00"
	scoped, err := db.Analysis(q, eventStart.Add(-billing.RequestEventRetention))
	if err != nil {
		t.Fatal(err)
	}
	if len(scoped.ModelGroups) != 0 || len(scoped.KeyTrends) != 0 || len(scoped.UsageDistribution.APIKeys) != 0 {
		t.Fatal("global data exposed to scoped request", scoped)
	}
}

func TestDashboardBucketsBeyondSQLiteCompoundLimit(t *testing.T) {
	db := openTestDB(t)
	state := billing.NewState()
	e := requestEvent("a", eventStart.Add(600*time.Hour))
	mustSave(t, db, state, billing.Changes{NormalRequestEvents: []billing.RequestEvent{e}})
	q := billing.RequestEventQuery{From: eventStart, To: eventStart.Add(999 * time.Hour), Granularity: "hour"}
	view, err := db.Analysis(q, eventStart.Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(view.Trends.Requests) != 999 || view.Summary.Requests != 1 || view.Trends.Requests[600].Value != 1 {
		t.Fatal("batch boundaries", view.Summary)
	}
	q.To = eventStart.Add(1001 * time.Hour)
	if _, err := db.Analysis(q, eventStart.Add(-time.Hour)); err == nil {
		t.Fatal("excessive buckets accepted")
	}
}

func TestV21MigrationPreservesHistoryAndRollsBack(t *testing.T) {
	for _, conflict := range []bool{false, true} {
		t.Run(fmt.Sprint(conflict), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "v20.db")
			db := openDatabase(t, path)
			ddl := `ALTER TABLE request_events DROP COLUMN reported_model;`
			if !conflict {
				ddl += `ALTER TABLE request_events DROP COLUMN requested_model;`
			}
			_, err := db.db.Exec(ddl + `PRAGMA user_version=20;
   INSERT INTO request_events(at,scope,failed,total_usd) VALUES(1,'a',0,.123),(2,'a',1,0),(3,'b',0,0);
   CREATE TRIGGER preserve_rows BEFORE UPDATE ON request_events BEGIN SELECT RAISE(ABORT,'rewritten'); END;
   CREATE TRIGGER preserve_deletes BEFORE DELETE ON request_events BEGIN SELECT RAISE(ABORT,'deleted'); END;`)
			if err != nil {
				t.Fatal(err)
			}
			db.Close()
			migrated, err := Open(path)
			if conflict {
				if err == nil {
					migrated.Close()
					t.Fatal("conflict accepted")
				}
				raw := maintenanceRawDB(t, path)
				var v int
				raw.QueryRow("PRAGMA user_version").Scan(&v)
				if v != 20 {
					t.Fatal(v)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			var n, missing int
			var cost float64
			if err = migrated.db.QueryRow("SELECT count(*),sum(requested_model IS NULL AND reported_model IS NULL),sum(total_usd) FROM request_events").Scan(&n, &missing, &cost); err != nil {
				t.Fatal(err)
			}
			if n != 3 || missing != 3 || cost != .123 {
				t.Fatal(n, missing, cost)
			}
			migrated.Close()
			again, err := Open(path)
			if err != nil {
				t.Fatal(err)
			}
			again.Close()
		})
	}
}

func TestDashboardUnknownMultiplierAndModelRoundTrip(t *testing.T) {
	db := openTestDB(t)
	state := billing.NewState()
	e := requestEvent("a", eventStart)
	e.RequestedModel = "exact-alias"
	e.ReportedModel = "actual-model"
	mustSave(t, db, state, billing.Changes{NormalRequestEvents: []billing.RequestEvent{e}})
	// Zero can exist in imported history; do not replace it with today's multiplier.
	if _, err := db.db.Exec("UPDATE request_events SET billing_multiplier=0"); err != nil {
		t.Fatal(err)
	}
	v, err := db.RequestEvents(billing.RequestEventQuery{}, eventStart.Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if v.Entries[0].RequestedModel != "exact-alias" || v.Entries[0].ReportedModel != "actual-model" {
		t.Fatal(v)
	}
	a, err := db.Analysis(billing.RequestEventQuery{From: eventStart.Add(-time.Hour), To: eventStart.Add(time.Hour)}, eventStart.Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if a.ModelGroups[0].Unconvertible != 1 || a.ModelGroups[0].BeforeGlobalUSD != 0 {
		t.Fatal(a.ModelGroups)
	}
}
