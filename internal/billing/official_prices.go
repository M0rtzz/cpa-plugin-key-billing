package billing

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"strings"
)

type OfficialPriceRefreshResult struct {
	Metadata OfficialPriceMetadata `json:"metadata"`
	Changed  bool                  `json:"changed"`
}

func (s *Store) OfficialPriceMetadata() OfficialPriceMetadata {
	s.mu.RLock()
	defer s.mu.RUnlock()
	metadata := s.officialPrices.Metadata
	metadata.SourceURL = OpenAIPricesURL
	metadata.Refreshing = s.officialPriceSyncing.Load()
	return metadata
}

// Download and parse within this management call. Billing keeps reading the
// previous immutable snapshot until the complete replacement is committed.
func (s *Store) RefreshOfficialPrices() (OfficialPriceRefreshResult, error) {
	return s.RefreshOfficialPricesWithDownloader(s.downloadOfficialPrices)
}

// Management calls can use the host's HTTP bridge to inherit CPA's configured
// proxy. The download still finishes synchronously before the snapshot write.
func (s *Store) RefreshOfficialPricesWithDownloader(download func(context.Context) ([]byte, error)) (OfficialPriceRefreshResult, error) {
	if !s.officialPriceSyncing.CompareAndSwap(false, true) {
		return OfficialPriceRefreshResult{}, conflictf("Official price synchronization is already running")
	}
	defer s.officialPriceSyncing.Store(false)
	s.mu.RLock()
	repo := s.repo
	before := s.officialPrices
	s.mu.RUnlock()
	if repo == nil {
		return OfficialPriceRefreshResult{}, fmt.Errorf("The billing database is not ready")
	}
	ctx, cancel := context.WithTimeout(context.Background(), referencePriceOperationTimeout)
	defer cancel()
	raw, err := download(ctx)
	next := before
	changed := false
	if err == nil {
		hash := fmt.Sprintf("%x", sha256.Sum256(raw))
		changed = !before.Metadata.Usable || before.Metadata.ContentHash != hash
		if changed {
			next, err = parseOpenAIPrices(raw)
			if err == nil && before.Metadata.Usable && len(next.Models)*2 < len(before.Models) {
				err = fmt.Errorf("OpenAI pricing catalog unexpectedly lost more than half its models; keeping the previous prices")
			}
			if err == nil {
				next.Metadata = OfficialPriceMetadata{ContentHash: hash, Version: before.Metadata.Version + 1, ModelCount: len(next.Models), Usable: true}
			}
		}
	}
	if err == nil {
		err = ctx.Err()
	}
	if err != nil {
		next, changed = before, false
	}
	next.Metadata.SourceURL = OpenAIPricesURL
	next.Metadata.LastAttemptAt = s.Now()
	if err != nil {
		next.Metadata.LastError = err.Error()
	} else {
		next.Metadata.LastError = ""
		next.Metadata.FetchedAt = next.Metadata.LastAttemptAt
	}
	// Reconfiguration must not publish a download into a different database.
	s.mu.Lock()
	if s.repo != repo {
		s.mu.Unlock()
		return OfficialPriceRefreshResult{}, fmt.Errorf("The billing database changed; retry the official price sync")
	}
	saveErr := repo.SaveOfficialPrices(next)
	if saveErr == nil {
		s.officialPrices = next
	}
	s.mu.Unlock()
	if saveErr != nil {
		return OfficialPriceRefreshResult{}, fmt.Errorf("Save official prices: %w", saveErr)
	}
	result := OfficialPriceRefreshResult{Metadata: next.Metadata, Changed: changed}
	if err != nil {
		s.AddPluginLog(PluginLogError, "Failed to sync OpenAI official prices: %v", err)
	} else {
		s.AddPluginLog(PluginLogInfo, "Synced OpenAI official prices for %d models", next.Metadata.ModelCount)
	}
	return result, err
}

