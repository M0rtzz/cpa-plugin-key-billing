package plugin

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

func callSelfServiceRequest(t *testing.T, app *App, request ManagementRequest) ManagementResponse {
	t.Helper()
	raw, err := app.HandleMethod(MethodManagementHandle, mustMarshal(t, request))
	if err != nil {
		t.Fatal(err)
	}
	var response ManagementResponse
	decodeResult(t, raw, &response)
	return response
}

func sessionCookieFromResponse(t *testing.T, response ManagementResponse) *http.Cookie {
	t.Helper()
	httpResponse := http.Response{Header: response.Headers}
	cookies := httpResponse.Cookies()
	if len(cookies) != 1 {
		t.Fatalf("cookies = %+v", cookies)
	}
	return cookies[0]
}

func TestAccountSessionPersistsWithoutBrowserKeyStorage(t *testing.T) {
	app, accepted := selfServiceAuthority(t)
	if _, err := app.store.SyncKeys([]string{validationTestKey}, false); err != nil {
		t.Fatal(err)
	}
	login := callSelfServiceRequest(t, app, ManagementRequest{
		Method: http.MethodGet, Path: resourceBase + resourceSessionPath,
		Headers: http.Header{"Authorization": {"Bearer " + validationTestKey}, "X-Forwarded-Proto": {"https"}},
	})
	if login.StatusCode != http.StatusOK || strings.Contains(string(login.Body), validationTestKey) ||
		strings.Contains(login.Headers.Get("Set-Cookie"), validationTestKey) {
		t.Fatalf("login response exposed key: %+v", login)
	}
	cookie := sessionCookieFromResponse(t, login)
	if !cookie.HttpOnly || !cookie.Secure || cookie.SameSite != http.SameSiteStrictMode || cookie.MaxAge != int(accountSessionTTL.Seconds()) ||
		cookie.Path != resourceBase+"/" || cookie.Value == "" {
		t.Fatalf("session cookie = %+v", cookie)
	}
	profile := callSelfServiceRequest(t, app, ManagementRequest{
		Method: http.MethodGet, Path: resourceBase + routeProfile,
		Headers: http.Header{"Cookie": {cookie.String()}},
	})
	if profile.StatusCode != http.StatusOK || !strings.Contains(string(profile.Body), `"tracked":true`) ||
		profile.Headers.Get("Vary") != "Authorization, Cookie" {
		t.Fatalf("cookie profile = %+v", profile)
	}

	accepted.Store(false)
	revoked := callSelfServiceRequest(t, app, ManagementRequest{
		Method: http.MethodGet, Path: resourceBase + routeProfile,
		Headers: http.Header{"Cookie": {cookie.String()}},
	})
	if revoked.StatusCode != http.StatusUnauthorized || sessionCookieFromResponse(t, revoked).MaxAge >= 0 {
		t.Fatalf("revoked session = %+v", revoked)
	}
}

func TestAccountSessionLogoutAndRestartInvalidateCookie(t *testing.T) {
	app, _ := selfServiceAuthority(t)
	login := callSelfServiceRequest(t, app, ManagementRequest{
		Method: http.MethodGet, Path: resourceBase + resourceSessionPath,
		Headers: http.Header{"Authorization": {"Bearer " + validationTestKey}},
	})
	cookie := sessionCookieFromResponse(t, login)
	logout := callSelfServiceRequest(t, app, ManagementRequest{
		Method: http.MethodGet, Path: resourceBase + resourceLogoutPath,
		Headers: http.Header{"Cookie": {cookie.String()}},
	})
	var state struct {
		Authenticated bool `json:"authenticated"`
	}
	if logout.StatusCode != http.StatusOK || json.Unmarshal(logout.Body, &state) != nil || state.Authenticated ||
		sessionCookieFromResponse(t, logout).MaxAge >= 0 {
		t.Fatalf("logout response = %+v", logout)
	}
	profile := callSelfServiceRequest(t, app, ManagementRequest{
		Method: http.MethodGet, Path: resourceBase + routeProfile,
		Headers: http.Header{"Cookie": {cookie.String()}},
	})
	if profile.StatusCode != http.StatusUnauthorized {
		t.Fatalf("logged-out session remained valid: %+v", profile)
	}

	restarted, _ := selfServiceAuthority(t)
	profile = callSelfServiceRequest(t, restarted, ManagementRequest{
		Method: http.MethodGet, Path: resourceBase + routeProfile,
		Headers: http.Header{"Cookie": {cookie.String()}},
	})
	if profile.StatusCode != http.StatusUnauthorized {
		t.Fatalf("session survived app restart: %+v", profile)
	}
}
