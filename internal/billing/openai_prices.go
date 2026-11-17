package billing

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

const OpenAIPricesURL = "https://developers.openai.com/api/docs/pricing.md"
const maxOfficialPriceResponseBytes = 8 << 20

// Keep the published dimensions and units, including prices the host's usage
// record cannot represent. A dash is unavailable, never a zero-dollar rate.
type OfficialPriceValue struct {
	Metric string `json:"metric"`
	Value  string `json:"value"`
}

type OfficialPriceQuote struct {
	Section  string               `json:"section"`
	Tier     string               `json:"tier"`
	Modality string               `json:"modality,omitempty"`
	Context  string               `json:"context,omitempty"`
	Unit     string               `json:"unit"`
	Values   []OfficialPriceValue `json:"values"`
}

type OfficialModelPrice struct {
	ModelID string               `json:"model_id"`
	Quotes  []OfficialPriceQuote `json:"quotes"`
}

type OfficialPriceMetadata struct {
	SourceURL     string    `json:"source_url"`
	ContentHash   string    `json:"content_hash,omitempty"`
	Version       uint64    `json:"version"`
	FetchedAt     time.Time `json:"fetched_at,omitzero"`
	LastAttemptAt time.Time `json:"last_attempt_at,omitzero"`
	LastError     string    `json:"last_error,omitempty"`
	ModelCount    int       `json:"model_count"`
	Usable        bool      `json:"usable"`
	Refreshing    bool      `json:"refreshing"`
}

type OfficialPriceCatalog struct {
	Metadata         OfficialPriceMetadata `json:"metadata"`
	ContextThreshold int64                 `json:"context_threshold,omitempty"`
	Models           []OfficialModelPrice  `json:"models"`
}

var (
	officialContextThreshold = regexp.MustCompile(`(?i)short context:\s*[≤<]=?\s*([0-9]+(?:\.[0-9]+)?)\s*([km]?)\s*input tokens`)
	officialContextSuffix    = regexp.MustCompile(`(?i)\s*\((?:[<≤>]\s*[0-9.,]+[kKmM]?\s*context length|short context|long context)\)\s*$`)
	officialLink             = regexp.MustCompile(`\[([^\]]+)\]\([^)]*\)`)
	officialHTML             = regexp.MustCompile(`<[^>]+>`)
	officialAmount           = regexp.MustCompile(`^\$([0-9]+(?:\.[0-9]+)?)$`)
)

func downloadOpenAIPrices(ctx context.Context) ([]byte, error) {
	transport := &http.Transport{Proxy: http.ProxyFromEnvironment, DisableKeepAlives: true, TLSHandshakeTimeout: 10 * time.Second}
	defer transport.CloseIdleConnections()
	return fetchOpenAIPrices(ctx, &http.Client{Timeout: referencePriceHTTPTimeout, Transport: transport})
}

func fetchOpenAIPrices(ctx context.Context, client *http.Client) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, OpenAIPricesURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "text/markdown")
	req.Header.Set("User-Agent", "cpa-key-billing/official-pricing")
	response, err := client.Do(req)
	if err != nil {
		var requestError *url.Error
		for errors.As(err, &requestError) {
			err = requestError.Err
		}
		return nil, fmt.Errorf("Download OpenAI official prices: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("OpenAI official pricing returned HTTP %d", response.StatusCode)
	}
	raw, err := io.ReadAll(io.LimitReader(response.Body, maxOfficialPriceResponseBytes+1))
	if err != nil {
		return nil, fmt.Errorf("Read OpenAI official prices: %w", err)
	}
	if len(raw) > maxOfficialPriceResponseBytes {
		return nil, fmt.Errorf("OpenAI official pricing response is too large")
	}
	return raw, nil
}

func officialText(value string) string {
	value = officialContextSuffix.ReplaceAllString(value, "")
	value = officialHTML.ReplaceAllString(value, "")
	value = officialLink.ReplaceAllString(value, "$1")
	return strings.Trim(strings.TrimSpace(value), "` ")
}

func officialCells(line string) []string {
	parts := strings.Split(strings.Trim(strings.TrimSpace(line), "|"), "|")
	for i := range parts {
		parts[i] = strings.TrimSpace(parts[i])
	}
	return parts
}

