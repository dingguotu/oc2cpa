package main

// filter_test.go covers the panel query filters (range / model / status)
// offline: canonicalization, calendar cutoffs, entry selection, and the
// filtered summary / records responses including endpoint dispatch.

import (
	"encoding/json"
	"net/http"
	"net/url"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

func mustRFC3339(t *testing.T, s string) string {
	t.Helper()
	_, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		t.Fatalf("bad test timestamp %q: %v", s, err)
	}
	return s
}

func TestUsageFilterFromQueryDefaults(t *testing.T) {
	f := usageFilterFromQuery(nil)
	if f.Range != usageRangeAll || f.Model != "" || f.Status != usageStatusAll {
		t.Fatalf("empty query must default to unfiltered: %+v", f)
	}
	f = usageFilterFromQuery(url.Values{
		"range":  {"7d"},
		"model":  {" glm-5.3-flash-free "},
		"status": {"failed"},
	})
	if f.Range != usageRange7d || f.Model != "glm-5.3-flash-free" || f.Status != usageStatusFailed {
		t.Fatalf("query decoding wrong: %+v", f)
	}
	// Unknown tokens fall back to the unfiltered defaults instead of erroring.
	f = usageFilterFromQuery(url.Values{"range": {"bogus"}, "status": {"bogus"}})
	if f.Range != usageRangeAll || f.Status != usageStatusAll {
		t.Fatalf("unknown tokens must canonicalize to defaults: %+v", f)
	}
}

func TestUsageFilterCutoffCalendarSemantics(t *testing.T) {
	now := time.Date(2026, 10, 27, 9, 30, 0, 0, time.Local)
	cases := []struct {
		rangeToken string
		wantDay    string
		bounded    bool
	}{
		{usageRangeToday, "2026-10-27", true},
		{usageRange7d, "2026-10-21", true},
		{usageRange30d, "2026-09-28", true},
		{usageRangeAll, "", false},
	}
	for _, c := range cases {
		cutoff, bounded := (usageFilter{Range: c.rangeToken}).cutoffTime(now)
		if bounded != c.bounded {
			t.Fatalf("range %s: bounded = %v", c.rangeToken, bounded)
		}
		if !bounded {
			continue
		}
		wantLocal := c.wantDay + " 00:00:00"
		if got := cutoff.Format("2006-01-02 15:04:05"); got != wantLocal {
			t.Fatalf("range %s cutoff = %s, want %s", c.rangeToken, got, wantLocal)
		}
		if cutoff.Location() != time.Local {
			t.Fatalf("range %s cutoff must stay local", c.rangeToken)
		}
	}
}

// seedUsageStore replaces the window with deterministic entries around now.
func seedUsageStore(t *testing.T, entries []usageEntry) time.Time {
	t.Helper()
	now := time.Now().In(time.Local)
	for i := range entries {
		if ts, err := time.Parse(time.RFC3339Nano, entries[i].Time); err == nil {
			entries[i].Time = ts.Format(time.RFC3339Nano)
			continue
		}
	}
	usageStoreMu.Lock()
	usageStore = entries
	usageStoreMu.Unlock()
	return now
}

