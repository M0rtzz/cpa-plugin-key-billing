package plugin

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"cpa-key-billing/internal/billing"
)

func quotaTestHost(t *testing.T, app *App, files []hostAuthFile) {
	t.Helper()
	app.SetHostCaller(func(method string, _ any) (json.RawMessage, error) {
		if method != hostAuthList {
			t.Fatalf("read-only quota called %q", method)
		}
		return mustJSONRaw(t, hostAuthListResponse{Files: files}), nil
	})
}

func TestAccountQuotaIdentityAndUncachedRefreshArePrivate(t *testing.T) {
	app := newConfiguredApp(t)
	file := hostAuthFile{ID: "private-auth-id", AuthIndex: "raw-auth-index", Name: "person@example.com.json", Email: "person@example.com", Type: "codex"}
	quotaTestHost(t, app, []hostAuthFile{file})
	access := viewAccess{APIKey: true, Scope: billing.CallerScope(accountTestKeyA)}
	id, name := app.accountAuthIdentity(file, access.Scope)
	otherID, _ := app.accountAuthIdentity(file, "other-scope")
	secondID, _ := newConfiguredApp(t).accountAuthIdentity(file, access.Scope)
	if id == file.AuthIndex || id == otherID || id == secondID || !strings.HasPrefix(name, "Codex account ") {
		t.Fatalf("opaque identities: %q %q %q %q", id, otherID, secondID, name)
	}
	response := app.authFiles(access)
	for _, value := range []string{file.ID, file.AuthIndex, file.Name, file.Email, `"email"`} {
		if strings.Contains(string(response.Body), value) {
			t.Fatalf("account list leaked %q: %s", value, response.Body)
		}
	}
	for _, refresh := range []string{"", "true", "1"} {
		response = app.authQuota(ManagementRequest{Query: url.Values{"auth_index": {id}, "refresh": {refresh}, "force": {refresh}}}, access)
		if response.StatusCode != http.StatusOK || !strings.Contains(string(response.Body), `"status":"not_loaded"`) {
			t.Fatalf("uncached response = %d %s", response.StatusCode, response.Body)
		}
	}
	response = app.authQuota(ManagementRequest{Query: url.Values{"auth_index": {otherID}}}, access)
	if response.StatusCode != http.StatusNotFound {
		t.Fatalf("cross-scope identifier accepted: %s", response.Body)
	}
}

func TestAccountQuotaSummaryReadOnlyDisabledAndStale(t *testing.T) {
	app := newConfiguredApp(t)
	files := []hostAuthFile{
		{ID: "one", AuthIndex: "one", Type: "codex", Email: "one@example.com"},
		{ID: "two", AuthIndex: "two", Type: "codex", Disabled: true},
		{ID: "three", AuthIndex: "three", Type: "codex"},
	}
	quotaTestHost(t, app, files)
	app.storeAuthQuota(files[0], authQuotaResponse{Plan: "pro", FetchedAt: time.Now().Add(-25 * time.Hour), Quota: []quotaRow{{Label: "5-hour limit", RemainingPercent: floatPointer(30)}}})
	response := app.accountQuotaSummary(ManagementRequest{Query: url.Values{"refresh": {"true"}}}, viewAccess{APIKey: true, Scope: "scope"})
	if response.StatusCode != http.StatusOK {
		t.Fatalf("summary = %d %s", response.StatusCode, response.Body)
	}
	var result accountQuotaSummaryResponse
	if err := json.Unmarshal(response.Body, &result); err != nil {
		t.Fatal(err)
	}
	if result.Counts.Total != 3 || result.Counts.Disabled != 1 || result.Counts.NotLoaded != 1 || result.Counts.Stale != 1 || len(result.Groups) != 1 {
		t.Fatalf("summary = %+v", result)
	}
	if result.Groups[0].Plan != "pro-20x" || !result.Groups[0].Stale || *result.Groups[0].RemainingPercent != 30 {
		t.Fatalf("group = %+v", result.Groups[0])
	}
	for _, account := range result.Accounts {
		if account.Status != "ready" && (len(account.Quota) != 0 || account.FetchedAt != nil) {
			t.Fatalf("invented quota: %+v", account)
		}
	}
	if strings.Contains(string(response.Body), "@") {
		t.Fatalf("email leaked: %s", response.Body)
	}
}

