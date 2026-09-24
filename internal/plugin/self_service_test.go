package plugin

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"cpa-key-billing/internal/billing"
)

// Unlike callAccount, this helper never supplies missing configuration: these
// tests exercise the actual public route boundary, including fail-closed setup.
func callSelfService(t *testing.T, app *App, path, key string, query url.Values) ManagementResponse {
	t.Helper()
	req := accountAuthRequest(key)
	req.Path = resourceBase + path
	req.Query = query
	raw, err := app.HandleMethod(MethodManagementHandle, mustMarshal(t, req))
	if err != nil {
		t.Fatalf("resource %s: %v", path, err)
	}
	var response ManagementResponse
	decodeResult(t, raw, &response)
	return response
}

func selfServiceAuthority(t *testing.T) (*App, *atomic.Bool) {
	t.Helper()
	accepted := &atomic.Bool{}
	accepted.Store(true)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !accepted.Load() || r.Header.Get("Authorization") != "Bearer "+validationTestKey {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_, _ = fmt.Fprint(w, `{"object":"list","data":[]}`)
	}))
	t.Cleanup(server.Close)
	return accountAuthTestApp(t, server.URL), accepted
}

func TestSelfServiceHTMLIsPublicWithoutAccountConfiguration(t *testing.T) {
	app := accountAuthTestApp(t, "")
	registration := managementRegistration()
	for _, path := range []string{"/usage.html", "/quota.html"} {
		t.Run(path, func(t *testing.T) {
			found := false
			for _, route := range registration.Resources {
				found = found || route.Path == resourceBase+path
			}
			if !found {
				t.Fatal("page is not registered as a public resource")
			}
			response := callSelfService(t, app, path, "", nil)
			if response.StatusCode != http.StatusOK || !strings.HasPrefix(response.Headers.Get("Content-Type"), "text/html") || len(response.Body) == 0 {
				t.Fatalf("public HTML response = %+v", response)
			}
			if !strings.Contains(response.Headers.Get("Content-Security-Policy"), "connect-src 'self'") {
				t.Fatal("page does not restrict API calls to its own origin")
			}
		})
	}
}

func TestSelfServiceAllJSONRoutesRequireCurrentKey(t *testing.T) {
	app, accepted := selfServiceAuthority(t)
	if _, err := app.store.SyncKeys([]string{validationTestKey}, false); err != nil {
		t.Fatal(err)
	}
	if response := callSelfService(t, app, routeProfile, validationTestKey, nil); response.StatusCode != http.StatusOK {
		t.Fatalf("valid key could not access profile: %s", response.Body)
	}
	accepted.Store(false)
	app.SetHostCaller(func(string, any) (json.RawMessage, error) {
		t.Error("an unauthorized resource request reached upstream credentials")
		return nil, fmt.Errorf("unexpected host call")
	})
	for _, endpoint := range resourceEndpoints {
		for _, key := range []string{"", "sk-invalid-dummy-key", validationTestKey} {
			t.Run(endpoint.path+"/"+key, func(t *testing.T) {
				response := callSelfService(t, app, endpoint.path, key, nil)
				assertAccountAuthStatus(t, &response, http.StatusUnauthorized)
			})
		}
	}
}

func TestSelfServiceUnconfiguredJSONFailsClosed(t *testing.T) {
	app := accountAuthTestApp(t, "")
	for _, endpoint := range resourceEndpoints {
		t.Run(endpoint.path, func(t *testing.T) {
			response := callSelfService(t, app, endpoint.path, validationTestKey, nil)
			assertAccountAuthStatus(t, &response, http.StatusServiceUnavailable)
		})
	}
	// Management authentication belongs to CPA and does not depend on the new
	// user portal configuration. Its already-authenticated handler still works.
	callOK(t, app, http.MethodGet, routeKeys, nil, nil, http.StatusOK, nil)
}

func TestSelfServiceNewValidKeyHasUnlimitedSubscriptionAndEmptyUsage(t *testing.T) {
	app, _ := selfServiceAuthority(t)
	profile := callSelfService(t, app, routeProfile, validationTestKey, nil)
	var identity accountProfileResponse
	if profile.StatusCode != http.StatusOK || json.Unmarshal(profile.Body, &identity) != nil || identity.Tracked {
		t.Fatalf("new key profile = %s", profile.Body)
	}
	response := callSelfService(t, app, routeSubscription, validationTestKey, nil)
	var subscription accountSubscriptionResponse
	if response.StatusCode != http.StatusOK || json.Unmarshal(response.Body, &subscription) != nil || !subscription.Subscription.Unlimited || subscription.Subscription.Blocked {
		t.Fatalf("new key subscription = %s", response.Body)
	}
	response = callSelfService(t, app, routeAnalysis, validationTestKey, nil)
	var analysis billing.AnalysisView
	if response.StatusCode != http.StatusOK || json.Unmarshal(response.Body, &analysis) != nil || analysis.Summary.Requests != 0 {
		t.Fatalf("new key analysis = %s", response.Body)
	}
	if len(app.store.KeyViews()) != 0 {
		t.Fatal("viewing an unused key fabricated a billing record")
	}
}

