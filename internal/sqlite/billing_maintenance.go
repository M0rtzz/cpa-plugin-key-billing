package sqlite

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/mattn/go-sqlite3"
)

// BillingBackfillResult is also persisted as the immutable audit of a completed
// operation. Totals cover retained events and stored quota windows separately:
// one request can consume several windows, and older events may have expired.
type BillingBackfillResult struct {
	OperationID      string    `json:"operation_id"`
	Multiplier       float64   `json:"multiplier"`
	CompletedAt      time.Time `json:"completed_at"`
	Events           int64     `json:"events"`
	Keys             int64     `json:"keys"`
	Windows          int64     `json:"windows"`
	EventTotalBefore float64   `json:"event_total_before_usd"`
	EventTotalAfter  float64   `json:"event_total_after_usd"`
	CycleSpentBefore float64   `json:"cycle_spent_before_usd"`
	CycleSpentAfter  float64   `json:"cycle_spent_after_usd"`
	DryRun           bool      `json:"dry_run,omitempty"`
	Replayed         bool      `json:"replayed,omitempty"`
}

// BackfillBillingMultiplier is an offline maintenance operation. Stop every
// process using this database before applying it: SQLite serializes writes but
// cannot invalidate a running plugin's in-memory quota snapshots. A dry run
// executes the identical operation on a consistent, private SQLite backup and
// never migrates or updates the source database.
func BackfillBillingMultiplier(path, operationID string, multiplier float64, dryRun bool) (BillingBackfillResult, error) {
	if operationID = strings.TrimSpace(operationID); operationID == "" || len(operationID) > 200 || strings.ContainsAny(operationID, "\r\n\x00") {
		return BillingBackfillResult{}, fmt.Errorf("operation-id must contain 1 to 200 characters without line breaks")
	}
	if multiplier <= 0 || math.IsNaN(multiplier) || math.IsInf(multiplier, 0) {
		return BillingBackfillResult{}, fmt.Errorf("multiplier must be a finite positive number")
	}
	if strings.TrimSpace(path) == "" {
		return BillingBackfillResult{}, fmt.Errorf("an existing database path is required")
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return BillingBackfillResult{}, err
	}
	info, err := os.Stat(absolute)
	if err != nil {
		return BillingBackfillResult{}, fmt.Errorf("Inspect billing database: %w", err)
	}
	if !info.Mode().IsRegular() {
		return BillingBackfillResult{}, fmt.Errorf("billing database must be an existing regular file")
	}
	if dryRun {
		directory, err := os.MkdirTemp("", "cpa-billing-preview-")
		if err != nil {
			return BillingBackfillResult{}, err
		}
		defer os.RemoveAll(directory)
		copyPath := filepath.Join(directory, "state.db")
		if err := backupBillingDatabase(absolute, copyPath); err != nil {
			return BillingBackfillResult{}, err
		}
		result, err := applyBillingBackfill(copyPath, operationID, multiplier)
		result.DryRun = true
		return result, err
	}
	return applyBillingBackfill(absolute, operationID, multiplier)
}

func maintenanceDatabase(path, mode string) (*sql.DB, error) {
	u := url.URL{Scheme: "file", Path: path, OmitHost: true}
	q := url.Values{"mode": {mode}, "_busy_timeout": {"5000"}, "_foreign_keys": {"on"}}
	if mode != "ro" {
		q.Set("_txlock", "immediate")
	}
	u.RawQuery = q.Encode()
	handle, err := sql.Open("sqlite3", u.String())
	if err == nil {
		handle.SetMaxOpenConns(1)
	}
	return handle, err
}