func TestAccountQuotaSummaryCredentialDenyAndModelScope(t *testing.T) {
	app := newConfiguredApp(t)
	scope := billing.CallerScope(accountTestKeyA)
	if _, err := app.store.SyncKeys([]string{accountTestKeyA}, false); err != nil {
		t.Fatal(err)
	}
	files := []hostAuthFile{{ID: "allowed", AuthIndex: "allowed", Type: "codex"}, {ID: "denied", AuthIndex: "denied", Type: "codex"}}
	quotaTestHost(t, app, files)
	if _, err := app.store.CreateRoute(billing.Route{Name: "model and credential restriction", Rule: billing.RouteRule{
		Models: []string{"custom-alias"}, DeniedCredentialIDs: []string{billing.CredentialFingerprint("denied")},
	}}, []string{scope}); err != nil {
		t.Fatal(err)
	}
	for _, file := range files {
		app.storeAuthQuota(file, authQuotaResponse{FetchedAt: time.Now(), Quota: []quotaRow{{Label: "5-hour limit", RemainingPercent: floatPointer(30)}}})
	}
	access := viewAccess{APIKey: true, Scope: scope}
	response := app.accountQuotaSummary(ManagementRequest{}, access)
	var result accountQuotaSummaryResponse
	if err := json.Unmarshal(response.Body, &result); err != nil {
		t.Fatal(err)
	}
	if len(result.Accounts) != 1 || len(result.Groups) != 0 || result.ModelScope.Complete || result.Counts.ExcludedFromSummary != 1 {
		t.Fatalf("summary = %+v", result)
	}
	if result.Accounts[0].Status != "unsupported" || result.Accounts[0].Reason != "model_scope_unknown" || len(result.Accounts[0].Quota) != 0 {
		t.Fatalf("model-restricted quota disclosed: %+v", result.Accounts[0])
	}
	id, _ := app.accountAuthIdentity(files[0], scope)
	response = app.authQuota(ManagementRequest{Query: url.Values{"auth_index": {id}}}, access)
	if strings.Contains(string(response.Body), "remaining_percent") || !strings.Contains(string(response.Body), "model_scope_unknown") {
		t.Fatalf("single-account model scope bypass: %s", response.Body)
	}
	deniedID, _ := app.accountAuthIdentity(files[1], scope)
	response = app.authQuota(ManagementRequest{Query: url.Values{"auth_index": {deniedID}}}, access)
	if response.StatusCode != http.StatusNotFound {
		t.Fatalf("credential deny bypass: %s", response.Body)
	}
}

func TestAuthQuotaCacheRevisionFailuresAndBound(t *testing.T) {
	app := newConfiguredApp(t)
	file := hostAuthFile{ID: "one", AuthIndex: "one", Type: "codex", ModTime: time.Now()}
	value := authQuotaResponse{FetchedAt: time.Now(), Quota: []quotaRow{{RemainingPercent: floatPointer(30)}}}
	app.storeAuthQuota(file, value)
	*value.Quota[0].RemainingPercent = 90
	cached := app.accountCachedQuota(file)
	if *cached.Quota[0].RemainingPercent != 30 || cached.Stale {
		t.Fatalf("cache wasn't copied: %+v", cached)
	}
	app.markAuthQuotaFailure(file)
	if !app.accountCachedQuota(file).Stale {
		t.Fatal("failed admin refresh left old quota fresh")
	}
	file.ModTime = file.ModTime.Add(time.Second)
	if result := app.accountCachedQuota(file); result.Status != "not_loaded" {
		t.Fatalf("changed auth reused stale credentials: %+v", result)
	}
	for i := 0; i <= authQuotaCacheLimit; i++ {
		file.ID, file.AuthIndex = fmt.Sprint(i), fmt.Sprint(i)
		app.storeAuthQuota(file, authQuotaResponse{FetchedAt: time.Now().Add(time.Duration(i) * time.Second)})
	}
	if len(app.quotaCache.entries) != authQuotaCacheLimit {
		t.Fatalf("unbounded cache: %d", len(app.quotaCache.entries))
	}
}