// Parse the public Markdown tables rather than enumerating model IDs or
// synthesizing prices. A structural change fails the sync instead of replacing
// a working catalog with an HTML challenge or a partly parsed price list.
func parseOpenAIPrices(raw []byte) (OfficialPriceCatalog, error) {
	catalog := OfficialPriceCatalog{Models: []OfficialModelPrice{}}
	text := strings.ReplaceAll(string(raw), "\r\n", "\n")
	if !strings.HasPrefix(strings.TrimSpace(text), "# Pricing") {
		return catalog, fmt.Errorf("OpenAI pricing is not the expected Markdown document")
	}
	if match := officialContextThreshold.FindStringSubmatch(text); match != nil {
		amount, _ := strconv.ParseFloat(match[1], 64)
		switch strings.ToLower(match[2]) {
		case "k":
			amount *= 1000
		case "m":
			amount *= 1_000_000
		}
		if amount > 0 && amount < 1e12 && amount == float64(int64(amount)) {
			catalog.ContextThreshold = int64(amount)
		}
	}
	models := map[string]*OfficialModelPrice{}
	section, tier := "", "standard"
	lines := strings.Split(text, "\n")
	for i := 0; i < len(lines); i++ {
		line := strings.TrimSpace(lines[i])
		heading := strings.TrimSpace(strings.TrimLeft(line, "#"))
		switch strings.ToLower(heading) {
		case "flagship models", "text tokens", "images", "audio", "gpt-live", "realtime & audio", "transcription", "specialized models", "tools":
			section, tier = heading, "standard"
		case "standard", "standard pricing data":
			tier = "standard"
		case "batch", "batch pricing data":
			tier = "batch"
		case "flex", "flex pricing data":
			tier = "flex"
		case "priority", "priority pricing data", "fast", "fast pricing data":
			tier = "priority"
		case "ultrafast", "ultrafast pricing data":
			tier = "ultrafast"
		}
		if !strings.HasPrefix(line, "|") || i+1 >= len(lines) || !strings.HasPrefix(strings.TrimSpace(lines[i+1]), "|") {
			continue
		}
		headers := officialCells(line)
		contextTable, shortInput, longInput := false, false, false
		for _, header := range headers {
			metric := strings.ToLower(header)
			if strings.HasPrefix(metric, "short context ") || strings.HasPrefix(metric, "long context ") {
				contextTable = true
			}
			shortInput = shortInput || metric == "short context input"
			longInput = longInput || metric == "long context input"
		}
		if contextTable && (!shortInput || !longInput) {
			return catalog, fmt.Errorf("OpenAI pricing context table has unsupported columns")
		}
		modelColumn := -1
		for j, header := range headers {
			if strings.EqualFold(header, "Model") {
				modelColumn = j
			}
		}
		if modelColumn < 0 {
			continue
		}
		separator := officialCells(lines[i+1])
		if len(separator) != len(headers) {
			return catalog, fmt.Errorf("OpenAI pricing table has an invalid header")
		}
		for _, cell := range separator {
			if strings.Trim(cell, "-: ") != "" || !strings.Contains(cell, "-") {
				return catalog, fmt.Errorf("OpenAI pricing table has an invalid separator")
			}
		}
		i += 2
		rowCount := 0
		for ; i < len(lines) && strings.HasPrefix(strings.TrimSpace(lines[i]), "|"); i++ {
			cells := officialCells(lines[i])
			if len(cells) != len(headers) {
				return catalog, fmt.Errorf("OpenAI pricing table has an incomplete row")
			}
			name := officialText(cells[modelColumn])
			if name == "" || len(name) > 1024 {
				return catalog, fmt.Errorf("OpenAI pricing table has an invalid model ID")
			}
			key := NormalizeModelID(name)
			model := models[key]
			if model == nil {
				model = &OfficialModelPrice{ModelID: name}
				models[key] = model
			}
			modality, category := "", section
			for j, header := range headers {
				switch strings.ToLower(header) {
				case "modality":
					modality = officialText(cells[j])
				case "category", "use case":
					category = officialText(cells[j])
				}
			}
			contexts := []string{""}
			for _, header := range headers {
				if strings.HasPrefix(strings.ToLower(header), "short context ") {
					contexts = []string{"short", "long"}
					break
				}
			}
			if strings.Contains(strings.ToLower(cells[modelColumn]), "(short context)") {
				contexts = []string{"short"}
			}
			if strings.Contains(strings.ToLower(cells[modelColumn]), "(long context)") {
				contexts = []string{"long"}
			}
			for _, contextName := range contexts {
				quote := OfficialPriceQuote{Section: category, Tier: tier, Modality: modality, Context: contextName, Unit: "USD / 1M tokens"}
				for j, header := range headers {
					metric := strings.ToLower(header)
					if j == modelColumn || metric == "category" || metric == "modality" || metric == "use case" {
						continue
					}
					if strings.HasPrefix(metric, "short context ") || strings.HasPrefix(metric, "long context ") {
						if !strings.HasPrefix(metric, contextName+" context ") {
							continue
						}
						header = header[len(contextName)+len(" context "):]
					}
					value := officialText(cells[j])
					if value == "" {
						return catalog, fmt.Errorf("OpenAI pricing table has an empty price cell")
					}
					if strings.Contains(metric, "minute") || metric == "estimated cost" || strings.Contains(value, " / ") {
						quote.Unit = "As quoted"
					}
					quote.Values = append(quote.Values, OfficialPriceValue{Metric: header, Value: value})
				}
				if len(quote.Values) == 0 {
					return catalog, fmt.Errorf("OpenAI pricing table has no prices")
				}
				model.Quotes = append(model.Quotes, quote)
			}
			rowCount++
		}
		if rowCount == 0 {
			return catalog, fmt.Errorf("OpenAI pricing table is empty")
		}
		i--
	}
	priced := false
	for _, model := range models {
		catalog.Models = append(catalog.Models, *model)
		for _, quote := range model.Quotes {
			if quote.Context != "" && catalog.ContextThreshold <= 0 {
				return catalog, fmt.Errorf("OpenAI pricing context threshold is missing or unsupported")
			}
			for _, value := range quote.Values {
				if _, ok := officialNumericPrice(value.Value); ok {
					priced = true
				}
			}
		}
	}
	if len(catalog.Models) == 0 || !priced {
		return catalog, fmt.Errorf("OpenAI pricing document contains no model prices")
	}
	sort.Slice(catalog.Models, func(i, j int) bool { return catalog.Models[i].ModelID < catalog.Models[j].ModelID })
	return catalog, nil
}

