package sqlite

import (
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"cpa-key-billing/internal/billing"
)

func TestV20UsageMigrationPreservesHistoryAndRollsBack(t *testing.T) {
	for _, conflict := range []bool{false, true} {
		t.Run(map[bool]string{false: "success", true: "rollback"}[conflict], func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "history.db")
			db := openDatabase(t, path)
			ddl := `ALTER TABLE request_events DROP COLUMN stream;`
			if !conflict {
				ddl += `ALTER TABLE request_events DROP COLUMN token_usage_json;`
			}
			if _, err := db.db.Exec(ddl + `PRAGMA user_version=19;
				INSERT INTO request_events(id,at,scope,failed,total_usd,billing_multiplier,service_tier_multiplier,pricing_json)
				VALUES(1,1,'old',0,.123,.2,2.5,'{"rule_version":"codex-oauth-family-v2"}'),(2,2,'old',1,0,1,1,'{}'),(3,3,'deleted',0,0,1,1,'{}');
				INSERT INTO request_errors(request_event_id,status_code,body) VALUES(2,502,'dummy failure');
				CREATE TRIGGER no_reprice BEFORE UPDATE ON request_events BEGIN SELECT RAISE(ABORT,'history rewritten'); END;
				CREATE TRIGGER no_delete BEFORE DELETE ON request_events BEGIN SELECT RAISE(ABORT,'history deleted'); END;`); err != nil {
				t.Fatal(err)
			}
			db.Close()
			reopened, err := Open(path)
			if conflict {
				if err == nil {
					reopened.Close()
					t.Fatal("accepted an incompatible partial schema")
				}
				raw := maintenanceRawDB(t, path)
				var version, columns, entries int
				if err := raw.QueryRow("PRAGMA user_version").Scan(&version); err != nil || version != 19 {
					t.Fatal(version, err)
				}
				if err := raw.QueryRow("SELECT count(*) FROM pragma_table_info('request_events') WHERE name='stream'").Scan(&columns); err != nil || columns != 0 {
					t.Fatal("migration did not roll back", columns, err)
				}
				if err := raw.QueryRow("SELECT count(*) FROM request_events").Scan(&entries); err != nil || entries != 3 {
					t.Fatal(entries, err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			view, err := reopened.RequestEvents(billing.RequestEventQuery{}, time.Time{})
			if err != nil || len(view.Entries) != 3 {
				t.Fatal(view, err)
			}
			for _, row := range view.Entries {
				if row.Stream != nil || row.TokenUsage != nil || row.RequestType != "unknown" {
					t.Fatal("invented historical usage", row)
				}
			}
			if view.Entries[2].Cost.TotalUSD != .123 || view.Entries[2].Cost.ServiceTierMultiplier != 2.5 || view.Entries[2].Cost.Pricing.RuleVersion != "codex-oauth-family-v2" || view.Entries[1].ErrorBody != "dummy failure" {
				t.Fatal("historical billing changed", view)
			}
			reopened.Close()
			reopened, err = Open(path)
			if err != nil {
				t.Fatal("reopen", err)
			}
			reopened.Close()
		})
	}
}

func TestRequestTypeFiltersPageAndTokenSnapshots(t *testing.T) {
	db := openTestDB(t)
	state := billing.NewState()
	yes, no := true, false
	usage := billing.TokenBreakdown{Quality: billing.TokenAccountingUnclassified, TotalTokens: 120, UnclassifiedTokens: 100,
		Output: billing.TokenOutputBreakdown{TotalTokens: 20, NonReasoningTokens: 20}}
	var entries []billing.RequestEvent
	for i, tt := range []struct {
		scope, model, executor string
		stream                 *bool
	}{
		{"a", "model-a", "CodexExecutor", &yes}, {"a", "model-a", "CodexExecutor", &yes},
		{"a", "model-b", "CodexExecutor", &no}, {"a", "model-a", "CodexExecutor", nil},
		{"a", "model-a", " CodexWebsocketsExecutor ", nil}, {"b", "private-model", "CodexExecutor", &yes},
	} {
		entry := requestEvent(tt.scope, eventStart.Add(time.Duration(i)*time.Minute))
		entry.Stream, entry.ExecutorType, entry.BillingModel, entry.TokenUsage = tt.stream, tt.executor, tt.model, &usage
		entries = append(entries, entry)
	}
	mustSave(t, db, state, billing.Changes{NormalRequestEvents: entries})
	for kind, expected := range map[string]int{"stream": 2, "sync": 1, "ws": 1, "unknown": 1} {
		query := billing.RequestEventQuery{Scope: "a", RequestType: kind, Limit: 1, IncludeFilters: true}
		view := mustQueryRequestEvents(t, db, query)
		if view.Total != expected || len(view.Entries) != 1 || len(view.Filters.Models) != 2 {
			t.Fatal(kind, view)
		}
		row := view.Entries[0]
		if row.RequestType != kind || row.ExecutionType() != kind || !reflect.DeepEqual(row.TokenUsage, &usage) {
			t.Fatal("type query and stored metadata disagree", kind, row)
		}
		query.Offset, query.SnapshotID = 1, &view.SnapshotID
		if second := mustQueryRequestEvents(t, db, query); second.Total != expected || len(second.Entries) != expected-1 {
			t.Fatal(second)
		}
	}
	if view := mustQueryRequestEvents(t, db, billing.RequestEventQuery{Scope: "a", Model: "model-b", RequestType: "stream"}); view.Total != 0 {
		t.Fatal("combined filter was not applied", view)
	}
}