func backupBillingDatabase(sourcePath, targetPath string) error {
	source, err := maintenanceDatabase(sourcePath, "ro")
	if err != nil {
		return err
	}
	defer source.Close()
	file, err := os.OpenFile(targetPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	target, err := maintenanceDatabase(targetPath, "rw")
	if err != nil {
		return err
	}
	defer target.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	from, err := source.Conn(ctx)
	if err != nil {
		return err
	}
	defer from.Close()
	to, err := target.Conn(ctx)
	if err != nil {
		return err
	}
	defer to.Close()
	return from.Raw(func(rawSource any) error {
		return to.Raw(func(rawTarget any) error {
			backup, err := rawTarget.(*sqlite3.SQLiteConn).Backup("main", rawSource.(*sqlite3.SQLiteConn), "main")
			if err != nil {
				return fmt.Errorf("Create consistent billing snapshot: %w", err)
			}
			done, stepErr := backup.Step(-1)
			finishErr := backup.Finish()
			if stepErr != nil {
				return stepErr
			}
			if finishErr != nil {
				return finishErr
			}
			if !done {
				return fmt.Errorf("billing database is busy; stop its writers and retry the preview")
			}
			return nil
		})
	})
}

func applyBillingBackfill(path, operationID string, multiplier float64) (BillingBackfillResult, error) {
	handle, err := maintenanceDatabase(path, "rw")
	if err != nil {
		return BillingBackfillResult{}, err
	}
	defer handle.Close()
	database := &DB{db: handle, path: path}
	result := BillingBackfillResult{OperationID: operationID, Multiplier: multiplier}
	err = database.transact(func(tx *sql.Tx) error {
		var version int
		if err := tx.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
			return err
		}
		if version == 0 {
			return fmt.Errorf("maintenance requires an initialized billing database")
		}
		// Migration, rebilling, and its completion marker succeed or roll back
		// together. Do not call Open or Load: neither pruning nor startup writes
		// belong in an offline history operation.
		if err := database.initSchema(tx); err != nil {
			return err
		}
		var priorMultiplier float64
		var priorJSON string
		err := tx.QueryRow("SELECT multiplier,result_json FROM billing_adjustments WHERE operation_id=?", operationID).Scan(&priorMultiplier, &priorJSON)
		if err == nil {
			if priorMultiplier != multiplier {
				return fmt.Errorf("operation-id was already used with a different multiplier")
			}
			if err := json.Unmarshal([]byte(priorJSON), &result); err != nil {
				return fmt.Errorf("Read prior billing adjustment: %w", err)
			}
			result.Replayed = true
			return nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		var adjusted bool
		if err := tx.QueryRow("SELECT EXISTS(SELECT 1 FROM billing_adjustments)").Scan(&adjusted); err != nil {
			return err
		}
		if adjusted {
			return fmt.Errorf("historical billing was already adjusted; reuse its operation-id to inspect the result")
		}
		if err := inspectBackfillEvents(tx, multiplier, &result); err != nil {
			return err
		}
		cycles, err := prepareBackfillCycles(tx, multiplier, &result)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(`UPDATE request_events SET
			total_usd=total_usd*?, uncached_input_usd=uncached_input_usd*?,
			cache_read_usd=cache_read_usd*?, cache_write_usd=cache_write_usd*?, output_usd=output_usd*?,
			applied_input_per_1m=applied_input_per_1m*?, applied_output_per_1m=applied_output_per_1m*?,
			applied_cache_read_per_1m=applied_cache_read_per_1m*?, applied_cache_write_per_1m=applied_cache_write_per_1m*?,
			billing_multiplier=?`, multiplier, multiplier, multiplier, multiplier, multiplier,
			multiplier, multiplier, multiplier, multiplier, multiplier); err != nil {
			return err
		}
		for _, key := range cycles {
			if _, err := tx.Exec("UPDATE api_keys SET cycles_json=? WHERE scope=?", key.json, key.scope); err != nil {
				return err
			}
		}
		result.CompletedAt = time.Now().UTC()
		raw, err := json.Marshal(result)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(`INSERT INTO billing_adjustments(operation_id,multiplier,completed_at,result_json) VALUES(?,?,?,?)`,
			operationID, multiplier, nanos(result.CompletedAt), string(raw)); err != nil {
			return err
		}
		_, err = tx.Exec("INSERT INTO plugin_logs(at,level,message) VALUES(?,?,?)", nanos(result.CompletedAt), "info",
			fmt.Sprintf("Historical billing adjusted: operation=%q, multiplier=%g, events=%d, quota_windows=%d; prior log messages preserve their original amounts", operationID, multiplier, result.Events, result.Windows))
		return err
	})
	if err != nil {
		return BillingBackfillResult{}, err
	}
	return result, nil
}

func inspectBackfillEvents(tx *sql.Tx, multiplier float64, result *BillingBackfillResult) error {
	rows, err := tx.Query(`SELECT id,billing_multiplier,service_tier_multiplier,total_usd,uncached_input_usd,
		cache_read_usd,cache_write_usd,output_usd,applied_input_per_1m,applied_output_per_1m,
		applied_cache_read_per_1m,applied_cache_write_per_1m FROM request_events`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var values [11]any
		destinations := []any{&id}
		for i := range values {
			destinations = append(destinations, &values[i])
		}
		if err := rows.Scan(destinations...); err != nil {
			return err
		}
		for i, raw := range values {
			value, err := storedMoney(raw)
			if err != nil {
				return fmt.Errorf("Invalid monetary data in request event %d: %w", id, err)
			}
			if i == 0 {
				if value != 1 {
					return fmt.Errorf("request event %d already has a global billing multiplier; mixed history cannot be backfilled", id)
				}
				continue
			}
			if i == 1 && value != 1 && value != 2.5 {
				return fmt.Errorf("request event %d has an unsupported service-tier multiplier", id)
			}
			if !finiteNonnegative(value * multiplier) {
				return fmt.Errorf("request event %d would overflow after applying the multiplier", id)
			}
			if i == 2 {
				if err := addBackfillTotal(&result.EventTotalBefore, value); err != nil {
					return err
				}
				if err := addBackfillTotal(&result.EventTotalAfter, value*multiplier); err != nil {
					return err
				}
			}
		}
		result.Events++
	}
	return rows.Err()
}

type backfillKeyCycles struct{ scope, json string }

func prepareBackfillCycles(tx *sql.Tx, multiplier float64, result *BillingBackfillResult) ([]backfillKeyCycles, error) {
	rows, err := tx.Query("SELECT scope,cycles_json FROM api_keys")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var changed []backfillKeyCycles
	for rows.Next() {
		var key backfillKeyCycles
		if err := rows.Scan(&key.scope, &key.json); err != nil {
			return nil, err
		}
		cycles, err := uniqueJSONObject([]byte(key.json))
		if err != nil {
			return nil, fmt.Errorf("Invalid quota cycle JSON: %w", err)
		}
		for id, raw := range cycles {
			cycle, err := uniqueJSONObject(raw)
			if err != nil {
				return nil, fmt.Errorf("Invalid quota window JSON: %w", err)
			}
			var spent float64
			if raw, exists := cycle["spent_usd"]; exists {
				if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
					return nil, fmt.Errorf("quota spent_usd must be a number")
				}
				if err := json.Unmarshal(raw, &spent); err != nil {
					return nil, fmt.Errorf("Invalid quota spent_usd: %w", err)
				}
			}
			if !finiteNonnegative(spent) || !finiteNonnegative(spent*multiplier) {
				return nil, fmt.Errorf("quota spent_usd must remain finite and non-negative")
			}
			if err := addBackfillTotal(&result.CycleSpentBefore, spent); err != nil {
				return nil, err
			}
			if err := addBackfillTotal(&result.CycleSpentAfter, spent*multiplier); err != nil {
				return nil, err
			}
			cycle["spent_usd"], _ = json.Marshal(spent * multiplier)
			cycles[id], _ = json.Marshal(cycle)
			result.Windows++
		}
		if len(cycles) > 0 {
			raw, err := json.Marshal(cycles)
			if err != nil {
				return nil, err
			}
			key.json = string(raw)
			changed = append(changed, key)
			result.Keys++
		}
	}
	return changed, rows.Err()
}