func TestSelfServiceHidesUpstreamIdentitiesWithoutChangingAdminData(t *testing.T) {
	app, _ := selfServiceAuthority(t)
	const account = "private-upstream@example.invalid"
	const authIndex = "private-auth-index"
	const failureType = "private-provider-error-type"
	const failureBody = "private-response-body-with-token"
	const foreignModel = "foreign-account-only-model"
	at := app.store.Now().Add(-time.Minute)
	event := billing.UsageEvent{
		Scope: billing.CallerScope(validationTestKey), KeyPreview: "sk-du…mmy",
		AuthType: "oauth", Provider: "codex", Account: account, AuthIndex: authIndex,
		UpstreamModel: "gpt-5.5", RouteModel: "gpt-5.5", At: at, RequestedAt: at,
	}
	app.store.RecordUsage(event)
	app.store.RecordUsageError(event, billing.RequestError{
		StatusCode: http.StatusForbidden, ErrorType: failureType, Body: failureBody,
	})
	foreign := event
	foreign.Scope = billing.CallerScope("sk-another-user-dummy")
	foreign.UpstreamModel, foreign.RouteModel = foreignModel, foreignModel
	app.store.RecordUsage(foreign)
	for _, path := range []string{routeEvents, routeErrors, routeAnalysis} {
		t.Run(path, func(t *testing.T) {
			response := callSelfService(t, app, path, validationTestKey, url.Values{
				"api_key": {foreign.Scope}, "source": {"nonexistent-source"}, "error_type": {"nonexistent-type"}, "error_type_empty": {"true"},
			})
			if response.StatusCode != http.StatusOK {
				t.Fatalf("user response = %d %s", response.StatusCode, response.Body)
			}
			for _, secret := range []string{account, authIndex, failureType, failureBody, foreignModel, foreign.Scope, event.Scope} {
				if strings.Contains(string(response.Body), secret) {
					t.Fatalf("user response disclosed %q", secret)
				}
			}
			switch path {
			case routeEvents:
				var view billing.RequestEventView
				if err := json.Unmarshal(response.Body, &view); err != nil || view.Total != 2 || view.Filters == nil || len(view.Filters.Sources) != 0 {
					t.Fatalf("user source filtering leaked a record-count oracle: %s", response.Body)
				}
			case routeErrors:
				var view billing.RequestErrorView
				if err := json.Unmarshal(response.Body, &view); err != nil || view.Total != 1 || view.Filters == nil || len(view.Filters.Sources) != 0 || len(view.Filters.ErrorTypes) != 0 {
					t.Fatalf("user error filtering leaked a record-count oracle: %s", response.Body)
				}
				if len(view.Entries) != 1 || view.Entries[0].StatusCode != http.StatusForbidden {
					t.Fatal("user error lost safe status information")
				}
			case routeAnalysis:
				var view billing.AnalysisView
				if err := json.Unmarshal(response.Body, &view); err != nil || view.Summary.Requests != 2 || len(view.UsageDistribution.Sources) != 0 || len(view.UsageDistribution.APIKeys) != 0 {
					t.Fatalf("user analysis is not private to this key: %s", response.Body)
				}
			}
			admin := callManagement(t, app, http.MethodGet, path, nil, nil)
			if admin.StatusCode != http.StatusOK || !strings.Contains(string(admin.Body), account) {
				t.Fatalf("admin lost upstream identity data: %s", admin.Body)
			}
			if path == routeErrors && (!strings.Contains(string(admin.Body), failureType) || !strings.Contains(string(admin.Body), failureBody)) {
				t.Fatal("admin lost upstream error details")
			}
		})
	}
	for _, path := range []string{routeEvents, routeErrors} {
		unfiltered := callManagement(t, app, http.MethodGet, path, nil, nil)
		var available struct {
			Filters struct {
				SourceOptions []billing.RequestSourceOption `json:"source_options"`
			} `json:"filter_options"`
		}
		if unfiltered.StatusCode != http.StatusOK || json.Unmarshal(unfiltered.Body, &available) != nil || len(available.Filters.SourceOptions) == 0 {
			t.Fatalf("admin source options missing: %s", unfiltered.Body)
		}
		response := callManagement(t, app, http.MethodGet, path, url.Values{"source": {available.Filters.SourceOptions[0].Value}}, nil)
		var view struct {
			Total int `json:"total"`
		}
		if response.StatusCode != http.StatusOK || json.Unmarshal(response.Body, &view) != nil || view.Total == 0 {
			t.Fatalf("admin source filter changed: %s", response.Body)
		}
	}
}
