package sqlite

import (
	"database/sql"
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"cpa-key-billing/internal/billing"
)

// Create the deployed v17 shape, including the old fast-mode source encoding.
// Rows older than retention deliberately exercise maintenance without pruning.
func billingMaintenanceFixture(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "billing.db")
	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	_, err = db.db.Exec(`
		ALTER TABLE prices DROP COLUMN service_tiers_json;
		ALTER TABLE request_events DROP COLUMN pricing_json;
		ALTER TABLE request_events DROP COLUMN billing_multiplier;
		ALTER TABLE request_events DROP COLUMN service_tier_multiplier;
		DROP TABLE billing_adjustments;
		ALTER TABLE request_events DROP COLUMN stream;
		ALTER TABLE request_events DROP COLUMN token_usage_json;
		PRAGMA user_version=17;
		INSERT INTO request_events(id,at,scope,price_source,total_usd,uncached_input_usd,cache_read_usd,cache_write_usd,output_usd,
			applied_input_per_1m,applied_output_per_1m,applied_cache_read_per_1m,applied_cache_write_per_1m,
			uncached_input_tokens,cache_read_tokens,cache_write_tokens,billed_output_tokens)
		VALUES(1,1,'a','custom',10,1,2,3,4,5,6,7,8,101,102,103,104),
			(2,2,'deleted','reference:x2.5',2.5,0,0,0,0,0,0,0,0,0,0,0,0);
		INSERT INTO request_events(id,at,scope,failed) VALUES(3,3,'deleted',1);
		INSERT INTO request_errors(request_event_id,status_code,body) VALUES(3,502,'preserve failure');
		INSERT INTO api_keys(scope,preview,cycles_json) VALUES('a','masked',
			'{"old":{"spent_usd":25,"used_tokens":9007199254740993,"used_requests":11,"start_at":"2024-01-01T00:00:00Z","custom":{"value":9007199254740995}}}');
		INSERT INTO api_keys(scope,preview,deleted_at,cycles_json) VALUES('deleted','masked',1,
			'{"long":{"spent_usd":250,"used_tokens":17,"used_requests":9},"empty":{"used_tokens":0}}');
		INSERT INTO plans(position,id,windows_json) VALUES(0,'p','[{"amount_usd":100,"token_limit":1000,"request_limit":20}]');
		INSERT INTO prices(position,model_id,input_per_1m,output_per_1m) VALUES(0,'m',15,30);
		INSERT INTO plugin_logs(at,level,message) VALUES(1,'info','Original expense $10.00');
	`)
	if err != nil {
		t.Fatal(err)
	}
	return path
}

func maintenanceRawDB(t *testing.T, path string) *sql.DB {
	t.Helper()
	db, err := maintenanceDatabase(path, "rw")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func TestV18MigrationPreservesLegacyCostsAndFastMode(t *testing.T) {
	path := billingMaintenanceFixture(t)
	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	view, err := db.RequestEvents(billing.RequestEventQuery{}, time.Time{})
	if err != nil || len(view.Entries) != 3 {
		t.Fatal(view, err)
	}
	fast := view.Entries[1]
	if fast.ID != 2 || fast.PriceSource != billing.PriceSourceReference || fast.Cost.TotalUSD != 2.5 ||
		fast.Cost.BillingMultiplier != 1 || fast.Cost.ServiceTierMultiplier != 2.5 || fast.Cost.Multiplier != 2.5 {
		t.Fatalf("legacy fast cost changed: %+v", fast)
	}
	ordinary := view.Entries[2]
	if ordinary.Cost.TotalUSD != 10 || ordinary.Cost.BillingMultiplier != 1 || ordinary.Cost.ServiceTierMultiplier != 1 {
		t.Fatalf("legacy ordinary cost changed: %+v", ordinary)
	}
	var auditCount int
	if err := db.db.QueryRow("SELECT count(*) FROM billing_adjustments").Scan(&auditCount); err != nil || auditCount != 0 {
		t.Fatal("schema migration unexpectedly rebilled history", auditCount, err)
	}
}

func TestRequestMultiplierRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "round-trip.db")
	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	state := billing.NewState()
	ordinary := requestEvent("a", eventStart)
	ordinary.Cost.BillingMultiplier, ordinary.Cost.ServiceTierMultiplier, ordinary.Cost.Multiplier = 0.2, 1, 0.2
	fast := ordinary
	fast.Cost.ServiceTierMultiplier, fast.Cost.Multiplier = 2.5, 0.5
	mustSave(t, db, state, billing.Changes{NormalRequestEvents: []billing.RequestEvent{ordinary, fast}})
	db.Close()
	db, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	view := mustQueryRequestEvents(t, db, billing.RequestEventQuery{})
	if view.Entries[0].Cost.Multiplier != 0.5 || view.Entries[1].Cost.Multiplier != 0.2 ||
		view.Entries[0].Cost.ServiceTierMultiplier != 2.5 || view.Entries[1].Cost.BillingMultiplier != 0.2 {
		t.Fatal("multiplier metadata lost on restart", view.Entries)
	}
}