func officialNumericPrice(value string) (float64, bool) {
	if strings.EqualFold(value, "Free") {
		return 0, true
	}
	match := officialAmount.FindStringSubmatch(value)
	if match == nil {
		return 0, false
	}
	amount, err := strconv.ParseFloat(match[1], 64)
	return amount, err == nil && !invalidPrice(amount)
}

// Image models follow the plugin's existing text-input/image-output convention.
// The original modality cards remain available; this is not exact multimodal
// billing when the host reports only aggregate usage. Audio and per-minute
// prices are display-only. TTL-specific cache writes are not guessed.
func (model OfficialModelPrice) tokenRates(tier string, threshold int64) (PriceRates, bool) {
	var short, long *PriceRates
	image := false
	for _, quote := range model.Quotes {
		if strings.EqualFold(quote.Section, "Images") {
			image = true
		}
		if quote.Modality != "" && !image {
			return PriceRates{}, false
		}
	}
	if image {
		rates := PriceRates{}
		input, output, cache := false, false, false
		for _, quote := range model.Quotes {
			if quote.Tier != tier || quote.Unit != "USD / 1M tokens" {
				continue
			}
			for _, entry := range quote.Values {
				amount, ok := officialNumericPrice(entry.Value)
				if !ok {
					continue
				}
				switch {
				case strings.EqualFold(quote.Modality, "Text") && strings.EqualFold(entry.Metric, "Input"):
					rates.InputPer1M, input = amount, true
				case strings.EqualFold(quote.Modality, "Text") && strings.EqualFold(entry.Metric, "Cached input"):
					rates.CacheReadPer1M, cache = &amount, true
				case strings.EqualFold(quote.Modality, "Image") && strings.EqualFold(entry.Metric, "Output"):
					rates.OutputPer1M, output = amount, true
				}
			}
		}
		return rates, input && output && cache
	}
	for _, quote := range model.Quotes {
		if quote.Modality != "" || quote.Unit != "USD / 1M tokens" {
			return PriceRates{}, false
		}
		if quote.Tier != tier {
			continue
		}
		rates := PriceRates{}
		input, output, cache := false, false, false
		for _, entry := range quote.Values {
			amount, ok := officialNumericPrice(entry.Value)
			if !ok {
				continue
			}
			switch strings.ToLower(entry.Metric) {
			case "input":
				rates.InputPer1M, input = amount, true
			case "output":
				rates.OutputPer1M, output = amount, true
			case "cached input":
				rates.CacheReadPer1M, cache = &amount, true
			}
		}
		if !input || !output || !cache {
			continue
		}
		if quote.Context == "long" {
			long = &rates
		} else {
			if short != nil {
				return PriceRates{}, false
			}
			short = &rates
		}
	}
	if short == nil {
		return PriceRates{}, false
	}
	if long != nil {
		if threshold <= 0 {
			return PriceRates{}, false
		}
		short.LongContext = &LongContextPrice{ThresholdInputTokens: threshold, InputPer1M: long.InputPer1M, OutputPer1M: long.OutputPer1M, CacheReadPer1M: long.CacheReadPer1M}
	}
	return *short, true
}