// The snapshot owns its slices and maps. Copies returned to callers must not
// let management consumers mutate the active catalog used by billing.
func cloneOfficialModel(model OfficialModelPrice) OfficialModelPrice {
	copy := model
	copy.Quotes = append([]OfficialPriceQuote(nil), model.Quotes...)
	for i := range copy.Quotes {
		copy.Quotes[i].Values = append([]OfficialPriceValue(nil), model.Quotes[i].Values...)
	}
	return copy
}

func (catalog OfficialPriceCatalog) lookup(model string) (OfficialModelPrice, bool) {
	key := ReferenceModelKey(ModelWithoutThinkingSuffix(model))
	for _, price := range catalog.Models {
		if NormalizeModelID(price.ModelID) == key {
			return price, true
		}
	}
	return OfficialModelPrice{}, false
}

func (catalog OfficialPriceCatalog) resolve(upstream, requested string) Price {
	for _, key := range referencePriceKeys(upstream, requested) {
		model, ok := catalog.lookup(key)
		if !ok {
			continue
		}
		rates, ok := model.tokenRates("standard", catalog.ContextThreshold)
		if !ok {
			return Price{Source: PriceSourceNone}
		}
		price := rates.resolve(PriceSourceOfficial)
		model = cloneOfficialModel(model)
		price.officialModel, price.officialThreshold = &model, catalog.ContextThreshold
		return price
	}
	return Price{Source: PriceSourceNone}
}

func (model OfficialModelPrice) tierLimit(tier string, threshold int64, rates PriceRates) int64 {
	if rates.LongContext != nil {
		return 0
	}
	for _, quote := range model.Quotes {
		if quote.Tier == tier && quote.Context == "short" {
			return threshold
		}
	}
	return 0
}

func officialServicePrice(base Price, event UsageEvent, meta PricingMetadata) (Price, PricingMetadata) {
	model := base.officialModel
	if model != nil {
		rates, ok := model.tokenRates(meta.ServiceTier, base.officialThreshold)
		limit := model.tierLimit(meta.ServiceTier, base.officialThreshold, rates)
		if ok && (limit == 0 || event.Breakdown.Input.TotalTokens <= limit) {
			meta.Method, meta.RuleVersion = "official_tier", "openai-official-snapshot"
			return rates.resolve(PriceSourceOfficial), meta
		}
	}
	meta.Method, meta.PriceFallback = "base_fallback", "missing_"+meta.ServiceTier+"_price"
	return base, meta
}

func (catalog OfficialPriceCatalog) Validate() error {
	if !catalog.Metadata.Usable {
		if len(catalog.Models) == 0 {
			return nil
		}
		return fmt.Errorf("Official price catalog is unusable")
	}
	if catalog.Metadata.SourceURL != OpenAIPricesURL || len(catalog.Models) != catalog.Metadata.ModelCount || len(catalog.Models) == 0 || catalog.Metadata.ContentHash == "" {
		return fmt.Errorf("Official price catalog metadata is inconsistent")
	}
	seen := map[string]bool{}
	for _, model := range catalog.Models {
		key := NormalizeModelID(model.ModelID)
		if key == "" || len(key) > 1024 || seen[key] || len(model.Quotes) == 0 {
			return fmt.Errorf("Official price catalog has an invalid model")
		}
		seen[key] = true
		for _, quote := range model.Quotes {
			if strings.TrimSpace(quote.Tier) == "" || len(quote.Values) == 0 {
				return fmt.Errorf("Official price catalog has an invalid quote")
			}
		}
	}
	return nil
}

// Used by persistence and tests to keep a detached snapshot at the boundary.
func DecodeOfficialPriceCatalog(raw []byte) (OfficialPriceCatalog, error) {
	var catalog OfficialPriceCatalog
	if err := json.Unmarshal(raw, &catalog); err != nil {
		return catalog, err
	}
	return catalog, catalog.Validate()
}
