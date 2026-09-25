package billing

import (
	"bytes"
	"encoding/json"
	"fmt"
	"maps"
	"strings"
)

// TierPriceRates is a complete price card. Pointers distinguish required zero
// input/output prices from omitted fields in management requests.
type TierPriceRates struct {
	InputPer1M      *float64          `json:"input_per_1m"`
	OutputPer1M     *float64          `json:"output_per_1m"`
	CacheReadPer1M  *float64          `json:"cache_read_per_1m,omitempty"`
	CacheWritePer1M *float64          `json:"cache_write_per_1m,omitempty"`
	LongContext     *LongContextPrice `json:"long_context,omitempty"`
}

// Validate JSON-required long-context fields as well as top-level pointer fields.
func (r *TierPriceRates) UnmarshalJSON(data []byte) error {
	type plain TierPriceRates
	var value plain
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&value); err != nil {
		return err
	}
	if value.LongContext != nil {
		var fields struct {
			LongContext map[string]json.RawMessage `json:"long_context"`
		}
		if err := json.Unmarshal(data, &fields); err != nil {
			return err
		}
		for _, name := range []string{"input_per_1m", "output_per_1m"} {
			raw, ok := fields.LongContext[name]
			if !ok || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
				return fmt.Errorf("Service-tier long context: %s is required", name)
			}
		}
	}
	*r = TierPriceRates(value)
	return nil
}

func (r TierPriceRates) rates() PriceRates {
	var input, output float64
	if r.InputPer1M != nil {
		input = *r.InputPer1M
	}
	if r.OutputPer1M != nil {
		output = *r.OutputPer1M
	}
	return PriceRates{InputPer1M: input, OutputPer1M: output, CacheReadPer1M: r.CacheReadPer1M, CacheWritePer1M: r.CacheWritePer1M, LongContext: r.LongContext}
}

// PricingMetadata describes the pricing decision made at ingestion. Empty
// metadata denotes a historical or unsupported-provider record; never reconstruct it.
// Keep this value comparable, like Cost, for historical-value checks.
type PricingMetadata struct {
	ServiceTier   string `json:"service_tier"`
	TierSource    string `json:"tier_source"`
	Method        string `json:"method"`
	RuleVersion   string `json:"rule_version,omitempty"`
	TierFallback  string `json:"tier_fallback,omitempty"`
	PriceFallback string `json:"price_fallback,omitempty"`
}

func apiTierEligible(event UsageEvent) bool {
	if !strings.EqualFold(strings.TrimSpace(event.AuthType), "apikey") {
		return false
	}
	provider := strings.ToLower(strings.TrimSpace(event.Provider))
	return provider == "openai" || provider == "codex" || strings.HasPrefix(provider, "openai-compatible-") || event.ExecutorType == "OpenAICompatExecutor"
}

func normalizedServiceTier(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "priority", "fast":
		return "priority"
	case "flex":
		return "flex"
	case "default", "standard":
		return "default"
	default:
		return ""
	}
}

func effectiveServiceTier(event UsageEvent) PricingMetadata {
	meta := PricingMetadata{TierSource: "response", Method: "base"}
	tier := strings.TrimSpace(event.ResponseServiceTier)
	if tier == "" {
		meta.TierSource, meta.TierFallback = "request", "missing_response_tier"
		tier = strings.TrimSpace(event.ServiceTier)
		if tier == "" || strings.EqualFold(tier, "auto") {
			tier = "default"
			meta.TierSource = "default"
		}
	}
	meta.ServiceTier = normalizedServiceTier(tier)
	// CPA's Codex request converter removes Flex, for both OAuth and API keys.
	// The original request tier is still reported in usage.handle, so it cannot
	// establish a Flex discount without an explicit upstream response tier.
	if meta.ServiceTier == "flex" && meta.TierSource == "request" &&
		(strings.EqualFold(strings.TrimSpace(event.Provider), "codex") ||
			event.ExecutorType == "CodexExecutor" || event.ExecutorType == "CodexWebsocketsExecutor") {
		meta.ServiceTier, meta.TierSource, meta.TierFallback = "default", "default", "unconfirmed_flex_tier"
	}
	if meta.ServiceTier == "" {
		meta.ServiceTier = "default"
		if meta.TierSource == "response" {
			meta.TierFallback = "unknown_response_tier"
		} else {
			meta.TierFallback = "unknown_request_tier"
		}
	}
	return meta
}

const codexOAuthRuleVersion = "codex-oauth-family-v7"

