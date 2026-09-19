package plugin

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"cpa-key-billing/internal/billing"
	"cpa-key-billing/internal/messages"
)

const authQuotaCacheLimit = 512
const authQuotaFreshFor = 60 * time.Second

// The cache contains normalized observations only. All maintenance runs inside
// host calls; no credentials, background refreshes, or timers are retained.
type quotaCacheState struct {
	mu      sync.Mutex
	secret  [32]byte
	ready   bool
	entries map[string]quotaCacheEntry
}

type quotaCacheEntry struct {
	quota    authQuotaResponse
	revision string
	failed   bool
}

func (a *App) accountCredentialIdentity(ref, provider, scope string) (string, string) {
	a.quotaCache.mu.Lock()
	if !a.quotaCache.ready {
		_, _ = rand.Read(a.quotaCache.secret[:])
		a.quotaCache.ready = true
	}
	secret := a.quotaCache.secret
	a.quotaCache.mu.Unlock()
	mac := hmac.New(sha256.New, secret[:])
	_, _ = fmt.Fprintf(mac, "account:v1\x00%s\x00%s\x00%s", scope, provider, ref)
	id := "account_" + hex.EncodeToString(mac.Sum(nil))
	label := map[string]string{"codex": "Codex", "claude": "Claude", "kimi": "Kimi", "xai": "xAI", "antigravity": "Antigravity"}[authCategory(provider)]
	if label == "" {
		label = "Upstream"
	}
	return id, label + " account " + strings.TrimPrefix(id, "account_")[:8]
}

func (a *App) accountAuthIdentity(file hostAuthFile, scope string) (string, string) {
	identity := file.ID
	if identity == "" {
		identity = "auth_index:" + file.AuthIndex
	}
	return a.accountCredentialIdentity(billing.CredentialFingerprint(identity), authFileProvider(file), scope)
}

func authFileProvider(file hostAuthFile) string {
	return authCategory(firstNonEmptyString(file.Provider, file.Type))
}

func authQuotaCacheKey(file hostAuthFile) string {
	return authFileProvider(file) + "\x00" + file.ID + "\x00" + file.AuthIndex
}

func cloneAuthQuota(result authQuotaResponse) authQuotaResponse {
	result.Quota = append([]quotaRow{}, result.Quota...)
	for i := range result.Quota {
		row := &result.Quota[i]
		if row.RemainingPercent != nil {
			row.RemainingPercent = floatPointer(*row.RemainingPercent)
		}
		if row.Used != nil {
			row.Used = floatPointer(*row.Used)
		}
		if row.Limit != nil {
			row.Limit = floatPointer(*row.Limit)
		}
	}
	if result.RateLimitResetCreditsAvailableCount != nil {
		count := *result.RateLimitResetCreditsAvailableCount
		result.RateLimitResetCreditsAvailableCount = &count
	}
	return result
}

func (a *App) storeAuthQuota(file hostAuthFile, result authQuotaResponse) {
	if authFileProvider(file) == "codex" {
		result.Plan = normalizeCodexPlan(result.Plan)
	}
	a.quotaCache.mu.Lock()
	defer a.quotaCache.mu.Unlock()
	if a.quotaCache.entries == nil {
		a.quotaCache.entries = make(map[string]quotaCacheEntry)
	}
	key := authQuotaCacheKey(file)
	a.evictAuthQuotaLocked(key)
	a.quotaCache.entries[key] = quotaCacheEntry{quota: cloneAuthQuota(result), revision: authFileRevision(file)}
}

func (a *App) evictAuthQuotaLocked(key string) {
	if _, exists := a.quotaCache.entries[key]; !exists && len(a.quotaCache.entries) >= authQuotaCacheLimit {
		oldestKey := ""
		var oldest time.Time
		for candidate, entry := range a.quotaCache.entries {
			if oldestKey == "" || entry.quota.FetchedAt.Before(oldest) {
				oldestKey, oldest = candidate, entry.quota.FetchedAt
			}
		}
		delete(a.quotaCache.entries, oldestKey)
	}
}

func (a *App) markAuthQuotaFailure(file hostAuthFile) {
	a.quotaCache.mu.Lock()
	defer a.quotaCache.mu.Unlock()
	key := authQuotaCacheKey(file)
	if a.quotaCache.entries == nil {
		a.quotaCache.entries = make(map[string]quotaCacheEntry)
	}
	a.evictAuthQuotaLocked(key)
	entry, exists := a.quotaCache.entries[key]
	if !exists {
		entry = quotaCacheEntry{revision: authFileRevision(file)}
	}
	entry.failed = true
	a.quotaCache.entries[key] = entry
}

func (a *App) cachedAuthQuota(file hostAuthFile) (authQuotaResponse, bool, bool, bool) {
	a.quotaCache.mu.Lock()
	defer a.quotaCache.mu.Unlock()
	key := authQuotaCacheKey(file)
	entry, exists := a.quotaCache.entries[key]
	if !exists {
		return authQuotaResponse{}, false, false, false
	}
	return cloneAuthQuota(entry.quota), true, entry.failed, entry.revision != authFileRevision(file)
}

