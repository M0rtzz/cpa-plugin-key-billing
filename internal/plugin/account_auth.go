package plugin

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"strings"
	"time"

	"cpa-key-billing/internal/billing"
)

const (
	accountAuthTimeout = 3 * time.Second
	accountAuthMaxBody = 4 << 20
)

// validateAccountRequest checks the current CPA access policy before consulting
// historical billing state. Keys live only in this call; success is not cached.
func (a *App) validateAccountRequest(req ManagementRequest) *ManagementResponse {
	if _, ok := accountScope(req.Headers); !ok {
		response := apiKeyUnauthorized()
		return &response
	}
	for _, char := range req.Headers.Get("Authorization") {
		if (char < 32 && char != '\t') || char == 127 {
			response := apiKeyUnauthorized()
			return &response
		}
	}
	if a == nil || a.store == nil {
		return accountAuthUnavailable("account_auth_unconfigured")
	}
	origin, err := billing.ValidateAccountAPIBaseURL(a.store.Config().AccountAPIBaseURL)
	if err != nil || origin == "" {
		return accountAuthUnavailable("account_auth_unconfigured")
	}

	ctx, cancel := context.WithTimeout(context.Background(), accountAuthTimeout)
	defer cancel()
	transport := &http.Transport{
		// Never use CPA's upstream proxy or the process proxy environment.
		Proxy:                  nil,
		DialContext:            (&net.Dialer{Timeout: accountAuthTimeout}).DialContext,
		DisableKeepAlives:      true,
		DisableCompression:     true,
		TLSHandshakeTimeout:    accountAuthTimeout,
		MaxResponseHeaderBytes: 64 << 10,
	}
	defer transport.CloseIdleConnections()
	client := &http.Client{
		Transport:     transport,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	endpoint := origin + "/v1/models"
	// CPA can accept every request after the final configured API key is
	// removed. An unauthenticated request must still be rejected before its
	// response can be treated as proof of a particular user's identity.
	status, _, ok := accountAuthModels(ctx, client, endpoint, "")
	if !ok || status != http.StatusUnauthorized {
		return accountAuthUnavailable("account_auth_unavailable")
	}
	parts := strings.Fields(req.Headers.Get("Authorization"))
	status, body, ok := accountAuthModels(ctx, client, endpoint, "Bearer "+parts[1])
	if !ok {
		return accountAuthUnavailable("account_auth_unavailable")
	}
	if status == http.StatusUnauthorized || status == http.StatusForbidden {
		response := apiKeyUnauthorized()
		return &response
	}
	if status != http.StatusOK {
		return accountAuthUnavailable("account_auth_unavailable")
	}
	var models struct {
		Object string            `json:"object"`
		Data   []json.RawMessage `json:"data"`
	}
	if json.Unmarshal(body, &models) != nil || models.Object != "list" || models.Data == nil {
		return accountAuthUnavailable("account_auth_unavailable")
	}
	return nil
}

func accountAuthModels(ctx context.Context, client *http.Client, endpoint, authorization string) (int, []byte, bool) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return 0, nil, false
	}
	req.Header.Set("Accept", "application/json")
	if authorization != "" {
		req.Header.Set("Authorization", authorization)
	}
	response, err := client.Do(req)
	if err != nil {
		return 0, nil, false
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, accountAuthMaxBody+1))
	if err != nil || len(body) > accountAuthMaxBody {
		return 0, nil, false
	}
	return response.StatusCode, body, true
}

func accountAuthUnavailable(code string) *ManagementResponse {
	response := apiKeyJSONError(http.StatusServiceUnavailable, code, "API key verification is unavailable. Please contact your administrator or try again later.")
	return &response
}