// Raw messages preserve unknown fields and exact integers, including counters
// above float64's integer precision. Duplicate object members are ambiguous and
// must not silently disappear during a financial adjustment.
func uniqueJSONObject(raw []byte) (map[string]json.RawMessage, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	start, err := decoder.Token()
	if err != nil || start != json.Delim('{') {
		return nil, fmt.Errorf("expected a JSON object")
	}
	object := make(map[string]json.RawMessage)
	for decoder.More() {
		member, err := decoder.Token()
		if err != nil {
			return nil, err
		}
		name := member.(string)
		if _, exists := object[name]; exists {
			return nil, fmt.Errorf("duplicate object member")
		}
		var value json.RawMessage
		if err := decoder.Decode(&value); err != nil {
			return nil, err
		}
		object[name] = value
	}
	if _, err := decoder.Token(); err != nil {
		return nil, err
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("unexpected data after JSON object")
	}
	return object, nil
}

func storedMoney(raw any) (float64, error) {
	var value float64
	switch raw := raw.(type) {
	case int64:
		value = float64(raw)
	case float64:
		value = raw
	default:
		return 0, fmt.Errorf("expected a stored numeric value")
	}
	if !finiteNonnegative(value) {
		return 0, fmt.Errorf("expected a finite non-negative value")
	}
	return value, nil
}

func finiteNonnegative(value float64) bool {
	return value >= 0 && !math.IsNaN(value) && !math.IsInf(value, 0)
}

func addBackfillTotal(total *float64, value float64) error {
	next := *total + value
	if !finiteNonnegative(next) {
		return fmt.Errorf("billing adjustment audit total would overflow")
	}
	*total = next
	return nil
}
