package sqlite

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"

	"cpa-key-billing/internal/billing"
)

func (d *DB) Analysis(query billing.RequestEventQuery, since time.Time) (billing.AnalysisView, error) {
	view := billing.AnalysisView{
		UsageDistribution: billing.UsageDistribution{
			APIKeys: []billing.AnalysisComposition{},
			Models:  []billing.AnalysisComposition{},
			Sources: []billing.AnalysisComposition{},
		},
	}
	if query.Granularity != "" && query.Granularity != "auto" && query.Granularity != "hour" && query.Granularity != "day" {
		return view, fmt.Errorf("Invalid analysis granularity")
	}
	if query.Granularity == "hour" && billing.AnalysisHourlyBuckets(query) > 1000 {
		return view, fmt.Errorf("At most 1000 analysis buckets are supported")
	}
	tx, err := d.db.Begin()
	if err != nil {
		return view, err
	}
	defer tx.Rollback()
	if err = tx.QueryRow("SELECT coalesce(max(id),0) FROM request_events").Scan(&view.SnapshotID); err != nil {
		return view, err
	}
	query.SnapshotID = &view.SnapshotID
	view.From, view.To = query.From, query.To
	view.Granularity = "hour"
	if query.Granularity == "day" || ((query.Granularity == "" || query.Granularity == "auto") && query.To.Sub(query.From) > 24*time.Hour) {
		view.Granularity = "day"
	}
	view.Trends = analysisTrendPoints(query)
	boundaries := view.Trends.Requests
	if len(boundaries) == 0 {
		return view, nil
	}
	keys, models, sources := analysisGroups{}, analysisGroups{}, analysisGroups{}
	includeKeys := strings.TrimSpace(query.Scope) == "" && strings.TrimSpace(query.KeyScope) == ""
	summary, trends := &view.Summary, &view.Trends
	modelGroups := map[string]*billing.AnalysisModelGroup{}
	keyPoints := map[string][]billing.AnalysisTrendPoint{}
	// SQLite limits compound SELECT terms. All batches share this transaction.
	for base := 0; base < len(boundaries); base += 120 {
		stop := min(base+120, len(boundaries))
		chunk := query
		if stop < len(boundaries) {
			chunk.To = boundaries[stop].Time
		}
		statement, args := analysisSQL(chunk, since, boundaries[base:stop])
		rows, err := tx.Query(statement, args...)
		if err != nil {
			return view, fmt.Errorf("Aggregate analysis data: %w", err)
		}
		for rows.Next() {
			var index int
			var scope, model, source string
			var part billing.AnalysisSummary
			var requested, reported, billingModel string
			var before float64
			var missing, unconvertible int64
			if err := rows.Scan(&index, &scope, &model, &source,
				&part.Requests, &part.Failed, &part.InputTokens, &part.OutputTokens,
				&part.CacheReadTokens, &part.CacheWriteTokens, &part.Cost.TotalUSD,
				&part.Cost.InputUSD, &part.Cost.CacheReadUSD, &part.Cost.CacheWriteUSD, &part.Cost.OutputUSD, &requested, &reported, &before, &unconvertible, &missing, &billingModel); err != nil {
				rows.Close()
				return billing.AnalysisView{}, fmt.Errorf("Read analysis data: %w", err)
			}
			index += base
			trends.Requests[index].Value += float64(part.Requests)
			trends.UncachedInputTokens[index].Value += float64(part.InputTokens - part.CacheReadTokens - part.CacheWriteTokens)
			trends.OutputTokens[index].Value += float64(part.OutputTokens)
			trends.CacheReadTokens[index].Value += float64(part.CacheReadTokens)
			trends.CacheWriteTokens[index].Value += float64(part.CacheWriteTokens)
			trends.TotalCost[index].Value += part.Cost.TotalUSD
			part.TotalTokens = part.InputTokens + part.OutputTokens
			trends.TotalTokens[index].Value += float64(part.TotalTokens)
			summary.Requests += part.Requests
			summary.Failed += part.Failed
			summary.InputTokens += part.InputTokens
			summary.OutputTokens += part.OutputTokens
			summary.TotalTokens += part.TotalTokens
			summary.CacheReadTokens += part.CacheReadTokens
			summary.CacheWriteTokens += part.CacheWriteTokens
			summary.Cost.TotalUSD += part.Cost.TotalUSD
			summary.Cost.InputUSD += part.Cost.InputUSD
			summary.Cost.CacheReadUSD += part.Cost.CacheReadUSD
			summary.Cost.CacheWriteUSD += part.Cost.CacheWriteUSD
			summary.Cost.OutputUSD += part.Cost.OutputUSD
			if includeKeys {
				keys.add(scope, part)
			}
			if includeKeys {
				if keyPoints[scope] == nil {
					keyPoints[scope] = slices.Clone(boundaries)
					for i := range keyPoints[scope] {
						keyPoints[scope][i].Value = 0
					}
				}
				keyPoints[scope][index].Value += float64(part.TotalTokens)
				encoded, _ := json.Marshal([]string{billingModel, requested, reported, scope})
				id := string(encoded)
				group := modelGroups[id]
				if group == nil {
					group = &billing.AnalysisModelGroup{BillingModel: billingModel, RequestedModel: requested, ReportedModel: reported, Key: scope}
					modelGroups[id] = group
				}
				group.Requests += part.Requests
				group.TotalTokens += part.TotalTokens
				group.CostUSD += part.Cost.TotalUSD
				group.BeforeGlobalUSD += before
				group.Unconvertible += unconvertible
				group.MissingUsage += missing
			}
			view.MissingUsage += missing
			models.add(model, part)
			sources.add(source, part)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return billing.AnalysisView{}, fmt.Errorf("Read analysis data: %w", err)
		}
		if err := rows.Close(); err != nil {
			return billing.AnalysisView{}, fmt.Errorf("Read analysis data: %w", err)
		}
	}
	summary.Succeeded = summary.Requests - summary.Failed
	if summary.Requests > 0 {
		summary.SuccessRate = float64(summary.Succeeded) * 100 / float64(summary.Requests)
	}
	if summary.InputTokens > 0 {
		summary.CacheRate = float64(summary.CacheReadTokens) * 100 / float64(summary.InputTokens)
	}
	for index := range boundaries {
		input := trends.UncachedInputTokens[index].Value + trends.CacheReadTokens[index].Value + trends.CacheWriteTokens[index].Value
		if input > 0 {
			trends.CacheRate[index].Value = trends.CacheReadTokens[index].Value * 100 / input
		}
	}
	if err := labelAnalysisKeys(tx, keys); err != nil {
		return billing.AnalysisView{}, err
	}
	view.UsageDistribution.APIKeys = keys.finish()
	view.UsageDistribution.Models = models.finish()
	view.UsageDistribution.Sources = sources.finish()
	for i := range view.UsageDistribution.Models {
		if view.UsageDistribution.Models[i].Key == "" {
			view.UsageDistribution.Models[i].Label = "Unknown model"
		}
	}
	for i := range view.UsageDistribution.Sources {
		if view.UsageDistribution.Sources[i].Key == "" {
			view.UsageDistribution.Sources[i].Label = "Unknown source"
		}
	}
	if includeKeys {
		for _, group := range modelGroups {
			if key := keys[group.Key]; key != nil {
				group.Label, group.Preview = key.Label, key.Preview
			}
			view.ModelGroups = append(view.ModelGroups, *group)
		}
		sort.Slice(view.ModelGroups, func(i, j int) bool {
			a, b := view.ModelGroups[i], view.ModelGroups[j]
			if a.BillingModel != b.BillingModel {
				return a.BillingModel < b.BillingModel
			}
			if a.RequestedModel != b.RequestedModel {
				return a.RequestedModel < b.RequestedModel
			}
			if a.ReportedModel != b.ReportedModel {
				return a.ReportedModel < b.ReportedModel
			}
			return a.Key < b.Key
		})
		top := slices.Clone(view.UsageDistribution.APIKeys)
		sort.Slice(top, func(i, j int) bool {
			if top[i].TotalTokens != top[j].TotalTokens {
				return top[i].TotalTokens > top[j].TotalTokens
			}
			return top[i].Key < top[j].Key
		})
		for _, key := range top[:min(10, len(top))] {
			if key.TotalTokens > 0 {
				view.KeyTrends = append(view.KeyTrends, billing.AnalysisKeyTrend{Key: key.Key, Label: key.Label, Preview: key.Preview, Points: keyPoints[key.Key]})
			}
		}
	}
	if err := tx.Commit(); err != nil {
		return view, err
	}
	return view, nil
}

