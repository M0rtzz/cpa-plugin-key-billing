package plugin

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"net/http"
	"strings"
	"sync"
	"time"
)

const (
	accountSessionCookieName = "cpa_key_billing_session"
	accountSessionTTL        = 7 * 24 * time.Hour
	accountSessionLimit      = 2048
)

type accountSessionState struct {
	mu      sync.Mutex
	entries map[string]accountSessionEntry
}

type accountSessionEntry struct {
	APIKey    string
	ExpiresAt time.Time
}

func accountSessionDigest(token string) string {
	digest := sha256.Sum256([]byte(token))
	return hex.EncodeToString(digest[:])
}

func (s *accountSessionState) create(apiKey string, now time.Time) (string, time.Time, bool) {
	secret := make([]byte, 32)
	if _, err := rand.Read(secret); err != nil {
		return "", time.Time{}, false
	}
	token := base64.RawURLEncoding.EncodeToString(secret)
	expiresAt := now.Add(accountSessionTTL)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.entries == nil {
		s.entries = make(map[string]accountSessionEntry)
	}
	s.purgeLocked(now)
	if len(s.entries) >= accountSessionLimit {
		var oldestKey string
		var oldest time.Time
		for key, entry := range s.entries {
			if oldestKey == "" || entry.ExpiresAt.Before(oldest) {
				oldestKey, oldest = key, entry.ExpiresAt
			}
		}
		delete(s.entries, oldestKey)
	}
	s.entries[accountSessionDigest(token)] = accountSessionEntry{APIKey: apiKey, ExpiresAt: expiresAt}
	return token, expiresAt, true
}

func (s *accountSessionState) resolve(token string, now time.Time) (string, bool) {
	if token == "" || len(token) > 128 {
		return "", false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	entry, ok := s.entries[accountSessionDigest(token)]
	if !ok || !entry.ExpiresAt.After(now) {
		if ok {
			delete(s.entries, accountSessionDigest(token))
		}
		return "", false
	}
	return entry.APIKey, true
}

func (s *accountSessionState) remove(token string) {
	if token == "" || len(token) > 128 {
		return
	}
	s.mu.Lock()
	delete(s.entries, accountSessionDigest(token))
	s.mu.Unlock()
}

func (s *accountSessionState) clear() {
	s.mu.Lock()
	clear(s.entries)
	s.mu.Unlock()
}

func (s *accountSessionState) purgeLocked(now time.Time) {
	for key, entry := range s.entries {
		if !entry.ExpiresAt.After(now) {
			delete(s.entries, key)
		}
	}
}

func accountSessionToken(headers http.Header) (string, bool) {
	request := http.Request{Header: headers}
	cookie, err := request.Cookie(accountSessionCookieName)
	if err != nil || cookie.Value == "" {
		return "", false
	}
	return cookie.Value, true
}

func accountSessionSecure(headers http.Header) bool {
	if strings.EqualFold(strings.TrimSpace(headers.Get("X-Forwarded-Proto")), "https") {
		return true
	}
	return strings.Contains(strings.ToLower(headers.Get("Forwarded")), "proto=https")
}

func setAccountSessionCookie(response *ManagementResponse, token string, expiresAt time.Time, secure bool) {
	cookie := http.Cookie{
		Name: accountSessionCookieName, Value: token, Path: resourceBase + "/",
		Expires: expiresAt, MaxAge: int(accountSessionTTL / time.Second),
		HttpOnly: true, Secure: secure, SameSite: http.SameSiteStrictMode,
	}
	response.Headers.Add("Set-Cookie", cookie.String())
}

func clearAccountSessionCookie(response *ManagementResponse, secure bool) {
	cookie := http.Cookie{
		Name: accountSessionCookieName, Value: "", Path: resourceBase + "/",
		Expires: time.Unix(1, 0).UTC(), MaxAge: -1,
		HttpOnly: true, Secure: secure, SameSite: http.SameSiteStrictMode,
	}
	response.Headers.Add("Set-Cookie", cookie.String())
}

func (a *App) createAccountSession(req ManagementRequest) ManagementResponse {
	if response := a.validateAccountRequest(req); response != nil {
		return *response
	}
	parts := strings.Fields(req.Headers.Get("Authorization"))
	token, expiresAt, ok := a.accountSessions.create(parts[1], time.Now().UTC())
	if !ok {
		return apiKeyJSONError(http.StatusServiceUnavailable, "session_unavailable", "Login session is temporarily unavailable")
	}
	if previous, exists := accountSessionToken(req.Headers); exists {
		a.accountSessions.remove(previous)
	}
	response := apiKeyJSON(http.StatusOK, map[string]any{"authenticated": true, "expires_at": expiresAt})
	setAccountSessionCookie(&response, token, expiresAt, accountSessionSecure(req.Headers))
	return response
}

func (a *App) endAccountSession(req ManagementRequest) ManagementResponse {
	if token, ok := accountSessionToken(req.Headers); ok {
		a.accountSessions.remove(token)
	}
	response := apiKeyJSON(http.StatusOK, map[string]any{"authenticated": false})
	clearAccountSessionCookie(&response, accountSessionSecure(req.Headers))
	return response
}

func (a *App) authorizeAccountRequest(req ManagementRequest) (ManagementRequest, *ManagementResponse) {
	if len(req.Headers.Values("Authorization")) > 0 {
		return req, a.validateAccountRequest(req)
	}
	token, present := accountSessionToken(req.Headers)
	apiKey, ok := a.accountSessions.resolve(token, time.Now().UTC())
	if !ok {
		response := apiKeyUnauthorized()
		if present {
			clearAccountSessionCookie(&response, accountSessionSecure(req.Headers))
		}
		return req, &response
	}
	if req.Headers == nil {
		req.Headers = http.Header{}
	} else {
		req.Headers = req.Headers.Clone()
	}
	req.Headers.Set("Authorization", "Bearer "+apiKey)
	if response := a.validateAccountRequest(req); response != nil {
		if response.StatusCode == http.StatusUnauthorized {
			a.accountSessions.remove(token)
			clearAccountSessionCookie(response, accountSessionSecure(req.Headers))
		}
		return req, response
	}
	return req, nil
}
