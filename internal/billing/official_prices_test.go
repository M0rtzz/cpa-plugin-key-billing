package billing

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

func officialFixture(t *testing.T) []byte {
	t.Helper()
	raw, err := os.ReadFile("testdata/openai_prices.md")
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestParseOfficialCatalogAndTokenCards(t *testing.T) {
	catalog, err := parseOpenAIPrices(officialFixture(t))
	if err != nil {
		t.Fatal(err)
	}
	if len(catalog.Models) != 74 || catalog.ContextThreshold != 272000 {
		t.Fatalf("catalog: %d models, threshold=%d", len(catalog.Models), catalog.ContextThreshold)
	}
	for _, tc := range []struct {
		model, tier          string
		input, output, cache float64
		long                 bool
	}{
		{"gpt-6.1-terra", "standard", 1, 5, .05, true},
		{"gpt-6.1-terra", "priority", 2, 10, .1, true},
		{"gpt-6.1-terra", "flex", .5, 2.5, .025, true},
		{"gpt-5.6-sol", "standard", 4, 20, .4, true},
		{"gpt-5.5", "priority", 12.5, 75, 1.25, false},
		{"gpt-image-2.6-flare", "standard", 5, 30, 1.25, false},
		{"gpt-image-2.5-sunburst", "batch", 2.5, 15, .625, false},
	} {
		model, ok := catalog.lookup(tc.model)
		if !ok {
			t.Fatal("missing official model", tc.model)
		}
		rates, ok := model.tokenRates(tc.tier, catalog.ContextThreshold)
		if !ok || rates.InputPer1M != tc.input || rates.OutputPer1M != tc.output || rates.CacheReadPer1M == nil || *rates.CacheReadPer1M != tc.cache || (rates.LongContext != nil) != tc.long || rates.CacheWritePer1M != nil {
			t.Fatalf("%s %s = %+v", tc.model, tc.tier, rates)
		}
	}
	image, _ := catalog.lookup("gpt-image-2")
	if _, ok := image.tokenRates("flex", catalog.ContextThreshold); ok {
		t.Fatal("Batch was incorrectly treated as Flex")
	}
	for _, name := range []string{"gpt-realtime-2.1", "gpt-live-1", "tts-1"} {
		model, ok := catalog.lookup(name)
		if !ok {
			t.Fatal("missing display-only model", name)
		}
		if _, ok := model.tokenRates("standard", catalog.ContextThreshold); ok {
			t.Fatal("non-scalar prices flattened", name)
		}
	}
}

func TestOfficialParserRejectsIncompleteOrNonMarkdownData(t *testing.T) {
	raw := string(officialFixture(t))
	for _, text := range []string{"<html>Pricing</html>", "# Pricing\nNo prices", strings.Replace(raw, "| $1.00 | $5.00 | $0.05 |", "| $1.00 |", 1), strings.Replace(raw, "| Model | Short context input", "| Model | Mystery input", 1)} {
		if _, err := parseOpenAIPrices([]byte(text)); err == nil {
			t.Fatal("invalid official document accepted")
		}
	}
	updated := strings.ReplaceAll(raw, "gpt-6.1-terra", "future-model-without-hardcoded-id")
	catalog, err := parseOpenAIPrices([]byte(updated))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := catalog.lookup("future-model-without-hardcoded-id"); !ok {
		t.Fatal("new model was filtered out")
	}
}

func TestOfficialPriceSyncPreservesOverridesAndLastGoodSnapshot(t *testing.T) {
	s, repo := newStoreWithRepository(t)
	raw := officialFixture(t)
	s.downloadOfficialPrices = func(context.Context) ([]byte, error) { return raw, nil }
	if _, err := s.UpsertPrice(CustomPrice{ModelID: "gpt-5.6-sol", PriceRates: PriceRates{InputPer1M: 99, OutputPer1M: 100}}); err != nil {
		t.Fatal(err)
	}
	result, err := s.RefreshOfficialPrices()
	if err != nil || !result.Changed || !result.Metadata.Usable {
		t.Fatal(result, err)
	}
	price, _, err := s.ResolveModelPrice("gpt-5.6-sol", "gpt-5.6-sol", false)
	if err != nil || price.Source != PriceSourceCustom || price.InputPer1M != 99 {
		t.Fatal(price, err)
	}
	price, _, err = s.ResolveModelPrice("gpt-6.1-terra", "gpt-6.1-terra", false)
	if err != nil || price.Source != PriceSourceOfficial || price.InputPer1M != 1 {
		t.Fatal(price, err)
	}
	for _, tc := range []struct {
		tier     string
		tokens   int64
		rate     float64
		fallback bool
	}{
		{"priority", 100, 2, false}, {"flex", 100, .5, false},
	} {
		resolved, meta := resolveAPIServicePrice(price, "gpt-6.1-terra", UsageEvent{ResponseServiceTier: tc.tier, Breakdown: TokenBreakdown{Input: TokenInputBreakdown{TotalTokens: tc.tokens}}})
		if resolved.InputPer1M != tc.rate || (meta.PriceFallback != "") != tc.fallback {
			t.Fatal(resolved, meta)
		}
	}
	image, _, err := s.ResolveModelPrice("gpt-image-2.5-flare", "gpt-image-2.5-flare", false)
	if err != nil || image.Source != PriceSourceOfficial || image.InputPer1M != 5 || image.OutputPer1M != 30 {
		t.Fatal(image, err)
	}
	resolved, meta := resolveAPIServicePrice(image, "gpt-image-2.5-flare", UsageEvent{ResponseServiceTier: "flex"})
	if resolved.InputPer1M != 5 || meta.PriceFallback != "missing_flex_price" {
		t.Fatal("image Flex was invented", resolved, meta)
	}
	rows, err := s.ModelPriceRowsWithOfficial([]string{"gpt-image-2.5-flare"}, true, true)
	if err != nil || len(rows) != len(s.officialPrices.Models) {
		t.Fatal(len(rows), err)
	}
	inModels := 0
	for _, row := range rows {
		if row.InModels {
			inModels++
		}
	}
	if inModels != 1 {
		t.Fatal("official sync enabled CPA models", inModels)
	}
	again, err := s.RefreshOfficialPrices()
	if err != nil || again.Changed || again.Metadata.Version != result.Metadata.Version {
		t.Fatal(again, err)
	}
	s.downloadOfficialPrices = func(context.Context) ([]byte, error) { return []byte("<html>challenge</html>"), nil }
	failed, err := s.RefreshOfficialPrices()
	if err == nil || !failed.Metadata.Usable || failed.Metadata.Version != result.Metadata.Version || failed.Metadata.LastError == "" {
		t.Fatal(failed, err)
	}
	if repo.officialPrices.Metadata.ContentHash != result.Metadata.ContentHash {
		t.Fatal("failed refresh replaced stored prices")
	}
	before := s.OfficialPriceMetadata()
	repo.fail = errors.New("disk full")
	s.downloadOfficialPrices = func(context.Context) ([]byte, error) {
		return []byte(strings.ReplaceAll(string(raw), "$1.00", "$1.01")), nil
	}
	if _, err = s.RefreshOfficialPrices(); err == nil {
		t.Fatal("failed persistence was published")
	}
	if after := s.OfficialPriceMetadata(); after.ContentHash != before.ContentHash {
		t.Fatal("uncommitted official prices became visible")
	}
	s.officialPriceSyncing.Store(true)
	if _, err = s.RefreshOfficialPrices(); KindOf(err) != KindConflict {
		t.Fatal("concurrent sync not rejected", err)
	}
	s.officialPriceSyncing.Store(false)
}

func TestOfficialDownloadAndHTTPFailure(t *testing.T) {
	raw := officialFixture(t)
	status := http.StatusOK
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			t.Error("official price sync sent credentials")
		}
		w.WriteHeader(status)
		w.Write(raw)
	}))
	defer server.Close()
	client := server.Client()
	transport := client.Transport
	client.Transport = referencePriceTransport(func(req *http.Request) (*http.Response, error) {
		clone := req.Clone(req.Context())
		clone.URL.Scheme, clone.URL.Host = "http", strings.TrimPrefix(server.URL, "http://")
		return transport.RoundTrip(clone)
	})
	if got, err := fetchOpenAIPrices(context.Background(), client); err != nil || string(got) != string(raw) {
		t.Fatal(err)
	}
	status = http.StatusForbidden
	if _, err := fetchOpenAIPrices(context.Background(), client); err == nil {
		t.Fatal("HTTP failure accepted")
	}
}