func effectiveOAuthServiceTier(event UsageEvent, billingModel string) PricingMetadata {
	// Codex OAuth's terminal tier does not reliably reflect Fast routing:
	// https://github.com/openai/codex/issues/14204#issuecomment-4033184620
	// Bill explicit Fast requests for these families under the operator's 2x
	// policy. Keep the raw response tier on the event for independent inspection.
	if normalizedServiceTier(event.ServiceTier) == "priority" &&
		codexPriorityMultiplier(event, billingModel) == 2 {
		// Only these explicit response values confirm a cheaper tier under the
		// operator's policy. Keep default distinct from standard: Codex can echo
		// default even for Fast turns. Auto/unknown/missing retain the request.
		switch strings.ToLower(strings.TrimSpace(event.ResponseServiceTier)) {
		case "standard", "flex":
			return effectiveServiceTier(event)
		}
		return PricingMetadata{ServiceTier: "priority", TierSource: "request_policy", Method: "base"}
	}
	return effectiveServiceTier(event)
}

// This is the operator's downstream OAuth billing policy, separate from API
// price cards. Use the host's model identity before a configured pricing alias.
func codexPriorityMultiplier(event UsageEvent, billingModel string) float64 {
	model := strings.TrimSpace(event.ResponseModel)
	if model == "" {
		model = strings.TrimSpace(event.UpstreamModel)
	}
	if model == "" {
		model = billingModel
	}
	model = NormalizeModelID(ModelWithoutThinkingSuffix(model))
	if slash := strings.LastIndexByte(model, '/'); slash >= 0 {
		model = model[slash+1:]
	}
	if model == "gpt-5.6" || strings.HasPrefix(model, "gpt-5.6-") ||
		model == "gpt-6" || strings.HasPrefix(model, "gpt-6-") {
		return 2
	}
	return CodexFastModeMultiplier
}

func resolveAPIServicePrice(base Price, model string, event UsageEvent) (Price, PricingMetadata) {
	meta := effectiveServiceTier(event)
	if base.Source == PriceSourceNone {
		meta.Method, meta.PriceFallback = "base_fallback", "missing_base_price"
		return base, meta
	}
	if meta.ServiceTier == "default" {
		return base, meta
	}
	if explicit, ok := base.ServiceTiers[meta.ServiceTier]; ok {
		meta.Method = "explicit_tier"
		return explicit.rates().resolve(base.Source), meta
	}
	if meta.ServiceTier == "flex" {
		meta.Method, meta.RuleVersion = "model_ratio", "flex-0.5-v1"
		return scaleServicePrice(base, tokenRateRatio{0.5, 0.5, 0.5, 0.5}), meta
	}
	if rule, ok := openAIPriorityRule(model); ok &&
		(rule.MaxInputTokens == 0 || event.Breakdown.Input.TotalTokens <= rule.MaxInputTokens) &&
		(event.Breakdown.Input.CacheReadTokens == 0 || rule.Ratio.CacheRead > 0) &&
		(event.Breakdown.Input.CacheWriteTokens == 0 || rule.Ratio.CacheWrite > 0) {
		meta.Method, meta.RuleVersion = "model_ratio", openAIServicePriceVersion
		return scaleServicePrice(base, rule.Ratio), meta
	}
	meta.Method, meta.PriceFallback = "base_fallback", "missing_priority_price"
	return base, meta
}

type tokenRateRatio struct{ Input, Output, CacheRead, CacheWrite float64 }
type servicePriceRule struct {
	Ratio          tokenRateRatio
	MaxInputTokens int64
}

// Source: https://developers.openai.com/api/docs/pricing (including All models),
// checked 2026-09-25. GPT-5.6 Sol long-context Fast rates are also explicit at
// https://developers.openai.com/api/docs/guides/fast-mode . Rates are ratios to
// the selected base price, preserving operator custom prices. Short-context-only
// cards and unavailable cache-write prices must not extrapolate unknown rates.
const openAIServicePriceVersion = "openai-2026-09-25"

var openAIPriorityRules = map[string]servicePriceRule{
	"gpt-6-astra":       {tokenRateRatio{2, 2, 2, 2}, 0},
	"gpt-6-sol":         {tokenRateRatio{2, 2, 2, 2}, 0},
	"gpt-6-luna":        {tokenRateRatio{2, 2, 2, 2}, 0},
	"gpt-5.6-sol":       {tokenRateRatio{2, 2, 2, 2}, 0},
	"gpt-5.6-terra":     {tokenRateRatio{2, 2, 2, 2}, 272000},
	"gpt-5.6-luna":      {tokenRateRatio{2, 2, 2, 2}, 272000},
	"gpt-5.5":           {tokenRateRatio{2.5, 2.5, 2.5, 0}, 272000},
	"gpt-5.4":           {tokenRateRatio{2, 2, 2, 0}, 272000},
	"gpt-5.4-mini":      {tokenRateRatio{2, 2, 2, 0}, 0},
	"gpt-5.2":           {tokenRateRatio{2, 2, 2, 0}, 0},
	"gpt-5.1":           {tokenRateRatio{2, 2, 2, 0}, 0},
	"gpt-5":             {tokenRateRatio{2, 2, 2, 0}, 0},
	"gpt-5-mini":        {tokenRateRatio{1.8, 1.8, 1.8, 0}, 0},
	"gpt-4.1":           {tokenRateRatio{1.75, 1.75, 1.75, 0}, 0},
	"gpt-4.1-mini":      {tokenRateRatio{1.75, 1.75, 1.75, 0}, 0},
	"gpt-4.1-nano":      {tokenRateRatio{2, 2, 2, 0}, 0},
	"gpt-4o":            {tokenRateRatio{1.7, 1.7, 1.7, 0}, 0},
	"gpt-4o-2024-05-13": {tokenRateRatio{1.75, 1.75, 0, 0}, 0},
	"gpt-4o-mini":       {tokenRateRatio{5.0 / 3, 5.0 / 3, 5.0 / 3, 0}, 0},
	"o3":                {tokenRateRatio{1.75, 1.75, 1.75, 0}, 0},
	"o4-mini":           {tokenRateRatio{20.0 / 11, 20.0 / 11, 20.0 / 11, 0}, 0},
}

