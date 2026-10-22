package main

import (
	"encoding/json"
	"io"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// modelDefaults carries the limits spot-verified against the free models in
// opencode README (data source: https://models.opencode.ai/api.json, the "limit"
// and "modalities" fields of each model).
type modelDefaults struct {
	ContextLength   int64
	MaxTokens       int64
	InputModalities []string
}

var knownModels = map[string]modelDefaults{
	"big-pickle":               {ContextLength: 200_000, MaxTokens: 32_000, InputModalities: []string{"text"}},
	"mimo-v2.6-flash-free":     {ContextLength: 200_000, MaxTokens: 32_000, InputModalities: []string{"text", "image"}},
	"deepseek-v4.1-flash-free": {ContextLength: 200_000, MaxTokens: 32_000, InputModalities: []string{"text", "image"}},
	"glm-5.3-flash-free":       {ContextLength: 200_000, MaxTokens: 32_000, InputModalities: []string{"text", "image"}},
}

// staticCatalogOrder is the display order of the built-in catalog (README order).
var staticCatalogOrder = []string{
	"big-pickle",
	"mimo-v2.6-flash-free",
	"deepseek-v4.1-flash-free",
	"glm-5.3-flash-free",
}

// isAllowedModelID mirrors the original server's whitelist: the fixed model
// big-pickle plus every model whose id ends with -free.
func isAllowedModelID(id string) bool {
	return id == "big-pickle" || strings.HasSuffix(id, "-free")
}

func buildModelInfo(id string) pluginapi.ModelInfo {
	info := pluginapi.ModelInfo{
		ID:                         id,
		Object:                     "model",
		DisplayName:                id,
		Name:                       id,
		OwnedBy:                    providerName,
		SupportedGenerationMethods: []string{"chat"},
	}
	// Livestream limits default to the free-tier envelope (README-verified for
	// every current free model) so discovered-but-unknown ids stay usable.
	info.ContextLength = 200_000
	info.MaxCompletionTokens = 32_000
	info.SupportedInputModalities = []string{"text", "image"}
	if defaults, ok := knownModels[id]; ok {
		info.ContextLength = defaults.ContextLength
		info.MaxCompletionTokens = defaults.MaxTokens
		info.SupportedInputModalities = defaults.InputModalities
	}
	info.SupportedOutputModalities = []string{"text"}
	return info
}

func filterExcluded(models []pluginapi.ModelInfo, host pluginapi.HostConfigSummary) []pluginapi.ModelInfo {
	if len(host.ExcludedModels) == 0 {
		return models
	}
	excluded := make(map[string]bool)
	for _, name := range host.ExcludedModels[providerName] {
		excluded[name] = true
	}
	if len(excluded) == 0 {
		return models
	}
	out := models[:0:0]
	for _, model := range models {
		if excluded[model.ID] {
			continue
		}
		out = append(out, model)
	}
	return out
}

var modelCacheMu sync.Mutex
var modelCacheTTL = 5 * time.Minute
var modelCacheTime time.Time
var modelCache []pluginapi.ModelInfo

// hostProxyMu/…HostProxy cache the host's globally configured upstream proxy
// (HostConfigSummary.ProxyURL). The executor RPC requests do not carry it, but
// every model request does, and the host applies the same proxy to its own
// upstream traffic — so it belongs here as the fallback when the auth record
// has no proxy_url. Reconfigure re-runs model requests, keeping it fresh.
var hostProxyMu sync.Mutex
var hostProxyURL string

// noteHostProxy records the host global proxy from a HostConfigSummary.
func noteHostProxy(host pluginapi.HostConfigSummary) {
	proxy := strings.TrimSpace(host.ProxyURL)
	hostProxyMu.Lock()
	defer hostProxyMu.Unlock()
	hostProxyURL = proxy
}

// readHostProxy returns the captured host global proxy (may be empty).
func readHostProxy() string {
	hostProxyMu.Lock()
	defer hostProxyMu.Unlock()
	return hostProxyURL
}

func invalidateModelCache() {
	modelCacheMu.Lock()
	defer modelCacheMu.Unlock()
	modelCacheTime = time.Time{}
	modelCache = nil
}

// handleStaticModels answers model.static with the built-in catalog, except when
// the host forces model prefixes. In that mode the host registers static models
// verbatim while the per-auth catalog is published with the credential prefix,
// so answering here would expose the same models twice and add unprefixed ids
// that routing rejects. The catalog stays available through model.for_auth.
func handleStaticModels(requestJSON []byte) pluginapi.ModelResponse {
	var req pluginapi.StaticModelRequest
	if len(requestJSON) > 0 {
		_ = json.Unmarshal(requestJSON, &req)
	}
	noteHostProxy(req.Host)
	noteHostAuthDir(req.Host.AuthDir)
	bootstrapAuthOnce()
	if req.Host.ForceModelPrefix {
		return pluginapi.ModelResponse{Provider: providerName}
	}
	models := staticCatalog()
	return pluginapi.ModelResponse{Provider: providerName, Models: filterExcluded(models, req.Host)}
}

// handleModelsForAuth answers model.for_auth; it merges upstream discovery when
// dynamic_models is enabled, otherwise it returns the static catalog.
func handleModelsForAuth(requestJSON []byte) pluginapi.ModelResponse {
	var req pluginapi.AuthModelRequest
	if len(requestJSON) > 0 {
		_ = json.Unmarshal(requestJSON, &req)
	}
	noteHostProxy(req.Host)
	models := staticCatalog()
	if readSettings().DynamicModels {
		models = mergeCatalog(models, discoveredModels())
	}
	// The Provider field must always be this plugin's canonical key; echoing the
	// requested provider-key variant makes the host drop the whole list.
	return pluginapi.ModelResponse{Provider: providerName, Models: filterExcluded(models, req.Host)}
}

func staticCatalog() []pluginapi.ModelInfo {
	out := make([]pluginapi.ModelInfo, 0, len(staticCatalogOrder))
	for _, id := range staticCatalogOrder {
		out = append(out, buildModelInfo(id))
	}
	return out
}

func mergeCatalog(base []pluginapi.ModelInfo, discovered []pluginapi.ModelInfo) []pluginapi.ModelInfo {
	seen := make(map[string]bool, len(base)+len(discovered))
	for _, model := range base {
		seen[model.ID] = true
	}
	out := base
	for _, model := range discovered {
		if seen[model.ID] {
			continue
		}
		seen[model.ID] = true
		out = append(out, model)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// discoveredModels lists upstream models filtered by the free-tier whitelist,
// cached for modelCacheTTL. Failures keep the previous discovery or fall back to
// the static catalog.
func discoveredModels() []pluginapi.ModelInfo {
	modelCacheMu.Lock()
	defer modelCacheMu.Unlock()
	if len(modelCache) > 0 && time.Since(modelCacheTime) < modelCacheTTL {
		return modelCache
	}
	body, err := fetchUpstreamModels()
	if err != nil || len(body) == 0 {
		return modelCache
	}
	var parsed struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return modelCache
	}
	out := make([]pluginapi.ModelInfo, 0, len(parsed.Data))
	for _, item := range parsed.Data {
		if !isAllowedModelID(item.ID) {
			continue
		}
		out = append(out, buildModelInfo(item.ID))
	}
	if len(out) == 0 {
		return modelCache
	}
	modelCache = out
	modelCacheTime = time.Now()
	return out
}

func fetchUpstreamModels() ([]byte, error) {
	modelsURL := readSettings().UpstreamBaseURL + "/models"
	request, err := newUpstreamModelsRequest(modelsURL)
	if err != nil {
		return nil, err
	}

	response, err := sharedHTTPClient().Do(request)
	if err != nil {
		return nil, err
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		message := readErrorText(response.Body, modelDiscoveryCtx(), response.StatusCode, "", &textDecoder{})
		return nil, mapUpstreamFailure(response.StatusCode, message).asError()
	}
	body, err := io.ReadAll(response.Body)
	if err != nil {
		return nil, err
	}
	return body, nil
}

// modelDiscoveryCtx satisfies the shared error-body reader's logging needs.
func modelDiscoveryCtx() *streamCtx {
	return &streamCtx{requestID: "model-discovery", model: "model-list"}
}
