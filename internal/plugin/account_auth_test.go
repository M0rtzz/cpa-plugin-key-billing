package plugin

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"cpa-key-billing/internal/billing"
)

const validationTestKey = "sk-account-validation-dummy"

func accountAuthTestApp(t *testing.T, origin string) *App {
	t.Helper()
	app := newApp(billing.NewStore(openRepository, nil))
	cfg := billing.DefaultConfig()
	cfg.Enabled = true
	cfg.StateFile = filepath.Join(t.TempDir(), "billing.db")
	cfg.AccountAPIBaseURL = origin
	if err := app.store.Configure(cfg); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(app.Shutdown)
	return app
}

func accountAuthRequest(key string) ManagementRequest {
	headers := http.Header{}
	if key != "" {
		headers.Set("Authorization", "Bearer "+key)
	}
	return ManagementRequest{Method: http.MethodGet, Headers: headers}
}

func assertAccountAuthStatus(t *testing.T, response *ManagementResponse, status int) {
	t.Helper()
	if response == nil || response.StatusCode != status {
		t.Fatalf("response = %+v, expected HTTP %d", response, status)
	}
	if response.Headers.Get("Cache-Control") != "private, no-store" {
		t.Fatal("authentication error is cacheable")
	}
	if strings.Contains(string(response.Body), validationTestKey) {
		t.Fatal("authentication error disclosed the key")
	}
}

func TestAccountAuthCurrentKeyAndRevocation(t *testing.T) {
	var authorized atomic.Bool
	var calls atomic.Int32
	authorized.Store(true)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.URL.Path != "/v1/models" || r.Method != http.MethodGet || r.URL.RawQuery != "" {
			t.Errorf("unexpected validation request: %s %s", r.Method, r.URL)
		}
		if r.Header.Get("Authorization") != "Bearer "+validationTestKey || !authorized.Load() {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_, _ = fmt.Fprint(w, `{"object":"list","data":[]}`)
	}))
	defer server.Close()
	app := accountAuthTestApp(t, server.URL)
	if response := app.validateAccountRequest(accountAuthRequest(validationTestKey)); response != nil {
		t.Fatalf("valid untracked key rejected: %+v", response)
	}
	if len(app.store.KeyViews()) != 0 {
		t.Fatal("authentication created a billing record")
	}
	if _, err := app.store.SyncKeys([]string{validationTestKey}, false); err != nil {
		t.Fatal(err)
	}
	if response := app.validateAccountRequest(accountAuthRequest(validationTestKey)); response != nil {
		t.Fatalf("valid tracked key rejected: %+v", response)
	}
	authorized.Store(false)
	assertAccountAuthStatus(t, app.validateAccountRequest(accountAuthRequest(validationTestKey)), http.StatusUnauthorized)
	if _, tracked := app.store.KeyViewForScope(billing.CallerScope(validationTestKey)); !tracked {
		t.Fatal("test did not preserve the revoked key's historical billing record")
	}
	if calls.Load() != 6 {
		t.Fatalf("validation calls = %d, expected anonymous and keyed checks on each request", calls.Load())
	}
}

func TestAccountAuthRejectsInvalidBearerWithoutNetwork(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { calls.Add(1) }))
	defer server.Close()
	app := accountAuthTestApp(t, server.URL)
	for _, values := range [][]string{nil, {"Basic dummy"}, {"Bearer"}, {"Bearer one two"}, {"Bearer one\n"}, {"Bearer one\x00"}, {"Bearer " + strings.Repeat("x", 8193)}, {"Bearer one", "Bearer two"}} {
		req := ManagementRequest{Headers: http.Header{"Authorization": values}}
		assertAccountAuthStatus(t, app.validateAccountRequest(req), http.StatusUnauthorized)
	}
	if calls.Load() != 0 {
		t.Fatal("malformed credentials caused a network request")
	}
}

func TestAccountAuthFailsClosed(t *testing.T) {
	for _, test := range []struct {
		name            string
		anonymousStatus int
		keyStatus       int
		body            string
		want            int
	}{
		{"open authentication", 200, 200, `{"object":"list","data":[]}`, 503},
		{"anonymous forbidden", 403, 200, `{"object":"list","data":[]}`, 503},
		{"revoked key", 401, 401, "", 401},
		{"forbidden key", 401, 403, "", 401},
		{"server error", 401, 500, "", 503},
		{"unexpected successful status", 401, 204, "", 503},
		{"HTML response", 401, 200, "<html>Sign in</html>", 503},
		{"missing data", 401, 200, `{"object":"list"}`, 503},
		{"null data", 401, 200, `{"object":"list","data":null}`, 503},
		{"wrong response object", 401, 200, `{"object":"account","data":[]}`, 503},
		{"oversized response", 401, 200, `{"object":"list","data":[],"padding":"` + strings.Repeat("x", accountAuthMaxBody) + `"}`, 503},
	} {
		t.Run(test.name, func(t *testing.T) {
			var keyedCalls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Authorization") == "" {
					w.WriteHeader(test.anonymousStatus)
					return
				}
				keyedCalls.Add(1)
				w.WriteHeader(test.keyStatus)
				_, _ = fmt.Fprint(w, test.body)
			}))
			defer server.Close()
			app := accountAuthTestApp(t, server.URL)
			assertAccountAuthStatus(t, app.validateAccountRequest(accountAuthRequest(validationTestKey)), test.want)
			if test.anonymousStatus != 401 && keyedCalls.Load() != 0 {
				t.Fatal("sent key before verifying anonymous access is denied")
			}
		})
	}
}

func TestAccountAuthConfigurationAndConnectionFailure(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	server.Close()
	for _, origin := range []string{"", "http://192.0.2.1:8317", "http://dummy-secret@127.0.0.1:8317", server.URL} {
		t.Run(origin, func(t *testing.T) {
			app := accountAuthTestApp(t, origin)
			assertAccountAuthStatus(t, app.validateAccountRequest(accountAuthRequest(validationTestKey)), http.StatusServiceUnavailable)
		})
	}
}

func TestAccountAuthDoesNotFollowRedirects(t *testing.T) {
	var targetCalls atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		targetCalls.Add(1)
		_, _ = fmt.Fprint(w, `{"object":"list","data":[]}`)
	}))
	defer target.Close()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		http.Redirect(w, r, target.URL, http.StatusFound)
	}))
	defer server.Close()
	app := accountAuthTestApp(t, server.URL)
	assertAccountAuthStatus(t, app.validateAccountRequest(accountAuthRequest(validationTestKey)), http.StatusServiceUnavailable)
	if targetCalls.Load() != 0 {
		t.Fatal("followed an authentication redirect")
	}
}

func TestAccountAuthTimeout(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	defer server.Close()
	app := accountAuthTestApp(t, server.URL)
	started := time.Now()
	assertAccountAuthStatus(t, app.validateAccountRequest(accountAuthRequest(validationTestKey)), http.StatusServiceUnavailable)
	if elapsed := time.Since(started); elapsed > accountAuthTimeout+2*time.Second {
		t.Fatalf("validation exceeded deadline: %s", elapsed)
	}
}
