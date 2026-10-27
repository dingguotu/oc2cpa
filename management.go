package main

// management.go implements the ManagementPlugin capability: two JSON data
// endpoints for the usage panel plus the embedded HTML dashboard, following
// the CLIProxyAPI plugin management contract
// (management.register / management.handle, relative paths resolved under
// /v0/management and /v0/resource/plugins/<pluginID>).

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// pluginRouteID is the plugin id used in management route paths. It must stay
// in sync with the provider identity (host config key and library file name).
const pluginRouteID = "oc2cpa"

var (
	managementBaseMu sync.RWMutex
	managementBase   = "/v0/management"
	resourceBase     atomic.Value // holds string
)

func setManagementBasePath(p string) {
	p = strings.TrimRight(strings.TrimSpace(p), "/")
	if p == "" {
		return
	}
	managementBaseMu.Lock()
	defer managementBaseMu.Unlock()
	managementBase = p
}

func loadedManagementBasePath() string {
	managementBaseMu.RLock()
	defer managementBaseMu.RUnlock()
	return managementBase
}

// loadedResourceBasePath returns the resource prefix the host assigned at
// registration; requests are matched against the full resolved path.
func loadedResourceBasePath() string {
	if v, ok := resourceBase.Load().(string); ok && v != "" {
		return strings.TrimRight(v, "/")
	}
	return "/v0/resource/plugins/" + pluginRouteID
}

// pluginManagementRoute mirrors pluginapi.ManagementRoute on the wire: the
// register result decodes routes/resources by lowercase json tags, while the
// inner fields decode by Go field names.
type pluginManagementRoute struct {
	Method      string
	Path        string
	Description string
}

type pluginManagementResource struct {
	Path        string
	Menu        string
	Description string
}

type managementRegistrationPayload struct {
	Routes    []pluginManagementRoute    `json:"routes,omitempty"`
	Resources []pluginManagementResource `json:"resources,omitempty"`
}

// handleManagementRoutes builds the management.register result: two JSON data
// routes and the panel resource. It also caches the host-provided prefixes so
// management.handle matches the exact paths the host will forward.
func handleManagementRoutes(requestJSON []byte) (managementRegistrationPayload, error) {
	var req pluginapi.ManagementRegistrationRequest
	if err := json.Unmarshal(requestJSON, &req); err == nil {
		setManagementBasePath(req.BasePath)
	}
	if strings.TrimSpace(req.ResourceBasePath) != "" {
		resourceBase.Store(strings.TrimRight(strings.TrimSpace(req.ResourceBasePath), "/"))
	}
	base := "/plugins/" + pluginRouteID
	payload := managementRegistrationPayload{
		Routes: []pluginManagementRoute{
			{Method: http.MethodGet, Path: base + "/usage/summary", Description: "Aggregate request and token usage observed by this plugin process. Query: range (today | 7d | 30d | all, default all), model (model id or alias), status (ok | failed | all)."},
			{Method: http.MethodGet, Path: base + "/usage/records", Description: fmt.Sprintf("Per-request records (model, latency, tokens, failure status), newest matches first. Query: range, model, status as in /usage/summary; limit (default 100, max %d).", usageStoreCapacity)},
		},
		Resources: []pluginManagementResource{
			{Path: "/panel", Menu: "OC2CPA", Description: "OC2CPA usage dashboard: filterable request counts, token consumption, per-model stats and the recent request log."},
		},
	}
	return payload, nil
}

func mgmtJSONResponse(status int, payload any) pluginapi.ManagementResponse {
	body, _ := json.Marshal(payload)
	return pluginapi.ManagementResponse{
		StatusCode: status,
		Headers:    http.Header{"Content-Type": {"application/json; charset=utf-8"}},
		Body:       body,
	}
}

func mgmtHTMLResponse(body []byte) pluginapi.ManagementResponse {
	return pluginapi.ManagementResponse{
		StatusCode: http.StatusOK,
		Headers:    http.Header{"Content-Type": {"text/html; charset=utf-8"}},
		Body:       body,
	}
}

// panelHTML returns the embedded dashboard page (fresh copy each call so a
// browser cannot mutate our source slice).
func panelHTML() []byte {
	out := make([]byte, len(panelHTMLRaw))
	copy(out, panelHTMLRaw)
	return out
}

//go:embed panel.html
var panelHTMLRaw []byte

// handleManagement routes one host-forwarded Management API or resource
// request. The host middleware validates the management key before us for
// /v0/management paths; resource pages are host-unauthenticated static pages.
func handleManagement(requestJSON []byte) (pluginapi.ManagementResponse, error) {
	var req pluginapi.ManagementRequest
	if err := json.Unmarshal(requestJSON, &req); err != nil {
		return mgmtJSONResponse(http.StatusBadRequest, map[string]string{"error": "invalid management request: " + err.Error()}), nil
	}
	method := strings.ToUpper(strings.TrimSpace(req.Method))
	path := strings.TrimRight(req.Path, "/")

	// Browser panel (resource, unauthenticated by design).
	if method == http.MethodGet && strings.HasPrefix(path, loadedResourceBasePath()) {
		sub := strings.TrimPrefix(path, loadedResourceBasePath())
		switch sub {
		case "", "/", "/panel", "/panel.html", "/index.html":
			return mgmtHTMLResponse(panelHTML()), nil
		default:
			return mgmtJSONResponse(http.StatusNotFound, map[string]string{"error": "unknown resource: " + sub}), nil
		}
	}

	base := loadedManagementBasePath() + "/plugins/" + pluginRouteID
	switch {
	case method == http.MethodGet && path == base+"/usage/summary":
		return mgmtJSONResponse(http.StatusOK, buildUsageSummary(usageFilterFromQuery(req.Query))), nil
	case method == http.MethodGet && path == base+"/usage/records":
		limit := 100
		if raw := strings.TrimSpace(req.Query.Get("limit")); raw != "" {
			if n, errParse := strconv.Atoi(raw); errParse == nil && n > 0 {
				limit = n
			}
		}
		return mgmtJSONResponse(http.StatusOK, buildUsageRecords(usageFilterFromQuery(req.Query), limit)), nil
	default:
		return mgmtJSONResponse(http.StatusNotFound, map[string]string{
			"error": "unknown management route: " + method + " " + path,
		}), nil
	}
}
