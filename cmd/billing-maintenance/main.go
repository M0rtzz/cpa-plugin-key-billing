// billing-maintenance performs explicit offline accounting operations. It must
// never run an applying operation while CPA has the same database open.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"

	"cpa-key-billing/internal/sqlite"
)

func main() {
	if err := run(os.Args[1:], os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(args []string, output, diagnostic io.Writer) error {
	flags := flag.NewFlagSet("billing-maintenance", flag.ContinueOnError)
	flags.SetOutput(diagnostic)
	path := flags.String("db", "", "Existing billing database (stop CPA before applying)")
	operationID := flags.String("operation-id", "", "Unique, stable ID for this one-time historical adjustment")
	multiplier := flags.Float64("multiplier", 0, "Positive multiplier applied once to historical costs and spent quota")
	dryRun := flags.Bool("dry-run", false, "Preview against a consistent temporary copy without changing the source database")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("unexpected positional arguments")
	}
	result, err := sqlite.BackfillBillingMultiplier(*path, *operationID, *multiplier, *dryRun)
	if err != nil {
		return err
	}
	encoder := json.NewEncoder(output)
	encoder.SetIndent("", "  ")
	return encoder.Encode(result)
}