func TestUsageFilterMatchesEntries(t *testing.T) {
	now := time.Now().In(time.Local)
	day := func(offsetDays int, hour int) string {
		day := now.AddDate(0, 0, -offsetDays)
		return time.Date(day.Year(), day.Month(), day.Day(), hour, 0, 0, 0, time.Local).Format(time.RFC3339Nano)
	}
	entries := []usageEntry{
		{Time: day(0, 8), Model: "glm-5.3-flash-free", Stream: true, TotalTokens: 100},
		{Time: day(0, 9), Model: "big-pickle", Alias: "GLM", Failed: true, Status: 500, TotalTokens: 5},
		{Time: day(3, 12), Model: "glm-5.3-flash-free", TotalTokens: 50},
		{Time: day(10, 12), Model: "big-pickle", TotalTokens: 20},
		{Time: day(40, 12), Model: "big-pickle", TotalTokens: 1},
	}
	reset := seedUsageStore(t, entries)
	defer seedUsageStore(t, nil)

	if got := len(filterUsageEntries(entries, usageFilter{Range: usageRangeAll, Status: usageStatusAll})); got != 5 {
		t.Fatalf("all range select = %d, want 5", got)
	}
	if got := len(filterUsageEntries(entries, usageFilter{Range: usageRangeToday, Status: usageStatusAll})); got != 2 {
		t.Fatalf("today range select = %d, want 2", got)
	}
	if got := len(filterUsageEntries(entries, usageFilter{Range: usageRange7d, Status: usageStatusAll})); got != 3 {
		t.Fatalf("7d range select = %d, want 3", got)
	}
	if got := len(filterUsageEntries(entries, usageFilter{Range: usageRange30d, Status: usageStatusAll})); got != 4 {
		t.Fatalf("30d range select = %d, want 4", got)
	}
	if got := len(filterUsageEntries(entries, usageFilter{Range: usageRangeAll, Model: "GLM"})); got != 1 {
		t.Fatalf("alias model match = %d, want 1", got)
	}
	if got := len(filterUsageEntries(entries, usageFilter{Range: usageRangeAll, Model: "big-pickle"})); got != 3 {
		t.Fatalf("model match = %d, want 3", got)
	}
	if got := len(filterUsageEntries(entries, usageFilter{Range: usageRangeAll, Status: usageStatusFailed})); got != 1 {
		t.Fatalf("failed-only select = %d, want 1", got)
	}
	if got := len(filterUsageEntries(entries, usageFilter{Range: usageRange7d, Status: usageStatusOK})); got != 2 {
		t.Fatalf("7d ok select = %d, want 2", got)
	}
	_ = mustRFC3339 // keep helper referenced
	_ = reset
}

func TestBuildUsageSummaryFiltered(t *testing.T) {
	now := time.Now().In(time.Local)
	day := func(offsetDays int, hour int) string {
		day := now.AddDate(0, 0, -offsetDays)
		return time.Date(day.Year(), day.Month(), day.Day(), hour, 0, 0, 0, time.Local).Format(time.RFC3339Nano)
	}
	seedUsageStore(t, []usageEntry{
		{Time: day(0, 8), Model: "glm-5.3-flash-free", Stream: true, TotalTokens: 100},
		{Time: day(3, 12), Model: "big-pickle", TotalTokens: 50},
		{Time: day(40, 12), Model: "big-pickle", Failed: true, Status: 500, TotalTokens: 1},
	})

	summary := buildUsageSummary(usageFilter{Range: usageRange7d, Status: usageStatusAll})
	if summary.Filters.Range != usageRange7d || summary.Filters.Status != usageStatusAll {
		t.Fatalf("filters echo wrong: %+v", summary.Filters)
	}
	if summary.All.Requests != 2 || summary.All.TotalTokens != 150 {
		t.Fatalf("7d all bucket wrong: %+v", summary.All)
	}
	if summary.Today.Requests != 1 || summary.Today.TotalTokens != 100 {
		t.Fatalf("7d today bucket wrong: %+v", summary.Today)
	}
	if summary.Records != 3 || summary.WindowCap != usageStoreCapacity {
		t.Fatalf("window meter must reflect the full store: records=%d", summary.Records)
	}
	if summary.Streaming != 1 || summary.NonStreaming != 1 {
		t.Fatalf("streaming split wrong: %+v", summary)
	}
	if len(summary.Models) != 2 {
		t.Fatalf("model rows = %d, want 2", len(summary.Models))
	}
	if len(summary.ModelOptions) != 2 {
		t.Fatalf("model options must cover the full window: %+v", summary.ModelOptions)
	}

	failed := buildUsageSummary(usageFilter{Range: usageRangeAll, Status: usageStatusFailed})
	if failed.All.Requests != 1 || failed.Models[0].Model != "big-pickle" {
		t.Fatalf("failed-only summary wrong: %+v", failed)
	}
	plain := buildUsageSummary(usageFilterFromQuery(nil))
	if plain.All.Requests != 3 {
		t.Fatalf("unfiltered summary must cover the window: %+v", plain.All)
	}
}