func TestBillingBackfillPreviewApplyAndReplay(t *testing.T) {
	path := billingMaintenanceFixture(t)
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	preview, err := BackfillBillingMultiplier(path, "initial-0.2", 0.2, true)
	if err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(path)
	if err != nil || string(after) != string(before) {
		t.Fatal("dry run changed source database", err)
	}
	if !preview.DryRun || preview.Events != 3 || preview.Keys != 2 || preview.Windows != 3 ||
		preview.EventTotalBefore != 12.5 || preview.EventTotalAfter != 2.5 || preview.CycleSpentBefore != 275 || preview.CycleSpentAfter != 55 {
		t.Fatalf("preview = %+v", preview)
	}
	result, err := BackfillBillingMultiplier(path, "initial-0.2", 0.2, false)
	if err != nil || result.Replayed || result.DryRun || result.EventTotalAfter != preview.EventTotalAfter {
		t.Fatal(result, err)
	}
	db := maintenanceRawDB(t, path)
	var version int
	if err := db.QueryRow("PRAGMA user_version").Scan(&version); err != nil || version != schemaVersion {
		t.Fatal(version, err)
	}
	var total, global, service, rate float64
	if err := db.QueryRow("SELECT total_usd,billing_multiplier,service_tier_multiplier,applied_cache_write_per_1m FROM request_events WHERE id=1").Scan(&total, &global, &service, &rate); err != nil ||
		total != 2 || global != 0.2 || service != 1 || rate != 1.6 {
		t.Fatal(total, global, service, rate, err)
	}
	if err := db.QueryRow("SELECT total_usd,billing_multiplier,service_tier_multiplier FROM request_events WHERE id=2").Scan(&total, &global, &service); err != nil ||
		total != 0.5 || global != 0.2 || service != 2.5 {
		t.Fatal("fast-mode legacy total with absent breakdown changed", total, global, service, err)
	}
	var raw, body, originalLog, plan string
	if err := db.QueryRow("SELECT cycles_json FROM api_keys WHERE scope='a'").Scan(&raw); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(raw, `"spent_usd":5`) || !strings.Contains(raw, `"used_tokens":9007199254740993`) ||
		!strings.Contains(raw, `"value":9007199254740995`) || !strings.Contains(raw, `"start_at":"2024-01-01T00:00:00Z"`) {
		t.Fatal("cycle snapshot was not scaled directly or metadata changed", raw)
	}
	if err := db.QueryRow("SELECT cycles_json FROM api_keys WHERE scope='deleted'").Scan(&raw); err != nil || !strings.Contains(raw, `"spent_usd":50`) {
		t.Fatal("deleted-key history changed", raw, err)
	}
	if err := db.QueryRow("SELECT windows_json FROM plans").Scan(&plan); err != nil || !strings.Contains(plan, `"amount_usd":100`) {
		t.Fatal("plan limits changed", plan, err)
	}
	if err := db.QueryRow("SELECT input_per_1m FROM prices").Scan(&rate); err != nil || rate != 15 {
		t.Fatal("base model prices changed", rate, err)
	}
	if err := db.QueryRow("SELECT body FROM request_errors WHERE request_event_id=3").Scan(&body); err != nil || body != "preserve failure" {
		t.Fatal(body, err)
	}
	if err := db.QueryRow("SELECT message FROM plugin_logs WHERE id=1").Scan(&originalLog); err != nil || originalLog != "Original expense $10.00" {
		t.Fatal("historical audit log rewritten", originalLog, err)
	}
	replayed, err := BackfillBillingMultiplier(path, "initial-0.2", 0.2, false)
	if err != nil || !replayed.Replayed || replayed.CompletedAt != result.CompletedAt {
		t.Fatal(replayed, err)
	}
	if _, err := BackfillBillingMultiplier(path, "initial-0.2", 0.3, false); err == nil {
		t.Fatal("accepted conflicting operation parameters")
	}
	if _, err := BackfillBillingMultiplier(path, "second-operation", 0.2, false); err == nil {
		t.Fatal("accepted a second historical adjustment")
	}
	if err := db.QueryRow("SELECT total_usd FROM request_events WHERE id=1").Scan(&total); err != nil || total != 2 {
		t.Fatal("replay scaled costs again", total, err)
	}
}

