package sqlite

import (
	"database/sql"
	"encoding/json"
	"fmt"

	"cpa-key-billing/internal/billing"
)

func (d *DB) LoadOfficialPrices() (billing.OfficialPriceCatalog, error) {
	var raw []byte
	err := d.db.QueryRow(`SELECT catalog_json FROM official_price_catalog WHERE id = 1`).Scan(&raw)
	if err == sql.ErrNoRows {
		return billing.OfficialPriceCatalog{}, nil
	}
	if err != nil {
		return billing.OfficialPriceCatalog{}, fmt.Errorf("Read official price catalog: %w", err)
	}
	catalog, err := billing.DecodeOfficialPriceCatalog(raw)
	if err != nil {
		return catalog, fmt.Errorf("Read official price catalog: %w", err)
	}
	return catalog, nil
}

func (d *DB) SaveOfficialPrices(catalog billing.OfficialPriceCatalog) error {
	if err := catalog.Validate(); err != nil {
		return err
	}
	raw, err := json.Marshal(catalog)
	if err != nil {
		return err
	}
	_, err = d.db.Exec(`INSERT INTO official_price_catalog (id, catalog_json) VALUES (1, ?)
        ON CONFLICT(id) DO UPDATE SET catalog_json = excluded.catalog_json`, string(raw))
	if err != nil {
		return fmt.Errorf("Save official price catalog: %w", err)
	}
	return nil
}
