package main

// usage.go implements the UsagePlugin capability (see sdk/pluginabi
// MethodUsageHandle). After every request the host pushes the canonical
// pluginapi.UsageRecord here — it already persists the token counters into its
// own usage manager for the management UI, so this plugin only emits one
// compact forwarding-log line (model, latency, tokens, failure) to stderr,
// which the host captures into its log stream. The record shape mirrors the
// host's wire format: Go field names, nested Detail and Failure objects.

import (
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// usageLogLine is the stable marker that lets users grep forwarding logs out
// of the host log stream.
const usageLogLine = "[oc2cpa-usage]"

// usageStoreCapacity bounds the in-memory record window backing the usage
// panel: the newest entries are kept, older ones are dropped. Records live
// only for the host process lifetime; durable statistics stay in the host's
// own usage database / management UI. A larger window keeps time-range
// filters (today / 7d / 30d) useful across long host sessions.
const usageStoreCapacity = 5000

// usageRange / usageStatus tokens accepted on the management endpoints.
const (
	usageRangeToday = "today"
	usageRange7d    = "7d"
	usageRange30d   = "30d"
	usageRangeAll   = "all"

	usageStatusAll    = "all"
	usageStatusOK     = "ok"
	usageStatusFailed = "failed"
)

// usageFilter is the decoded panel query filter (range, model, status).
type usageFilter struct {
	Range  string
	Model  string
	Status string
}

// usageFilterFromQuery decodes range / model / status query parameters with
// test-friendly defaults: no parameters means the unfiltered window.
func usageFilterFromQuery(q url.Values) usageFilter {
	f := usageFilter{Range: usageRangeAll, Status: usageStatusAll}
	if q != nil {
		f.Range = canonicalUsageRange(q.Get("range"))
		f.Model = strings.TrimSpace(q.Get("model"))
		f.Status = canonicalUsageStatus(q.Get("status"))
	}
	return f
}

func canonicalUsageRange(raw string) string {
	switch strings.TrimSpace(raw) {
	case usageRangeToday:
		return usageRangeToday
	case usageRange7d:
		return usageRange7d
	case usageRange30d:
		return usageRange30d
	default:
		return usageRangeAll
	}
}

func canonicalUsageStatus(raw string) string {
	switch strings.TrimSpace(raw) {
	case usageStatusOK:
		return usageStatusOK
	case usageStatusFailed:
		return usageStatusFailed
	default:
		return usageStatusAll
	}
}

// cutoffTime returns the lower bound of the selected range in local time.
// Calendar semantics: "today" starts at local midnight; "7d"/"30d" include
// that many whole calendar days. The boolean reports whether a bound exists.
func (f usageFilter) cutoffTime(now time.Time) (time.Time, bool) {
	now = now.In(time.Local)
	switch f.Range {
	case usageRangeToday:
		y, m, d := now.Date()
		return time.Date(y, m, d, 0, 0, 0, 0, time.Local), true
	case usageRange7d, usageRange30d:
		days := 6
		if f.Range == usageRange30d {
			days = 29
		}
		y, m, d := now.AddDate(0, 0, -days).Date()
		return time.Date(y, m, d, 0, 0, 0, 0, time.Local), true
	default:
		return time.Time{}, false
	}
}

// matches applies the filter to one stored entry. The model filter matches
// either the upstream model id or the client-facing alias.
func (f usageFilter) matches(entry usageEntry, cutoff time.Time, bounded bool) bool {
	if bounded {
		ts, err := time.Parse(time.RFC3339Nano, entry.Time)
		if err != nil || !ts.After(cutoff) {
			return false
		}
	}
	if f.Model != "" && entry.Model != f.Model && entry.Alias != f.Model {
		return false
	}
	switch f.Status {
	case usageStatusOK:
		if entry.Failed {
			return false
		}
	case usageStatusFailed:
		if !entry.Failed {
			return false
		}
	}
	return true
}

// filterUsageEntries selects the entries covered by the filter.
func filterUsageEntries(entries []usageEntry, f usageFilter) []usageEntry {
	cutoff, bounded := f.cutoffTime(time.Now())
	out := make([]usageEntry, 0, len(entries))
	for _, entry := range entries {
		if f.matches(entry, cutoff, bounded) {
			out = append(out, entry)
		}
	}
	return out
}

// usageEntry is one stored request for the usage panel. Lowercase json tags
// are the panel's API contract.
type usageEntry struct {
	Time         string `json:"time"`
	RequestID    string `json:"request_id,omitempty"`
	AuthID       string `json:"auth_id,omitempty"`
	AuthType     string `json:"auth_type,omitempty"`
	APIKey       string `json:"api_key,omitempty"`
	Model        string `json:"model"`
	Alias        string `json:"alias,omitempty"`
	Stream       bool   `json:"stream"`
	Failed       bool   `json:"failed"`
	Status       int    `json:"status,omitempty"`
	LatencyMS    int64  `json:"latency_ms"`
	TTFTMS       int64  `json:"ttft_ms,omitempty"`
	InputTokens  int64  `json:"input_tokens"`
	OutputTokens int64  `json:"output_tokens"`
	Reasoning    int64  `json:"reasoning_tokens,omitempty"`
	Cached       int64  `json:"cached_tokens,omitempty"`
	TotalTokens  int64  `json:"total_tokens"`
}

var usageStoreMu sync.Mutex
var usageStore []usageEntry

// usageLogEvent is the JSON shape of one forwarding-log line. Lowercase keys
// keep the console output human-friendly; counters are always present so
// log scrapers can rely on the schema.
type usageLogEvent struct {
	Time          string `json:"time"`
	RequestID     string `json:"request_id,omitempty"`
	Provider      string `json:"provider"`
	Model         string `json:"model"`
	ResponseModel string `json:"response_model,omitempty"`
	AuthType      string `json:"auth_type,omitempty"`
	APIKey        string `json:"api_key,omitempty"`
	Stream        bool   `json:"stream"`
	Failed        bool   `json:"failed"`
	Status        int    `json:"status,omitempty"`
	LatencyMS     int64  `json:"latency_ms"`
	TTFTMS        int64  `json:"ttft_ms,omitempty"`
	InputTokens   int64  `json:"input_tokens"`
	OutputTokens  int64  `json:"output_tokens"`
	Reasoning     int64  `json:"reasoning_tokens,omitempty"`
	Cached        int64  `json:"cached_tokens,omitempty"`
	TotalTokens   int64  `json:"total_tokens"`
}

// handleUsage renders and prints the forwarding-log line for one record.
// Logging is strictly best-effort: a malformed record or a disabled switch
// simply no-ops, because the host logs a plugin fault if this RPC errors and
// usage telemetry must never break request handling.
func handleUsage(requestJSON []byte) error {
	var record pluginapi.UsageRecord
	if err := json.Unmarshal(requestJSON, &record); err != nil {
		return nil
	}
	// Only log this plugin's own records; the host routes other plugins' usage
	// to their owners, but defensive filtering keeps the log honest if a host
	// ever broadcasts globally.
	if record.Provider != "" && record.Provider != providerName {
		return nil
	}
	if !readSettings().UsageLog {
		return nil
	}
	entry := usageEntryFromRecord(record)
	usageStoreMu.Lock()
	usageStore = append(usageStore, entry)
	if len(usageStore) > usageStoreCapacity {
		usageStore = usageStore[len(usageStore)-usageStoreCapacity:]
	}
	usageStoreMu.Unlock()
	fmt.Fprintln(os.Stderr, usageLogLine+" "+formatUsageLog(record))
	return nil
}

// usageEntryFromRecord maps the host's canonical UsageRecord into the compact
// panel entry. client-sensitive identifiers are reduced the same way as the
// console log line.
func usageEntryFromRecord(record pluginapi.UsageRecord) usageEntry {
	entry := usageEntry{
		Time:         record.RequestedAt.Format(time.RFC3339Nano),
		RequestID:    record.RequestID,
		AuthID:       record.AuthID,
		AuthType:     record.AuthType,
		APIKey:       redactKey(record.APIKey),
		Model:        record.Model,
		Alias:        record.Alias,
		Stream:       record.Stream,
		Failed:       record.Failed,
		LatencyMS:    record.Latency.Milliseconds(),
		InputTokens:  record.Detail.InputTokens,
		OutputTokens: record.Detail.OutputTokens,
		Reasoning:    record.Detail.ReasoningTokens,
		Cached:       record.Detail.CachedTokens + record.Detail.CacheReadTokens,
		TotalTokens:  record.Detail.TotalTokens,
	}
	if record.TTFT > 0 {
		entry.TTFTMS = record.TTFT.Milliseconds()
	}
	if record.Failed {
		entry.Status = record.Failure.StatusCode
	}
	if entry.Time == "" || entry.Time == "0001-01-01T00:00:00Z" {
		entry.Time = time.Now().Format(time.RFC3339Nano)
	}
	return entry
}

// snapshotUsageEntries returns a copy of the newest entries (oldest first).
func snapshotUsageEntries() []usageEntry {
	usageStoreMu.Lock()
	defer usageStoreMu.Unlock()
	out := make([]usageEntry, len(usageStore))
	copy(out, usageStore)
	return out
}

// usageBucket accumulates token counters and request outcomes.
type usageBucket struct {
	Requests     int64 `json:"requests"`
	Success      int64 `json:"success"`
	Failed       int64 `json:"failed"`
	InputTokens  int64 `json:"input_tokens"`
	OutputTokens int64 `json:"output_tokens"`
	Reasoning    int64 `json:"reasoning_tokens,omitempty"`
	Cached       int64 `json:"cached_tokens,omitempty"`
	TotalTokens  int64 `json:"total_tokens"`
}

func (b *usageBucket) add(entry usageEntry) {
	b.Requests++
	if entry.Failed {
		b.Failed++
	} else {
		b.Success++
	}
	b.InputTokens += entry.InputTokens
	b.OutputTokens += entry.OutputTokens
	b.Reasoning += entry.Reasoning
	b.Cached += entry.Cached
	b.TotalTokens += entry.TotalTokens
}

// modelUsage is one per-model row of the usage summary.
type modelUsage struct {
	usageBucket
	Model      string `json:"model"`
	LastUsed   string `json:"last_used,omitempty"`
	LastStatus int    `json:"last_status,omitempty"`
}

// usageFilterEcho reports back the parsed filter so the panel can confirm
// what a response describes.
type usageFilterEcho struct {
	Range  string `json:"range"`
	Model  string `json:"model,omitempty"`
	Status string `json:"status"`
}

// usageSummary is the aggregated response of the management usage endpoints.
// Totals, streaming counts and per-model rows cover the currently selected
// filter, while Records / ModelOptions always describe the full window so the
// panel can render the occupancy meter and the model dropdown.
type usageSummary struct {
	GeneratedAt  string          `json:"generated_at"`
	Provider     string          `json:"provider"`
	WindowCap    int             `json:"window_cap"`
	Records      int             `json:"records"`
	Streaming    int64           `json:"streaming"`
	NonStreaming int64           `json:"non_streaming"`
	Today        usageBucket     `json:"today"`
	All          usageBucket     `json:"all"`
	Models       []modelUsage    `json:"models"`
	Filters      usageFilterEcho `json:"filters"`
	ModelOptions []string        `json:"model_options,omitempty"`
}

// buildUsageSummary aggregates the in-memory window under the requested
// filter: a filtered all-range bucket, a filtered local-today bucket, and
// filtered per-model rows. Durable totals remain the host's business; this
// summary describes what this process observed.
func buildUsageSummary(filter usageFilter) usageSummary {
	entries := snapshotUsageEntries()
	selected := filterUsageEntries(entries, filter)
	summary := usageSummary{
		GeneratedAt: time.Now().Format(time.RFC3339),
		Provider:    providerName,
		WindowCap:   usageStoreCapacity,
		Records:     len(entries),
		Models:      make([]modelUsage, 0),
		Filters: usageFilterEcho{
			Range:  filter.Range,
			Model:  filter.Model,
			Status: filter.Status,
		},
	}
	dayStart, bounded := usageFilter{Range: usageRangeToday}.cutoffTime(time.Now())
	byModel := make(map[string]*modelUsage)
	for _, entry := range entries {
		modelUsed := pickModelOption(entry)
		if !containsString(summary.ModelOptions, modelUsed) {
			summary.ModelOptions = append(summary.ModelOptions, modelUsed)
		}
	}
	sort.Strings(summary.ModelOptions)
	for _, entry := range selected {
		summary.All.add(entry)
		if entry.Stream {
			summary.Streaming++
		} else {
			summary.NonStreaming++
		}
		if ts, err := time.Parse(time.RFC3339Nano, entry.Time); err == nil && (!bounded || ts.After(dayStart)) {
			summary.Today.add(entry)
		}
		row := byModel[entry.Model]
		if row == nil {
			row = &modelUsage{}
			byModel[entry.Model] = row
		}
		row.add(entry)
		row.LastUsed = entry.Time
		row.LastStatus = entry.Status
	}
	for model, row := range byModel {
		row.Model = model
		summary.Models = append(summary.Models, *row)
	}
	sort.Slice(summary.Models, func(i, j int) bool {
		if summary.Models[i].Requests != summary.Models[j].Requests {
			return summary.Models[i].Requests > summary.Models[j].Requests
		}
		return summary.Models[i].Model < summary.Models[j].Model
	})
	return summary
}

// pickModelOption exposes the model id (with alias fallback) used in the
// panel's model dropdown.
func pickModelOption(entry usageEntry) string {
	if entry.Model != "" {
		return entry.Model
	}
	return entry.Alias
}

func containsString(list []string, s string) bool {
	for _, item := range list {
		if item == s {
			return true
		}
	}
	return false
}

// usageRecordsResponse is the raw record listing for the panel. Total reports
// how many entries match the filter in the whole window, Records carries at
// most Limit (oldest first among the newest matches).
type usageRecordsResponse struct {
	Records []usageEntry `json:"records"`
	Count   int          `json:"count"`
	Total   int          `json:"total"`
	Limit   int          `json:"limit"`
}

// buildUsageRecords applies the panel filter to the window and returns the
// newest matching entries, capped by limit.
func buildUsageRecords(filter usageFilter, limit int) usageRecordsResponse {
	if limit <= 0 {
		limit = 100
	}
	if limit > usageStoreCapacity {
		limit = usageStoreCapacity
	}
	selected := filterUsageEntries(snapshotUsageEntries(), filter)
	start := 0
	if limit < len(selected) {
		start = len(selected) - limit
	}
	shown := selected[start:]
	return usageRecordsResponse{
		Records: shown,
		Count:   len(shown),
		Total:   len(selected),
		Limit:   limit,
	}
}

// formatUsageLog renders one record as a compact single-line JSON event.
func formatUsageLog(record pluginapi.UsageRecord) string {
	event := usageLogEvent{
		Time:          time.Now().Format(time.RFC3339),
		RequestID:     record.RequestID,
		Provider:      record.Provider,
		Model:         record.Model,
		ResponseModel: record.ResponseModel,
		AuthType:      record.AuthType,
		APIKey:        redactKey(record.APIKey),
		Stream:        record.Stream,
		Failed:        record.Failed,
		LatencyMS:     record.Latency.Milliseconds(),
		InputTokens:   record.Detail.InputTokens,
		OutputTokens:  record.Detail.OutputTokens,
		Reasoning:     record.Detail.ReasoningTokens,
		Cached:        record.Detail.CachedTokens + record.Detail.CacheReadTokens,
		TotalTokens:   record.Detail.TotalTokens,
	}
	if record.TTFT > 0 {
		event.TTFTMS = record.TTFT.Milliseconds()
	}
	if record.Failed {
		event.Status = record.Failure.StatusCode
	}
	out, err := json.Marshal(event)
	if err != nil {
		return fmt.Sprintf(`{"provider":%q,"model":%q,"failed":%v}`,
			record.Provider, record.Model, record.Failed)
	}
	return string(out)
}

// redactKey keeps log scrapes from leaking client keys: only a short prefix
// of the API key identifier survives into the usage log.
func redactKey(key string) string {
	key = strings.TrimSpace(key)
	if key == "" {
		return ""
	}
	if len(key) <= 8 {
		return strings.Repeat("*", len(key))
	}
	return key[:8] + "..."
}