func openAIPriorityRule(model string) (servicePriceRule, bool) {
	key := NormalizeModelID(ModelWithoutThinkingSuffix(model))
	if slash := strings.LastIndexByte(key, '/'); slash >= 0 {
		key = key[slash+1:]
	}
	switch key {
	case "gpt-5.6":
		key = "gpt-5.6-sol"
	case "gpt-5.5-2026-04-23":
		key = "gpt-5.5"
	}
	rule, ok := openAIPriorityRules[key]
	return rule, ok
}

func scaleServicePrice(base Price, ratio tokenRateRatio) Price {
	// A missing, unused cache price is retained rather than advertised as free.
	if ratio.CacheRead == 0 {
		ratio.CacheRead = 1
	}
	if ratio.CacheWrite == 0 {
		ratio.CacheWrite = 1
	}
	base.InputPer1M *= ratio.Input
	base.OutputPer1M *= ratio.Output
	base.CacheReadPer1M *= ratio.CacheRead
	base.CacheWritePer1M *= ratio.CacheWrite
	if base.LongContext != nil {
		tier := *base.LongContext
		tier.InputPer1M *= ratio.Input
		tier.OutputPer1M *= ratio.Output
		tier.CacheReadPer1M *= ratio.CacheRead
		tier.CacheWritePer1M *= ratio.CacheWrite
		base.LongContext = &tier
	}
	return base
}

func cloneServiceTiers(tiers map[string]TierPriceRates) map[string]TierPriceRates {
	if tiers == nil {
		return nil
	}
	cloned := maps.Clone(tiers)
	for key, tier := range cloned {
		copyNumber := func(n *float64) *float64 {
			if n == nil {
				return nil
			}
			v := *n
			return &v
		}
		tier.InputPer1M, tier.OutputPer1M = copyNumber(tier.InputPer1M), copyNumber(tier.OutputPer1M)
		tier.CacheReadPer1M, tier.CacheWritePer1M = copyNumber(tier.CacheReadPer1M), copyNumber(tier.CacheWritePer1M)
		if tier.LongContext != nil {
			lc := *tier.LongContext
			lc.CacheReadPer1M, lc.CacheWritePer1M = copyNumber(lc.CacheReadPer1M), copyNumber(lc.CacheWritePer1M)
			tier.LongContext = &lc
		}
		cloned[key] = tier
	}
	return cloned
}

// ServiceTierPriceView makes effective short-context prices visible without
// changing the optional, operator-authored service_tiers configuration.
type ServiceTierPriceView struct {
	Price           Price    `json:"price"`
	Method          string   `json:"method"`
	RuleVersion     string   `json:"rule_version,omitempty"`
	PriceFallback   string   `json:"price_fallback,omitempty"`
	MaxInputTokens  int64    `json:"max_input_tokens,omitempty"`
	UnverifiedCache []string `json:"unverified_cache,omitempty"`
}

func serviceTierPriceViews(model string, rates PriceRates, source PriceSource) map[string]ServiceTierPriceView {
	views := make(map[string]ServiceTierPriceView, 2)
	for _, tier := range []string{"priority", "flex"} {
		price, meta := resolveAPIServicePrice(rates.resolve(source), model, UsageEvent{ResponseServiceTier: tier})
		price.ServiceTiers = nil
		view := ServiceTierPriceView{Price: price, Method: meta.Method, RuleVersion: meta.RuleVersion, PriceFallback: meta.PriceFallback}
		if meta.Method == "model_ratio" && tier == "priority" {
			rule, _ := openAIPriorityRule(model)
			view.MaxInputTokens = rule.MaxInputTokens
			if rule.Ratio.CacheRead == 0 {
				view.UnverifiedCache = append(view.UnverifiedCache, "cache_read")
			}
			if rule.Ratio.CacheWrite == 0 {
				view.UnverifiedCache = append(view.UnverifiedCache, "cache_write")
			}
			if rule.MaxInputTokens > 0 {
				view.Price.LongContext = nil
			}
		}
		views[tier] = view
	}
	return views
}