func analysisTrendPoints(query billing.RequestEventQuery) billing.AnalysisTrends {
	location := query.Timezone
	if location == nil {
		location = time.UTC
	}
	daily := query.Granularity == "day" || ((query.Granularity == "" || query.Granularity == "auto") && query.To.Sub(query.From) > 24*time.Hour)
	start := query.From
	if query.Granularity == "hour" {
		local := query.From.In(location)
		start = local.Add(-time.Duration(local.Minute())*time.Minute - time.Duration(local.Second())*time.Second - time.Duration(local.Nanosecond()))
	}
	if daily {
		local := query.From.In(location)
		start = time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, location)
	}
	points := []billing.AnalysisTrendPoint{}
	for cursor := start; cursor.Before(query.To); {
		points = append(points, billing.AnalysisTrendPoint{Time: cursor})
		if daily {
			cursor = cursor.AddDate(0, 0, 1)
		} else {
			cursor = cursor.Add(time.Hour)
		}
	}
	return billing.AnalysisTrends{
		Requests: points, TotalTokens: slices.Clone(points),
		UncachedInputTokens: slices.Clone(points), OutputTokens: slices.Clone(points),
		CacheReadTokens: slices.Clone(points), CacheWriteTokens: slices.Clone(points),
		CacheRate: slices.Clone(points), TotalCost: slices.Clone(points),
	}
}