func TestUnavailableAndFailedQuotaRetainOnlyKnownSnapshot(t *testing.T) {
	app := newConfiguredApp(t)
	file := hostAuthFile{ID: "one", AuthIndex: "one", Type: "codex", ModTime: time.Now()}
	value := authQuotaResponse{Plan: "pro", FetchedAt: time.Now(), Quota: []quotaRow{{RemainingPercent: floatPointer(30)}}}
	app.storeAuthQuota(file, value)
	for _, state := range []string{"disabled", "unavailable", "runtime_only"} {
		changed := file
		changed.ModTime = changed.ModTime.Add(time.Second)
		changed.Disabled = state == "disabled"
		changed.Unavailable = state == "unavailable"
		changed.RuntimeOnly = state == "runtime_only"
		observation := app.accountCachedQuota(changed)
		if observation.Plan != "pro-20x" || observation.FetchedAt == nil || len(observation.Quota) != 0 || !observation.Stale {
			t.Fatalf("%s lost known metadata or exposed quota: %+v", state, observation)
		}
	}
	app.markAuthQuotaFailure(file)
	previous := app.accountCachedQuota(file)
	if previous.Status != "failed" || !previous.Stale || len(previous.Quota) != 1 || previous.Plan != "pro-20x" {
		t.Fatalf("failed refresh lost snapshot: %+v", previous)
	}
	fresh := hostAuthFile{ID: "new", AuthIndex: "new", Type: "codex"}
	app.markAuthQuotaFailure(fresh)
	initial := app.accountCachedQuota(fresh)
	if initial.Status != "failed" || initial.FetchedAt != nil || initial.Plan != "" || len(initial.Quota) != 0 {
		t.Fatalf("first failure invented successful snapshot: %+v", initial)
	}
	quotaTestHost(t, app, []hostAuthFile{file, fresh})
	response := app.accountQuotaSummary(ManagementRequest{}, viewAccess{APIKey: true, Scope: "scope"})
	var summary accountQuotaSummaryResponse
	if err := json.Unmarshal(response.Body, &summary); err != nil {
		t.Fatal(err)
	}
	if summary.Counts.Failed != 2 || len(summary.Groups) != 0 {
		t.Fatalf("failed snapshots entered usable summary: %+v", summary)
	}
}

func TestAggregateQuotaSeparatesUnitsPlansAndUnknownValues(t *testing.T) {
	accounts := []accountQuotaAccount{
		{Provider: "codex", accountQuotaObservation: accountQuotaObservation{Status: "ready", Plan: "plus", Quota: []quotaRow{
			{Label: "5-hour limit", RemainingPercent: floatPointer(20)},
			{Label: "Weekly limit"},
			{Label: "Spending", Currency: "USD", Used: floatPointer(2), Limit: floatPointer(10)},
		}}},
		{Provider: "codex", accountQuotaObservation: accountQuotaObservation{Status: "ready", Plan: "plus", Quota: []quotaRow{
			{Label: "5-hour limit", RemainingPercent: floatPointer(80)},
			{Label: "Spending", Currency: "USD", Used: floatPointer(3), Limit: floatPointer(20)},
			{Label: "Spending", Currency: "EUR", Used: floatPointer(1), Limit: floatPointer(4)},
		}}},
		{Provider: "codex", accountQuotaObservation: accountQuotaObservation{Status: "ready", Plan: "pro-20x", Quota: []quotaRow{{Label: "5-hour limit", RemainingPercent: floatPointer(10)}}}},
	}
	groups := aggregateAccountQuota(accounts)
	if len(groups) != 5 {
		t.Fatalf("group count = %d: %+v", len(groups), groups)
	}
	for _, group := range groups {
		switch {
		case group.Plan == "plus" && group.Window == "5-hour limit":
			if group.SampleCount != 2 || *group.RemainingPercent != 50 {
				t.Fatalf("not a percentage mean: %+v", group)
			}
		case group.Window == "Weekly limit":
			if group.RemainingPercent != nil || group.SampleCount != 0 {
				t.Fatalf("unknown turned into zero: %+v", group)
			}
		case group.Unit == "USD":
			if *group.Used != 5 || *group.Limit != 30 || *group.Remaining != 25 || group.AbsoluteSampleCount != 2 {
				t.Fatalf("wrong absolute totals: %+v", group)
			}
		}
	}
	var row quotaRow
	fillQuotaValues(&row, map[string]any{"limit": float64(100)})
	if row.Used != nil || row.RemainingPercent != nil {
		t.Fatalf("limit-only response fabricated unused quota: %+v", row)
	}
}

func TestAccountQuotaHostErrorsHideIdentity(t *testing.T) {
	app := newConfiguredApp(t)
	app.SetHostCaller(func(string, any) (json.RawMessage, error) {
		return nil, fmt.Errorf("private@example.com /secret/auth.json")
	})
	access := viewAccess{APIKey: true, Scope: "scope"}
	responses := []ManagementResponse{app.authFiles(access), app.authQuota(ManagementRequest{Query: url.Values{"auth_index": {"opaque"}}}, access), app.accountQuotaSummary(ManagementRequest{}, access)}
	for _, response := range responses {
		if response.StatusCode != http.StatusBadGateway || strings.Contains(string(response.Body), "private") || strings.Contains(string(response.Body), "/secret") {
			t.Fatalf("host error leaked: %d %s", response.StatusCode, response.Body)
		}
	}
}
