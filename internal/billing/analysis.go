package billing

import "time"

type AnalysisTrendPoint struct {
	Time  time.Time `json:"time"`
	Value float64   `json:"value"`
}

type AnalysisTrends struct {
	Requests            []AnalysisTrendPoint `json:"requests"`
	TotalTokens         []AnalysisTrendPoint `json:"total_tokens"`
	UncachedInputTokens []AnalysisTrendPoint `json:"input_tokens"`
	OutputTokens        []AnalysisTrendPoint `json:"output_tokens"`
	CacheReadTokens     []AnalysisTrendPoint `json:"cache_read_tokens"`
	CacheWriteTokens    []AnalysisTrendPoint `json:"cache_write_tokens"`
	CacheRate           []AnalysisTrendPoint `json:"cache_rate"`
	TotalCost           []AnalysisTrendPoint `json:"total_cost"`
}

type AnalysisComposition struct {
	Key         string  `json:"key"`
	Label       string  `json:"label"`
	Preview     string  `json:"preview,omitempty"`
	TotalTokens int64   `json:"total_tokens"`
	Requests    int64   `json:"requests"`
	CostUSD     float64 `json:"cost_usd"`
	Percent     float64 `json:"percent"`
}

type UsageDistribution struct {
	APIKeys []AnalysisComposition `json:"api_keys"`
	Models  []AnalysisComposition `json:"models"`
	Sources []AnalysisComposition `json:"sources"`
}

type AnalysisCostSummary struct {
	TotalUSD      float64 `json:"total_usd"`
	InputUSD      float64 `json:"input_usd"`
	CacheReadUSD  float64 `json:"cache_read_usd"`
	CacheWriteUSD float64 `json:"cache_write_usd"`
	OutputUSD     float64 `json:"output_usd"`
}

type AnalysisSummary struct {
	Requests         int64               `json:"requests"`
	Succeeded        int64               `json:"succeeded"`
	Failed           int64               `json:"failed"`
	SuccessRate      float64             `json:"success_rate"`
	TotalTokens      int64               `json:"total_tokens"`
	InputTokens      int64               `json:"input_tokens"`
	OutputTokens     int64               `json:"output_tokens"`
	CacheReadTokens  int64               `json:"cache_read_tokens"`
	CacheWriteTokens int64               `json:"cache_write_tokens"`
	CacheRate        float64             `json:"cache_rate"`
	Cost             AnalysisCostSummary `json:"cost"`
}

type AnalysisModelGroup struct {
	RequestedModel  string  `json:"requested_model"`
	ReportedModel   string  `json:"reported_model"`
	Key             string  `json:"key"`
	Label           string  `json:"label"`
	Preview         string  `json:"preview,omitempty"`
	Requests        int64   `json:"requests"`
	TotalTokens     int64   `json:"total_tokens"`
	CostUSD         float64 `json:"cost_usd"`
	BeforeGlobalUSD float64 `json:"before_global_usd"`
	Unconvertible   int64   `json:"unconvertible"`
	MissingUsage    int64   `json:"missing_usage"`
}

type AnalysisKeyTrend struct {
	Key     string               `json:"key"`
	Label   string               `json:"label"`
	Preview string               `json:"preview,omitempty"`
	Points  []AnalysisTrendPoint `json:"points"`
}

type AnalysisView struct {
	SnapshotID   int64                `json:"snapshot_id,string"`
	Granularity  string               `json:"granularity"`
	From         time.Time            `json:"from"`
	To           time.Time            `json:"to"`
	ModelGroups  []AnalysisModelGroup `json:"model_groups,omitempty"`
	KeyTrends    []AnalysisKeyTrend   `json:"key_trends,omitempty"`
	MissingUsage int64                `json:"missing_usage"`

	Summary           AnalysisSummary   `json:"summary"`
	Trends            AnalysisTrends    `json:"trends"`
	UsageDistribution UsageDistribution `json:"usage_distribution"`
}

func (s *Store) Analysis(query RequestEventQuery) (AnalysisView, error) {
	now := s.Now()
	from, to := effectiveAnalysisRange(query, now)
	if !from.Before(to) {
		return AnalysisView{}, invalidf("The analysis range is outside the retained request history")
	}
	query.From, query.To = from, to
	switch query.Granularity {
	case "", "auto", "hour", "day":
	default:
		return AnalysisView{}, invalidf("Granularity must be auto, hour or day")
	}
	if query.Granularity == "hour" && AnalysisHourlyBuckets(query) > 1000 {
		return AnalysisView{}, invalidf("At most 1000 time buckets are supported; select daily granularity")
	}
	view, err := withRepository(s, func(repo Repository) (AnalysisView, error) {
		return repo.Analysis(query, now.Add(-RequestEventRetention))
	})
	if view.UsageDistribution.APIKeys == nil {
		view.UsageDistribution.APIKeys = []AnalysisComposition{}
	}
	if view.UsageDistribution.Models == nil {
		view.UsageDistribution.Models = []AnalysisComposition{}
	}
	if view.UsageDistribution.Sources == nil {
		view.UsageDistribution.Sources = []AnalysisComposition{}
	}
	return view, err
}

func effectiveAnalysisRange(query RequestEventQuery, now time.Time) (time.Time, time.Time) {
	to := query.To
	if to.IsZero() || to.After(now) {
		to = now
	}
	from := query.From
	if from.IsZero() {
		from = to.Add(-30 * 24 * time.Hour)
	}
	cutoff := now.Add(-RequestEventRetention)
	if from.Before(cutoff) {
		from = cutoff
	}
	return from, to
}

// AnalysisHourlyBuckets includes the partial first hour in the selected timezone.
func AnalysisHourlyBuckets(query RequestEventQuery) int {
	location := query.Timezone
	if location == nil {
		location = time.UTC
	}
	local := query.From.In(location)
	start := local.Add(-time.Duration(local.Minute())*time.Minute - time.Duration(local.Second())*time.Second - time.Duration(local.Nanosecond()))
	duration := query.To.Sub(start)
	if duration <= 0 {
		return 0
	}
	return int((duration + time.Hour - 1) / time.Hour)
}
