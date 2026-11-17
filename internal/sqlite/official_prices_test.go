package sqlite

import (
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"cpa-key-billing/internal/billing"
)

func TestOfficialCatalogPersistenceAndMigrationPreserveBilling(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	db := openDatabase(t, path)
	if _, err := db.db.Exec(`DROP TABLE official_price_catalog; PRAGMA user_version=21;
        INSERT INTO request_events(id,at,scope,failed,total_usd) VALUES(1,1,'dummy',0,.125),(2,2,'dummy',1,0),(3,3,'dummy',0,0);
        INSERT INTO request_errors(request_event_id,status_code,body) VALUES(2,502,'dummy failure');
        INSERT INTO prices(position,model_id,input_per_1m,output_per_1m) VALUES(0,'dummy-model',99,100);
        CREATE TRIGGER no_reprice BEFORE UPDATE ON request_events BEGIN SELECT RAISE(ABORT,'history changed'); END;
        CREATE TRIGGER no_delete BEFORE DELETE ON request_events BEGIN SELECT RAISE(ABORT,'history deleted'); END;`); err != nil {
		t.Fatal(err)
	}
	db.Close()
	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	catalog := billing.OfficialPriceCatalog{Metadata: billing.OfficialPriceMetadata{SourceURL: billing.OpenAIPricesURL, ContentHash: "dummy-hash", Version: 1, ModelCount: 1, Usable: true, FetchedAt: time.Now().UTC()}, Models: []billing.OfficialModelPrice{{ModelID: "dummy-model", Quotes: []billing.OfficialPriceQuote{{Tier: "standard", Unit: "USD / 1M tokens", Values: []billing.OfficialPriceValue{{Metric: "Input", Value: "$1.25"}, {Metric: "Output", Value: "$10.00"}}}}}}}
	if err := db.SaveOfficialPrices(catalog); err != nil {
		t.Fatal(err)
	}
	db.Close()
	db, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	loaded, err := db.LoadOfficialPrices()
	if err != nil {
		t.Fatal(err)
	}
	want, _ := json.Marshal(catalog)
	got, _ := json.Marshal(loaded)
	if string(want) != string(got) {
		t.Fatal("official snapshot did not survive restart")
	}
	bad := catalog
	bad.Metadata.ModelCount = 5
	if err := db.SaveOfficialPrices(bad); err == nil {
		t.Fatal("inconsistent snapshot accepted")
	}
	var count int
	var cost float64
	if err := db.db.QueryRow(`SELECT count(*),sum(total_usd) FROM request_events`).Scan(&count, &cost); err != nil || count != 3 || cost != .125 {
		t.Fatal(count, cost, err)
	}
	var input float64
	if err := db.db.QueryRow(`SELECT input_per_1m FROM prices WHERE model_id='dummy-model'`).Scan(&input); err != nil || input != 99 {
		t.Fatal("custom prices changed", input, err)
	}
	if err := db.db.QueryRow(`SELECT count(*) FROM request_errors WHERE body='dummy failure'`).Scan(&count); err != nil || count != 1 {
		t.Fatal("failure lost", count, err)
	}
}

func TestOfficialCatalogMigrationConflictRollsBack(t *testing.T) {
	path := filepath.Join(t.TempDir(), "conflict.db")
	db := openDatabase(t, path)
	if _, err := db.db.Exec(`PRAGMA user_version=21;`); err != nil {
		t.Fatal(err)
	}
	db.Close()
	if db, err := Open(path); err == nil {
		db.Close()
		t.Fatal("unexpected schema accepted")
	}
	raw := maintenanceRawDB(t, path)
	var version int
	if err := raw.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil || version != 21 {
		t.Fatal("migration conflict did not roll back", version, err)
	}
}
