package sqlite

import (
	"path/filepath"
	"testing"
	"time"

	"cpa-key-billing/internal/billing"
)

func TestV19ServiceTierMigrationPreservesHistoryAndRollsBack(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(map[bool]string{false: "success", true: "rollback"}[fail], func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "billing.db")
			db := openDatabase(t, path)
			if _, err := db.db.Exec(`INSERT INTO prices(model_id,input_per_1m,output_per_1m) VALUES('dummy-model',2,10);
   INSERT INTO request_events(id,at,scope,failed,total_usd,billing_multiplier,service_tier_multiplier) VALUES(1,1,'dummy',0,.123,.2,2.5),(2,2,'dummy',1,0,1,1),(3,3,'deleted',0,0,1,1);
   INSERT INTO request_errors(request_event_id,status_code,body) VALUES(2,502,'dummy failure');
   ALTER TABLE prices DROP COLUMN service_tiers_json;
   ALTER TABLE request_events DROP COLUMN requested_model; ALTER TABLE request_events DROP COLUMN reported_model; ALTER TABLE request_events DROP COLUMN stream;
   ALTER TABLE request_events DROP COLUMN token_usage_json;
   PRAGMA user_version=18;`); err != nil {
				t.Fatal(err)
			}
			if !fail {
				if _, err := db.db.Exec(`ALTER TABLE request_events DROP COLUMN pricing_json`); err != nil {
					t.Fatal(err)
				}
			}
			db.Close()
			migrated, err := Open(path)
			if fail {
				if err == nil {
					migrated.Close()
					t.Fatal("partial schema accepted")
				}
				raw := maintenanceRawDB(t, path)
				var version, columns int
				if err := raw.QueryRow("PRAGMA user_version").Scan(&version); err != nil || version != 18 {
					t.Fatal(version, err)
				}
				if err := raw.QueryRow("SELECT count(*) FROM pragma_table_info('prices') WHERE name='service_tiers_json'").Scan(&columns); err != nil || columns != 0 {
					t.Fatal("partial migration committed", columns, err)
				}
				var amount float64
				if err := raw.QueryRow("SELECT total_usd FROM request_events WHERE id=1").Scan(&amount); err != nil || amount != .123 {
					t.Fatal(amount, err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			view, err := migrated.RequestEvents(billing.RequestEventQuery{}, time.Time{})
			if err != nil || len(view.Entries) != 3 {
				t.Fatal(view, err)
			}
			if view.Entries[2].Cost.TotalUSD != .123 || view.Entries[2].Cost.ServiceTierMultiplier != 2.5 || view.Entries[1].ErrorBody != "dummy failure" {
				t.Fatal(view)
			}
			for _, event := range view.Entries {
				if event.Cost.Pricing != (billing.PricingMetadata{}) {
					t.Fatal("invented historical metadata", event)
				}
			}
			migrated.Close()
			reopened, err := Open(path)
			if err != nil {
				t.Fatal(err)
			}
			reopened.Close()
		})
	}
}

func TestServiceTierPriceAndMetadataRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "billing.db")
	db := openDatabase(t, path)
	in, out, zero := 3.0, 7.0, 0.0
	card := billing.CustomPrice{ModelID: "dummy-model", PriceRates: billing.PriceRates{InputPer1M: 1, OutputPer1M: 2, ServiceTiers: map[string]billing.TierPriceRates{
		"priority": {InputPer1M: &in, OutputPer1M: &out, CacheReadPer1M: &zero, LongContext: &billing.LongContextPrice{ThresholdInputTokens: 1000, InputPer1M: 8, OutputPer1M: 13}},
	}}}
	if err := db.UpsertPrice(card); err != nil {
		t.Fatal(err)
	}
	// An older writer omitting the new field preserves explicit tier prices.
	if err := db.UpsertPrice(billing.CustomPrice{ModelID: "dummy-model", PriceRates: billing.PriceRates{InputPer1M: 2, OutputPer1M: 4}}); err != nil {
		t.Fatal(err)
	}
	meta := billing.PricingMetadata{ServiceTier: "priority", TierSource: "request", Method: "base_fallback", TierFallback: "missing_response_tier", PriceFallback: "missing_priority_price"}
	event := billing.RequestEvent{Scope: "dummy", At: time.Now(), Cost: billing.Cost{Pricing: meta, TotalUSD: .015, BillingMultiplier: .2, ServiceTierMultiplier: 1}}
	if err := db.Save(billing.NewState(), billing.Changes{NormalRequestEvents: []billing.RequestEvent{event}}); err != nil {
		t.Fatal(err)
	}
	db.Close()
	db = openDatabase(t, path)
	defer db.Close()
	state := billing.NewState()
	if err := db.loadPrices(state); err != nil {
		t.Fatal(err)
	}
	got := state.Prices["dummy-model"].ServiceTiers["priority"]
	if got.InputPer1M == nil || *got.InputPer1M != 3 || *got.CacheReadPer1M != 0 || got.LongContext.OutputPer1M != 13 {
		t.Fatal(got)
	}
	view, err := db.RequestEvents(billing.RequestEventQuery{}, time.Time{})
	if err != nil || len(view.Entries) != 1 || view.Entries[0].Cost.Pricing != meta || view.Entries[0].Cost.TotalUSD != .015 {
		t.Fatal(view, err)
	}
	card.ServiceTiers = map[string]billing.TierPriceRates{}
	if err := db.UpsertPrice(card); err != nil {
		t.Fatal(err)
	}
	state = billing.NewState()
	if err := db.loadPrices(state); err != nil || len(state.Prices["dummy-model"].ServiceTiers) != 0 {
		t.Fatal(state, err)
	}
}