func TestBuildUsageRecordsFiltered(t *testing.T) {
	now := time.Now().In(time.Local)
	day := func(offsetDays int, hour int) string {
		day := now.AddDate(0, 0, -offsetDays)
		return time.Date(day.Year(), day.Month(), day.Day(), hour, 0, 0, 0, time.Local).Format(time.RFC3339Nano)
	}
	t0 := day(0, 8)
	t1 := day(0, 9)
	t2 := day(1, 10)
	seedUsageStore(t, []usageEntry{
		{Time: t0, Model: "glm-5.3-flash-free", TotalTokens: 1},
		{Time: t1, Model: "glm-5.3-flash-free", Failed: true, Status: 429, TotalTokens: 2},
		{Time: t2, Model: "big-pickle", TotalTokens: 3},
	})

	records := buildUsageRecords(usageFilter{Range: usageRangeAll, Status: usageStatusAll}, 2)
	if records.Total != 3 || records.Count != 2 {
		t.Fatalf("records total/count wrong: %+v", records)
	}
	if len(records.Records) != 2 || records.Records[0].Time != t1 || records.Records[1].Time != t2 {
		t.Fatalf("records must be the newest matches, oldest first: %+v", records)
	}

	failed := buildUsageRecords(usageFilter{Range: usageRangeAll, Status: usageStatusFailed}, 10)
	if failed.Total != 1 || failed.Records[0].Status != 429 {
		t.Fatalf("failed-only records wrong: %+v", failed)
	}

	model := buildUsageRecords(usageFilter{Range: usageRangeAll, Model: "big-pickle"}, 10)
	if model.Total != 1 || model.Records[0].Model != "big-pickle" {
		t.Fatalf("model-filtered records wrong: %+v", model)
	}
}

func TestManagementEndpointsFilterParams(t *testing.T) {
	now := time.Now().In(time.Local)
	day := func(offsetDays int, hour int) string {
		day := now.AddDate(0, 0, -offsetDays)
		return time.Date(day.Year(), day.Month(), day.Day(), hour, 0, 0, 0, time.Local).Format(time.RFC3339Nano)
	}
	seedUsageStore(t, []usageEntry{
		{Time: day(0, 8), Model: "glm-5.3-flash-free", TotalTokens: 10},
		{Time: day(3, 8), Model: "big-pickle", Failed: true, Status: 502, TotalTokens: 4},
	})

	req, _ := json.Marshal(pluginapi.ManagementRequest{
		Method: http.MethodGet,
		Path:   "/v0/management/plugins/" + pluginRouteID + "/usage/summary",
		Query:  map[string][]string{"range": {"7d"}, "status": {"failed"}},
	})
	resp, err := handleManagement(req)
	if err != nil {
		t.Fatalf("summary endpoint: %v", err)
	}
	var summary usageSummary
	if err := json.Unmarshal(resp.Body, &summary); err != nil {
		t.Fatalf("decode summary: %v", err)
	}
	if summary.All.Requests != 1 || summary.All.TotalTokens != 4 {
		t.Fatalf("failed-range summary wrong: %+v", summary.All)
	}
	if summary.Filters.Range != usageRange7d || summary.Filters.Status != usageStatusFailed {
		t.Fatalf("filters echo not reported: %+v", summary.Filters)
	}
	if len(summary.ModelOptions) != 2 {
		t.Fatalf("model options must list the full window: %+v", summary.ModelOptions)
	}

	req, _ = json.Marshal(pluginapi.ManagementRequest{
		Method: http.MethodGet,
		Path:   "/v0/management/plugins/" + pluginRouteID + "/usage/records",
		Query:  map[string][]string{"model": {"glm-5.3-flash-free"}},
	})
	resp, err = handleManagement(req)
	if err != nil {
		t.Fatalf("records endpoint: %v", err)
	}
	var records usageRecordsResponse
	if err := json.Unmarshal(resp.Body, &records); err != nil {
		t.Fatalf("decode records: %v", err)
	}
	if records.Total != 1 || len(records.Records) != 1 || records.Records[0].TotalTokens != 10 {
		t.Fatalf("model-filtered records wrong: %+v", records)
	}
}