func TestBillingBackfillRollsBackInvalidDataAndSchema(t *testing.T) {
	for _, mutation := range []string{
		`UPDATE api_keys SET cycles_json='invalid' WHERE scope='a'`,
		`UPDATE api_keys SET cycles_json='null' WHERE scope='a'`,
		`UPDATE api_keys SET cycles_json='{"w":{"spent_usd":null}}' WHERE scope='a'`,
		`UPDATE api_keys SET cycles_json='{"w":{"spent_usd":-1}}' WHERE scope='a'`,
		`UPDATE api_keys SET cycles_json='{"w":{"spent_usd":1,"spent_usd":2}}' WHERE scope='a'`,
		`UPDATE api_keys SET cycles_json='{"w":{"spent_usd":1},"w":{"spent_usd":2}}' WHERE scope='a'`,
		`UPDATE request_events SET output_usd='invalid' WHERE id=2`,
		`UPDATE request_events SET output_usd=-1 WHERE id=2`,
		`UPDATE request_events SET output_usd=1e999 WHERE id=2`,
	} {
		t.Run(mutation, func(t *testing.T) {
			path := billingMaintenanceFixture(t)
			db := maintenanceRawDB(t, path)
			if _, err := db.Exec(mutation); err != nil {
				t.Fatal(err)
			}
			if _, err := BackfillBillingMultiplier(path, "bad-data", 0.2, false); err == nil {
				t.Fatal("corrupt history was accepted")
			}
			var version, columns int
			var total float64
			if err := db.QueryRow("PRAGMA user_version").Scan(&version); err != nil || version != 17 {
				t.Fatal("schema migration did not roll back", version, err)
			}
			if err := db.QueryRow("SELECT count(*) FROM pragma_table_info('request_events') WHERE name='billing_multiplier'").Scan(&columns); err != nil || columns != 0 {
				t.Fatal("new column survived failed operation", columns, err)
			}
			if err := db.QueryRow("SELECT total_usd FROM request_events WHERE id=1").Scan(&total); err != nil || total != 10 {
				t.Fatal("cost partially rebilled", total, err)
			}
		})
	}
}

func TestBillingBackfillRejectsMixedHistoryAndOverflow(t *testing.T) {
	path := billingMaintenanceFixture(t)
	if _, err := BackfillBillingMultiplier(path, "overflow", math.MaxFloat64, false); err == nil {
		t.Fatal("overflow accepted")
	}
	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.db.Exec("UPDATE request_events SET billing_multiplier=0.2 WHERE id=2"); err != nil {
		t.Fatal(err)
	}
	db.Close()
	if _, err := BackfillBillingMultiplier(path, "mixed", 0.2, false); err == nil {
		t.Fatal("mixed multiplier history was rescaled")
	}
}