type accountQuotaObservation struct {
	Plan      string     `json:"plan,omitempty"`
	Status    string     `json:"status"`
	Reason    string     `json:"reason,omitempty"`
	FetchedAt *time.Time `json:"fetched_at,omitempty"`
	Stale     bool       `json:"stale"`
	Quota     []quotaRow `json:"quota"`
}

func safeQuotaText(value string) string {
	return emailLikeToken.ReplaceAllString(cleanText(value), "[redacted]")
}

func restrictAccountQuotaScope(observation accountQuotaObservation) accountQuotaObservation {
	if observation.Status != "disabled" && observation.Status != "unavailable" {
		observation.Status, observation.Reason = "unsupported", "model_scope_unknown"
	}
	observation.Quota = []quotaRow{}
	return observation
}

func (a *App) accountCachedQuota(file hostAuthFile) accountQuotaObservation {
	out := accountQuotaObservation{Status: "not_loaded", Quota: []quotaRow{}}
	result, exists, failed, changed := a.cachedAuthQuota(file)
	if exists {
		out.Status, out.Plan = "ready", safeQuotaText(result.Plan)
		out.Stale = failed || changed || result.FetchedAt.IsZero() || time.Since(result.FetchedAt) > authQuotaFreshFor
		if !result.FetchedAt.IsZero() {
			out.FetchedAt = &result.FetchedAt
		}
		out.Quota = result.Quota
	}
	supported, _ := authQuotaAvailability(file, authFileProvider(file))
	switch {
	case file.Disabled:
		out.Status, out.Quota = "disabled", []quotaRow{}
	case file.Unavailable:
		out.Status, out.Quota = "unavailable", []quotaRow{}
	case !supported:
		out.Status, out.Quota = "unsupported", []quotaRow{}
	case changed:
		out.Status, out.Quota = "not_loaded", []quotaRow{}
	case failed:
		out.Status = "failed"
	}
	for i := range out.Quota {
		row := &out.Quota[i]
		row.Label, row.GroupLabel, row.LabelPrefix = safeQuotaText(row.Label), safeQuotaText(row.GroupLabel), safeQuotaText(row.LabelPrefix)
		// Rebuild translated messages from sanitized display text instead of
		// returning provider-controlled translation parameters to the browser.
		row.LabelMessage, row.GroupMessage = messages.Literal(row.Label), messages.Literal(row.GroupLabel)
		row.groupKey, row.windowKey, row.unit = safeQuotaText(row.groupKey), safeQuotaText(row.windowKey), safeQuotaText(row.unit)
		for _, number := range []**float64{&row.RemainingPercent, &row.Used, &row.Limit} {
			if *number != nil && (math.IsNaN(**number) || math.IsInf(**number, 0)) {
				*number = nil
			}
		}
	}
	return out
}

type accountQuotaAccount struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Provider string `json:"provider"`
	accountQuotaObservation
}

type accountQuotaCounts struct {
	Total               int `json:"total"`
	Disabled            int `json:"disabled"`
	Unsupported         int `json:"unsupported"`
	Unavailable         int `json:"unavailable"`
	Failed              int `json:"failed"`
	NotLoaded           int `json:"not_loaded"`
	Ready               int `json:"ready"`
	Stale               int `json:"stale"`
	ExcludedFromSummary int `json:"excluded_from_summary"`
}

type accountQuotaModelScope struct {
	Restricted bool   `json:"restricted"`
	Complete   bool   `json:"complete"`
	Message    string `json:"message,omitempty"`
}

type accountQuotaGroup struct {
	Provider            string   `json:"provider"`
	Plan                string   `json:"plan,omitempty"`
	Group               string   `json:"group"`
	Window              string   `json:"window"`
	Unit                string   `json:"unit"`
	RemainingPercent    *float64 `json:"remaining_percent,omitempty"`
	SampleCount         int      `json:"sample_count"`
	Used                *float64 `json:"used,omitempty"`
	Limit               *float64 `json:"limit,omitempty"`
	Remaining           *float64 `json:"remaining,omitempty"`
	AbsoluteSampleCount int      `json:"absolute_sample_count"`
	NextResetAt         string   `json:"next_reset_at,omitempty"`
	Stale               bool     `json:"stale"`
}

type accountQuotaSummaryResponse struct {
	Subscription accountSubscription    `json:"subscription"`
	Concurrency  accountConcurrency     `json:"concurrency"`
	Accounts     []accountQuotaAccount  `json:"accounts"`
	Groups       []accountQuotaGroup    `json:"groups"`
	Counts       accountQuotaCounts     `json:"counts"`
	ModelScope   accountQuotaModelScope `json:"model_scope"`
}

