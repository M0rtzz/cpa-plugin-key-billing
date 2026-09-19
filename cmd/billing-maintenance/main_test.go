package main

import (
	"bytes"
	"encoding/json"
	"path/filepath"
	"testing"

	"cpa-key-billing/internal/sqlite"
)

func TestMaintenanceCLIRequiresExplicitArguments(t *testing.T) {
	for _, args := range [][]string{nil, {"--db", "missing.db"}, {"--unknown"}, {"unexpected"}} {
		var output, diagnostic bytes.Buffer
		if err := run(args, &output, &diagnostic); err == nil {
			t.Fatal("invalid arguments accepted", args)
		}
	}
}

func TestMaintenanceCLIPrintsPreviewAndReplay(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	db, err := sqlite.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	db.Close()
	base := []string{"--db", path, "--operation-id", "test-adjustment", "--multiplier", "0.2"}
	for i, args := range [][]string{append(append([]string{}, base...), "--dry-run"), base, base} {
		var output, diagnostic bytes.Buffer
		if err := run(args, &output, &diagnostic); err != nil {
			t.Fatal(err, diagnostic.String())
		}
		var result sqlite.BillingBackfillResult
		if err := json.Unmarshal(output.Bytes(), &result); err != nil {
			t.Fatal(err)
		}
		if result.OperationID != "test-adjustment" || result.Multiplier != 0.2 || result.DryRun != (i == 0) || result.Replayed != (i == 2) {
			t.Fatalf("result = %+v", result)
		}
	}
}