type analysisGroups map[string]*billing.AnalysisComposition

func (groups analysisGroups) add(key string, part billing.AnalysisSummary) {
	if groups[key] == nil {
		groups[key] = &billing.AnalysisComposition{Key: key, Label: key}
	}
	row := groups[key]
	row.Requests += part.Requests
	row.TotalTokens += part.TotalTokens
	row.CostUSD += part.Cost.TotalUSD
}

func (groups analysisGroups) finish() []billing.AnalysisComposition {
	rows := make([]billing.AnalysisComposition, 0, len(groups))
	var tokens, requests int64
	for _, row := range groups {
		rows = append(rows, *row)
		tokens += row.TotalTokens
		requests += row.Requests
	}
	for index := range rows {
		if tokens > 0 {
			rows[index].Percent = float64(rows[index].TotalTokens) * 100 / float64(tokens)
		} else if requests > 0 {
			rows[index].Percent = float64(rows[index].Requests) * 100 / float64(requests)
		}
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].TotalTokens != rows[j].TotalTokens {
			return rows[i].TotalTokens > rows[j].TotalTokens
		}
		if rows[i].Requests != rows[j].Requests {
			return rows[i].Requests > rows[j].Requests
		}
		if rows[i].Label != rows[j].Label {
			return rows[i].Label < rows[j].Label
		}
		return rows[i].Key < rows[j].Key
	})
	return rows
}

func analysisSQL(query billing.RequestEventQuery, since time.Time, boundaries []billing.AnalysisTrendPoint) (string, []any) {
	where, args := eventFilter(requestEventSource, query, since)
	modelSQL := "coalesce(NULLIF(" + eventModelSQL + ", ''), '')"
	sourceSQL := "coalesce(NULLIF(" + requestEventSourceName + ", ''), '')"
	var statement strings.Builder
	queryArgs := make([]any, 0, len(boundaries)*(len(args)+2))
	for index, boundary := range boundaries {
		end := query.To
		if index+1 < len(boundaries) {
			end = boundaries[index+1].Time
		}
		if index > 0 {
			statement.WriteString(" UNION ALL ")
		}
		// Indexed calendar ranges preserve DST without a CASE per event.
		statement.WriteString(`SELECT ` + fmt.Sprint(index) + `, r.scope, ` + modelSQL + `, ` + sourceSQL + `,
			count(*), sum(r.failed != 0),
			sum(r.uncached_input_tokens + r.cache_read_tokens + r.cache_write_tokens), sum(r.billed_output_tokens),
			sum(r.cache_read_tokens), sum(r.cache_write_tokens),
			sum(r.total_usd), sum(r.uncached_input_usd), sum(r.cache_read_usd),
			sum(r.cache_write_usd), sum(r.output_usd), coalesce(r.requested_model,''), coalesce(r.reported_model,''),
 coalesce(sum(CASE WHEN r.billing_multiplier > 0 THEN r.total_usd/r.billing_multiplier ELSE 0 END),0),
 sum(CASE WHEN r.billing_multiplier > 0 THEN 0 ELSE 1 END),
 sum(CASE WHEN r.accounting_quality IN ('complete','unclassified') THEN 0 ELSE 1 END), coalesce(r.billing_model,'') ` + where + `
			AND r.at >= ? AND r.at < ? GROUP BY 2, 3, 4, 16, 17, 21`)
		queryArgs = append(queryArgs, args...)
		queryArgs = append(queryArgs, nanos(boundary.Time), nanos(end))
	}
	return statement.String(), queryArgs
}

func labelAnalysisKeys(tx *sql.Tx, keys analysisGroups) error {
	if len(keys) == 0 {
		return nil
	}
	if key := keys[""]; key != nil {
		key.Label = "Unassigned"
	}
	metadata, err := tx.Query("SELECT scope, preview, label FROM api_keys")
	if err != nil {
		return fmt.Errorf("Read analysis key metadata: %w", err)
	}
	defer metadata.Close()
	for metadata.Next() {
		var scope, preview, label string
		if err := metadata.Scan(&scope, &preview, &label); err != nil {
			return fmt.Errorf("Read analysis key metadata: %w", err)
		}
		if key := keys[scope]; key != nil {
			key.Preview = preview
			if label != "" {
				key.Label = label
			} else if preview != "" {
				key.Label = preview
			}
		}
	}
	if err := metadata.Err(); err != nil {
		return fmt.Errorf("Read analysis key metadata: %w", err)
	}
	return nil
}