func (a *App) accountQuotaSummary(_ ManagementRequest, access viewAccess) ManagementResponse {
	files, err := a.listHostAuthFiles()
	if err != nil {
		return apiKeyJSONError(http.StatusBadGateway, "host_unavailable", "Upstream account information is temporarily unavailable")
	}
	decision := a.store.ResolveRouting(access.Scope, "", "")
	out := accountQuotaSummaryResponse{
		Subscription: accountSubscription{Name: access.Key.PlanName, QuotaView: access.Key.QuotaView},
		Concurrency:  accountConcurrency{Limit: access.Key.ConcurrencyLimit, Current: access.Key.CurrentConcurrency},
		Accounts:     []accountQuotaAccount{}, Groups: []accountQuotaGroup{},
		ModelScope: accountQuotaModelScope{Restricted: decision.RestrictsModels(), Complete: true},
	}
	if decision.ConfigurationError != "" {
		out.ModelScope.Complete = false
		out.ModelScope.Message = "Routing configuration is unavailable; contact the administrator"
		return apiKeyJSON(http.StatusOK, out)
	}
	if decision.RestrictsModels() {
		out.ModelScope.Complete = false
		out.ModelScope.Message = "Account quota is shared; its applicability to the permitted models is unknown and is excluded from the usable summary"
	}
	for _, file := range files {
		if file.AuthIndex == "" || strings.EqualFold(file.AccountType, "api_key") || !routingAllowsAuthFile(file, decision) {
			continue
		}
		id, name := a.accountAuthIdentity(file, access.Scope)
		observation := a.accountCachedQuota(file)
		if decision.RestrictsModels() {
			observation = restrictAccountQuotaScope(observation)
		}
		out.Accounts = append(out.Accounts, accountQuotaAccount{ID: id, Name: name, Provider: safeQuotaText(authFileProvider(file)), accountQuotaObservation: observation})
		out.Counts.Total++
		switch observation.Status {
		case "disabled":
			out.Counts.Disabled++
		case "unsupported":
			out.Counts.Unsupported++
		case "unavailable":
			out.Counts.Unavailable++
		case "failed":
			out.Counts.Failed++
		case "not_loaded":
			out.Counts.NotLoaded++
		case "ready":
			out.Counts.Ready++
		}
		if observation.Stale {
			out.Counts.Stale++
		}
		if decision.RestrictsModels() {
			out.Counts.ExcludedFromSummary++
		}
	}
	sort.Slice(out.Accounts, func(i, j int) bool { return out.Accounts[i].Name < out.Accounts[j].Name })
	if !decision.RestrictsModels() {
		out.Groups = aggregateAccountQuota(out.Accounts)
	}
	return apiKeyJSON(http.StatusOK, out)
}

func aggregateAccountQuota(accounts []accountQuotaAccount) []accountQuotaGroup {
	groups := map[string]*accountQuotaGroup{}
	for _, account := range accounts {
		if account.Status != "ready" {
			continue
		}
		seen := map[string]bool{}
		for _, row := range account.Quota {
			group := firstNonEmptyString(row.groupKey, row.GroupLabel, strings.TrimSpace(row.LabelPrefix), "shared")
			window := firstNonEmptyString(row.windowKey, row.Label)
			if row.windowSeconds > 0 {
				window = fmt.Sprintf("%ds", row.windowSeconds)
			}
			unit := firstNonEmptyString(row.Currency, row.unit, "units")
			if row.Used == nil && row.Limit == nil && row.Currency == "" {
				unit = "percent"
			}
			key := strings.Join([]string{account.Provider, account.Plan, group, window, unit}, "\x00")
			if seen[key] {
				continue
			}
			seen[key] = true
			entry := groups[key]
			if entry == nil {
				entry = &accountQuotaGroup{Provider: account.Provider, Plan: account.Plan, Group: group, Window: window, Unit: unit}
				groups[key] = entry
			}
			entry.Stale = entry.Stale || account.Stale
			if row.RemainingPercent != nil && !math.IsNaN(*row.RemainingPercent) && !math.IsInf(*row.RemainingPercent, 0) {
				if entry.RemainingPercent == nil {
					entry.RemainingPercent = floatPointer(0)
				}
				*entry.RemainingPercent += math.Max(0, math.Min(100, *row.RemainingPercent))
				entry.SampleCount++
			}
			if row.Used != nil && row.Limit != nil && *row.Used >= 0 && *row.Limit > 0 && !math.IsInf(*row.Used+*row.Limit, 0) {
				if entry.Used == nil {
					entry.Used, entry.Limit, entry.Remaining = floatPointer(0), floatPointer(0), floatPointer(0)
				}
				*entry.Used += *row.Used
				*entry.Limit += *row.Limit
				*entry.Remaining += math.Max(0, *row.Limit-*row.Used)
				entry.AbsoluteSampleCount++
			}
			if reset := quotaResetAt(row.ResetAt); reset != "" && (entry.NextResetAt == "" || reset < entry.NextResetAt) {
				entry.NextResetAt = reset
			}
		}
	}
	keys := make([]string, 0, len(groups))
	for key := range groups {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	out := make([]accountQuotaGroup, 0, len(keys))
	for _, key := range keys {
		group := groups[key]
		if group.SampleCount > 0 {
			*group.RemainingPercent /= float64(group.SampleCount)
		}
		out = append(out, *group)
	}
	return out
}