func TestBillingBackfillRollsBackWritesWhenAuditInsertFails(t *testing.T) {
	path := billingMaintenanceFixture(t)
	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var beforeCycles string
	if err := db.db.QueryRow("SELECT cycles_json FROM api_keys WHERE scope='a'").Scan(&beforeCycles); err != nil {
		t.Fatal(err)
	}
	if _, err := db.db.Exec(`CREATE TRIGGER reject_adjustment BEFORE INSERT ON billing_adjustments
		BEGIN SELECT RAISE(ABORT, 'injected audit failure'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := BackfillBillingMultiplier(path, "audit-failure", 0.2, false); err == nil || !strings.Contains(err.Error(), "injected audit failure") {
		t.Fatal("audit insertion did not fail", err)
	}
	var afterCycles string
	var total, multiplier float64
	var auditCount, logCount int
	if err := db.db.QueryRow("SELECT total_usd,billing_multiplier FROM request_events WHERE id=1").Scan(&total, &multiplier); err != nil || total != 10 || multiplier != 1 {
		t.Fatal("event writes were not rolled back", total, multiplier, err)
	}
	if err := db.db.QueryRow("SELECT cycles_json FROM api_keys WHERE scope='a'").Scan(&afterCycles); err != nil || afterCycles != beforeCycles {
		t.Fatal("cycle writes were not rolled back", beforeCycles, afterCycles, err)
	}
	if err := db.db.QueryRow("SELECT count(*) FROM billing_adjustments").Scan(&auditCount); err != nil || auditCount != 0 {
		t.Fatal("failed operation left an audit marker", auditCount, err)
	}
	if err := db.db.QueryRow("SELECT count(*) FROM plugin_logs").Scan(&logCount); err != nil || logCount != 1 {
		t.Fatal("failed operation changed logs", logCount, err)
	}
	if _, err := db.db.Exec("DROP TRIGGER reject_adjustment"); err != nil {
		t.Fatal(err)
	}
	if result, err := BackfillBillingMultiplier(path, "audit-failure", 0.2, false); err != nil || result.Replayed || result.EventTotalBefore != 12.5 {
		t.Fatal("rolled-back operation could not be retried", result, err)
	}
}

func TestBillingBackfillDryRunIncludesWAL(t *testing.T) {
	path := billingMaintenanceFixture(t)
	db := maintenanceRawDB(t, path)
	if _, err := db.Exec("PRAGMA wal_autocheckpoint=0; INSERT INTO request_events(at,scope,total_usd) VALUES(4,'wal',5)"); err != nil {
		t.Fatal(err)
	}
	result, err := BackfillBillingMultiplier(path, "wal-preview", 0.2, true)
	if err != nil || result.Events != 4 || result.EventTotalBefore != 17.5 || result.EventTotalAfter != 3.5 {
		t.Fatal("consistent preview omitted committed WAL rows", result, err)
	}
	var version int
	if err := db.QueryRow("PRAGMA user_version").Scan(&version); err != nil || version != 17 {
		t.Fatal("dry run migrated the live source", version, err)
	}
}

func TestBillingBackfillRequiresExistingDatabaseAndValidArguments(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "missing.db")
	if _, err := BackfillBillingMultiplier(missing, "initial", 0.2, false); err == nil {
		t.Fatal("missing database accepted")
	}
	if _, err := os.Stat(missing); !os.IsNotExist(err) {
		t.Fatal("maintenance created a missing database", err)
	}
	path := billingMaintenanceFixture(t)
	for _, multiplier := range []float64{0, -1, math.NaN(), math.Inf(1)} {
		if _, err := BackfillBillingMultiplier(path, "initial", multiplier, false); err == nil {
			t.Fatal("invalid multiplier accepted", multiplier)
		}
	}
	if _, err := BackfillBillingMultiplier(path, "", 0.2, false); err == nil {
		t.Fatal("empty operation ID accepted")
	}
}

func TestUniqueJSONObjectPreservesExactUnrelatedValues(t *testing.T) {
	object, err := uniqueJSONObject([]byte(`{"used_tokens":9223372036854775807,"custom":{"nested":1234567890123456789},"spent_usd":2}`))
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(object)
	if err != nil || !strings.Contains(string(raw), "9223372036854775807") || !strings.Contains(string(raw), "1234567890123456789") {
		t.Fatal(string(raw), err)
	}
}
