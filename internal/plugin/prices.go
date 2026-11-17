package plugin

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"cpa-key-billing/internal/billing"
)

func (a *App) listPrices(req ManagementRequest, access viewAccess) ManagementResponse {
	models := req.Query["model"]
	includeCustom := !access.APIKey
	if raw := req.Query.Get("include_custom"); raw != "" {
		value, err := strconv.ParseBool(raw)
		if err != nil {
			return viewJSONError(access, http.StatusBadRequest, "invalid", "include_custom must be true or false")
		}
		includeCustom = includeCustom && value
	}
	includeOfficial := false
	if raw := req.Query.Get("include_official"); raw != "" {
		value, err := strconv.ParseBool(raw)
		if err != nil {
			return viewJSONError(access, http.StatusBadRequest, "invalid", "include_official must be true or false")
		}
		includeOfficial = !access.APIKey && value
	}
	prices, err := a.store.ModelPriceRowsWithOfficial(models, includeCustom, includeOfficial)
	if err != nil {
		return viewErrorResponse(access, err)
	}
	return viewJSON(access, http.StatusOK, prices)
}

func (a *App) putPrices(req ManagementRequest) ManagementResponse {
	var price billing.CustomPrice
	if errDecode := decodeStrict(req.Body, &price); errDecode != nil {
		return errorResponse(errDecode)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(req.Body, &fields); err != nil {
		return errorResponse(err)
	}
	if bytes.Equal(bytes.TrimSpace(fields["service_tiers"]), []byte("null")) {
		return JSONError(http.StatusBadRequest, "invalid", "service_tiers must be an object; use {} to clear it")
	}
	stored, errUpsert := a.store.UpsertPrice(price)
	if errUpsert != nil {
		return errorResponse(errUpsert)
	}
	return JSONResponse(http.StatusOK, map[string]any{"price": stored})
}

func (a *App) searchReferencePrices(req ManagementRequest) ManagementResponse {
	limit := 20
	if raw := strings.TrimSpace(req.Query.Get("limit")); raw != "" {
		parsed, errParse := strconv.Atoi(raw)
		if errParse != nil || parsed < 1 || parsed > 50 {
			return JSONError(http.StatusBadRequest, "invalid", "Limit must be an integer from 1 to 50")
		}
		limit = parsed
	}
	prices, err := a.store.SearchReferencePrices(req.Query.Get("q"), limit)
	if err != nil {
		return errorResponse(err)
	}
	return JSONResponse(http.StatusOK, map[string]any{"prices": prices})
}

func (a *App) refreshReferencePrices() ManagementResponse {
	result, errRefresh := a.store.RefreshReferencePrices()
	if errRefresh != nil {
		return errorResponse(errRefresh)
	}
	return JSONResponse(http.StatusOK, result)
}

func (a *App) refreshOfficialPrices(req ManagementRequest) ManagementResponse {
	var result billing.OfficialPriceRefreshResult
	var err error
	if a.hostCaller == nil {
		result, err = a.store.RefreshOfficialPrices()
	} else {
		result, err = a.store.RefreshOfficialPricesWithDownloader(func(ctx context.Context) ([]byte, error) {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			raw, err := a.hostCaller(hostHTTPDo, hostHTTPRequest{
				HostCallbackID: req.HostCallbackID, Method: http.MethodGet, URL: billing.OpenAIPricesURL,
				Headers: http.Header{"Accept": {"text/markdown"}, "User-Agent": {"cpa-key-billing/official-pricing"}},
			})
			if err != nil {
				return nil, fmt.Errorf("Download OpenAI official prices through CPA: %w", err)
			}
			var response hostHTTPResponse
			if err := json.Unmarshal(raw, &response); err != nil {
				return nil, fmt.Errorf("Parse official price download: %w", err)
			}
			if response.StatusCode != http.StatusOK {
				return nil, fmt.Errorf("OpenAI official pricing returned HTTP %d", response.StatusCode)
			}
			if len(response.Body) > 8<<20 {
				return nil, fmt.Errorf("OpenAI official pricing response is too large")
			}
			return response.Body, ctx.Err()
		})
	}
	if err != nil {
		return errorResponse(err)
	}
	return JSONResponse(http.StatusOK, result)
}

func (a *App) deletePrice(req ManagementRequest) ManagementResponse {
	if err := a.store.DeletePrice(req.Query.Get("model_id")); err != nil {
		return errorResponse(err)
	}
	return JSONResponse(200, map[string]any{"deleted": req.Query.Get("model_id")})
}

func (a *App) referencePriceStatus() ManagementResponse {
	return JSONResponse(200, map[string]any{"metadata": a.store.ReferencePriceMetadata()})
}
