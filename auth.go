package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// ocAuthView is the subset of the selected auth the executor needs. A
// synthetic auth file looks like:
//
//	{"type":"oc2cpa","label":"OpenCode free (anonymous)","proxy_url":"http://user:pass@host:port"}
type ocAuthView struct {
	Holder   string
	ProxyURL string
}

func parseAuthView(storageJSON []byte) ocAuthView {
	view := ocAuthView{Holder: "anonymous"}
	if len(storageJSON) == 0 {
		return view
	}
	var raw map[string]any
	if err := json.Unmarshal(storageJSON, &raw); err != nil {
		return view
	}
	if proxy, ok := raw["proxy_url"].(string); ok && proxy != "" {
		view.ProxyURL = proxy
	}
	return view
}

// handleParseAuth claims auth files typed as this provider. As with well-known
// CPA plugin providers, the plugin leaves Auth.ID empty and echoes FileName so
// the host performs its own per-file upsert and never duplicates records.
func handleParseAuth(requestJSON []byte) pluginapi.AuthParseResponse {
	var req pluginapi.AuthParseRequest
	if err := json.Unmarshal(requestJSON, &req); err != nil {
		return pluginapi.AuthParseResponse{Handled: false}
	}
	var raw map[string]any
	if err := json.Unmarshal(req.RawJSON, &raw); err != nil {
		return pluginapi.AuthParseResponse{Handled: false}
	}
	declaredType, _ := raw["type"].(string)
	if declaredType != providerName {
		return pluginapi.AuthParseResponse{Handled: false}
	}
	storageJSON, _ := json.Marshal(raw)
	return pluginapi.AuthParseResponse{
		Handled: true,
		Auth: pluginapi.AuthData{
			Provider:    providerName,
			FileName:    req.FileName,
			Label:       authLabel(raw),
			Prefix:      authPrefix(raw),
			StorageJSON: storageJSON,
			Metadata:    authMetadata(raw),
		},
	}
}

func authLabel(raw map[string]any) string {
	if label, ok := raw["label"].(string); ok && label != "" {
		return label
	}
	return "OpenCode free models (anonymous)"
}

// authMetadata is what the host persists back into the auth file; anything left
// out here is dropped when the host rewrites the file after a refresh.
func authMetadata(raw map[string]any) map[string]any {
	meta := map[string]any{
		"type":     providerName,
		"provider": providerName,
	}
	if prefix := authPrefix(raw); prefix != "" {
		meta["prefix"] = prefix
	}
	return meta
}

// authPrefix carries the optional model prefix from the auth file. The host
// rewrites the file from every AuthData it receives, so both parse and refresh
// must echo it or the prefix is lost on the first refresh cycle.
func authPrefix(raw map[string]any) string {
	if prefix, ok := raw["prefix"].(string); ok {
		return strings.TrimSpace(prefix)
	}
	return ""
}

// handleRefreshAuth keeps the auth permanently valid: the free tier uses no
// expiring credentials, so the refresh call echoes the stored payload back
// without scheduling another refresh cycle.
func handleRefreshAuth(requestJSON []byte) pluginapi.AuthRefreshResponse {
	var req pluginapi.AuthRefreshRequest
	if err := json.Unmarshal(requestJSON, &req); err != nil {
		return pluginapi.AuthRefreshResponse{}
	}
	var raw map[string]any
	_ = json.Unmarshal(req.StorageJSON, &raw)
	return pluginapi.AuthRefreshResponse{
		Auth: pluginapi.AuthData{
			Provider:    providerName,
			Prefix:      authPrefix(raw),
			StorageJSON: req.StorageJSON,
			Metadata:    authMetadata(raw),
		},
	}
}

// stripAuthPrefix removes the credential's model prefix from a requested id. The
// host publishes aliases as "prefix/model" but the upstream only knows the bare
// model name, so forwarding the routed id verbatim is rejected as unavailable.
func stripAuthPrefix(model string, req pluginapi.ExecutorRequest) string {
	prefix := ""
	if raw, ok := req.AuthMetadata["prefix"].(string); ok {
		prefix = strings.TrimSpace(raw)
	}
	if prefix == "" {
		var raw map[string]any
		if json.Unmarshal(req.StorageJSON, &raw) == nil {
			prefix = authPrefix(raw)
		}
	}
	prefix = strings.TrimSuffix(prefix, "/")
	if prefix == "" {
		return model
	}
	if trimmed, ok := strings.CutPrefix(model, prefix+"/"); ok {
		return trimmed
	}
	return model
}

// hostAuthListEntry is the wire payload for host.auth.list. The plugin only
// needs the file names and provider fields for bootstrap detection.
type hostAuthListResponse struct {
	Files []pluginapi.HostAuthFileEntry `json:"files"`
}

// authFileName is the credential file the plugin bootstraps into auth-dir.
const authFileName = providerName + ".json"

var (
	hostAuthDirMu sync.RWMutex
	hostAuthDir   string
)

// noteHostAuthDir records the auth-dir the host publishes with model requests.
func noteHostAuthDir(dir string) {
	dir = strings.TrimSpace(dir)
	if dir == "" {
		return
	}
	hostAuthDirMu.Lock()
	hostAuthDir = dir
	hostAuthDirMu.Unlock()
}

func currentHostAuthDir() string {
	hostAuthDirMu.RLock()
	defer hostAuthDirMu.RUnlock()
	return hostAuthDir
}

// bootstrapAuthOnce creates the synthetic auth file when no oc2cpa credential
// exists yet. It only writes on a confirmed absence: host.auth.save replaces the
// file wholesale, and the host answers host.auth.list before it has loaded the
// auth directory, so an empty answer must not be read as "file missing" — that
// would drop the fields the host persists but the plugin does not echo, such as
// prefix and excluded_models.
func bootstrapAuthOnce() {
	if oc2cpaAuthState() == authAbsent {
		createBootstrapAuthFile()
	}
}

type authFileState int

const (
	authUnknown authFileState = iota
	authPresent
	authAbsent
)

// oc2cpaAuthState inspects the auth dir the host publishes with every model
// request. That directory is authoritative; host.auth.list is only a fallback,
// and stays empty until the host has finished loading credentials.
func oc2cpaAuthState() authFileState {
	if dir := currentHostAuthDir(); dir != "" {
		_, err := os.Stat(filepath.Join(dir, authFileName))
		switch {
		case err == nil:
			return authPresent
		case os.IsNotExist(err):
			return authAbsent
		default:
			// Unreadable location: assume a file is there rather than replace it.
			return authUnknown
		}
	}
	responseBytes, err := goHostCall(pluginabi.MethodHostAuthList, []byte("{}"))
	if err != nil {
		return authUnknown
	}
	var list hostAuthListResponse
	if err := json.Unmarshal(responseBytes, &list); err != nil {
		return authUnknown
	}
	if len(list.Files) == 0 {
		return authUnknown
	}
	for _, file := range list.Files {
		if file.Name == authFileName || file.Provider == providerName || file.Type == providerName {
			return authPresent
		}
	}
	return authAbsent
}

func createBootstrapAuthFile() {
	payload, _ := json.Marshal(map[string]any{
		"type":      providerName,
		"provider":  providerName,
		"label":     "OpenCode free models (anonymous)",
		"proxy_url": "",
	})
	if _, err := goHostCallJson(pluginabi.MethodHostAuthSave, pluginapi.HostAuthSaveRequest{
		Name: providerName + ".json",
		JSON: json.RawMessage(payload),
	}); err != nil {
		fsDebugLog("auto-bootstrap oc2cpa.json auth file failed: " + err.Error())
	}
}
