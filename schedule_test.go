package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestLoadSessionMappingsUsesNamedColumnsAndWarnsForEmptyIDs(t *testing.T) {
	path := writeSessionCSV(t, "Session ID,ignored,議程 ID\nSESSION-A,value,meeting-a\n,value,meeting-b\n")
	mappings, warnings, err := loadSessionMappings(path)
	if err != nil {
		t.Fatalf("loadSessionMappings() error = %v", err)
	}
	if len(mappings) != 1 || mappings[0].meetingID != "meeting-a" || mappings[0].sessionID != "SESSION-A" {
		t.Fatalf("mappings = %#v, want named columns to map meeting-a", mappings)
	}
	if len(warnings) != 1 || warnings[0].Line != 3 {
		t.Fatalf("warnings = %#v, want empty ID warning for line 3", warnings)
	}
}

func TestLoadSessionMappingsAllowsNoValidRows(t *testing.T) {
	mappings, warnings, err := loadSessionMappings(writeSessionCSV(t, "議程 ID,Session ID\n,\n"))
	if err != nil {
		t.Fatalf("loadSessionMappings() error = %v", err)
	}
	if len(mappings) != 0 {
		t.Fatalf("mappings = %#v, want none", mappings)
	}
	if len(warnings) != 2 || warnings[1].Reason != "session CSV contains no valid mappings" {
		t.Fatalf("warnings = %#v, want row and empty schedule warnings", warnings)
	}
}

func TestLoadSessionMappingsRejectsDuplicateIDs(t *testing.T) {
	tests := []struct {
		name      string
		content   string
		wantError string
	}{
		{
			name:      "meeting ID",
			content:   "議程 ID,Session ID\nmeeting-a,SESSION-A\nmeeting-a,SESSION-B\n",
			wantError: "duplicates meeting ID",
		},
		{
			name:      "session ID",
			content:   "議程 ID,Session ID\nmeeting-a,SESSION-A\nmeeting-b,SESSION-A\n",
			wantError: "duplicates session ID",
		},
		{
			name:      "meeting ID after empty session ID",
			content:   "議程 ID,Session ID\nmeeting-a,\nmeeting-a,SESSION-A\n",
			wantError: "duplicates meeting ID",
		},
		{
			name:      "session ID after empty meeting ID",
			content:   "議程 ID,Session ID\n,SESSION-A\nmeeting-a,SESSION-A\n",
			wantError: "duplicates session ID",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, _, err := loadSessionMappings(writeSessionCSV(t, test.content))
			if err == nil || !strings.Contains(err.Error(), test.wantError) {
				t.Fatalf("loadSessionMappings() error = %v, want containing %q", err, test.wantError)
			}
		})
	}
}

func TestLoadMeetingScheduleRetriesEveryFetchError(t *testing.T) {
	var attempts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch attempts.Add(1) {
		case 1:
			_, _ = writer.Write([]byte(`{"sessions":`))
		case 2:
			writer.WriteHeader(http.StatusInternalServerError)
		default:
			writer.Header().Set("content-type", "application/json")
			_, _ = writer.Write([]byte(`{"sessions":[{"id":"SESSION-A","start":"2026-08-08T12:30:00+08:00"}]}`))
		}
	}))
	defer server.Close()

	schedule, warnings, err := loadMeetingScheduleWithOptions(
		context.Background(),
		writeSessionCSV(t, "議程 ID,Session ID\nmeeting-a,SESSION-A\n"),
		scheduleLoadOptions{url: server.URL, client: server.Client(), retryDelays: []time.Duration{0, 0}},
	)
	if err != nil {
		t.Fatalf("loadMeetingScheduleWithOptions() error = %v", err)
	}
	if attempts.Load() != 3 {
		t.Fatalf("attempts = %d, want 3", attempts.Load())
	}
	if len(warnings) != 0 {
		t.Fatalf("warnings = %#v, want none", warnings)
	}
	if _, found := schedule.starts["meeting-a"]; !found {
		t.Fatalf("schedule = %#v, want meeting-a start", schedule)
	}
}

func TestFetchOPASSScheduleStopsAfterConfiguredAttempts(t *testing.T) {
	var attempts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		attempts.Add(1)
		writer.WriteHeader(http.StatusBadGateway)
	}))
	defer server.Close()

	_, err := fetchOPASSSchedule(context.Background(), scheduleLoadOptions{
		url:         server.URL,
		client:      server.Client(),
		retryDelays: []time.Duration{0, 0},
	})
	if err == nil || !strings.Contains(err.Error(), "after 3 attempts") {
		t.Fatalf("fetchOPASSSchedule() error = %v, want exhausted attempts", err)
	}
	if attempts.Load() != 3 {
		t.Fatalf("attempts = %d, want 3", attempts.Load())
	}
}

func TestScheduleLoadDistinguishesRemoteFailureFromLocalConfiguration(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.WriteHeader(http.StatusBadGateway)
	}))
	defer server.Close()
	_, _, err := loadMeetingScheduleWithOptions(
		context.Background(),
		writeSessionCSV(t, "議程 ID,Session ID\nmeeting-a,SESSION-A\n"),
		scheduleLoadOptions{url: server.URL, client: server.Client()},
	)
	var remoteErr *scheduleRemoteError
	if !errors.As(err, &remoteErr) {
		t.Fatalf("remote schedule error = %v, want scheduleRemoteError", err)
	}
	goodServer := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("content-type", "application/json")
		_, _ = writer.Write([]byte(`{"sessions":[]}`))
	}))
	defer goodServer.Close()
	_, _, err = loadMeetingScheduleWithOptions(context.Background(), filepath.Join(t.TempDir(), "missing.csv"), scheduleLoadOptions{url: goodServer.URL, client: goodServer.Client()})
	if errors.As(err, &remoteErr) {
		t.Fatalf("local schedule error = %v, must not be scheduleRemoteError", err)
	}
}

func TestLoadMeetingScheduleIgnoresSessionMappingsMissingFromOPASS(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("content-type", "application/json")
		_, _ = writer.Write([]byte(`{"sessions":[{"id":"INVALID","start":"2026-08-08T12:30:00+08:00"}]}`))
	}))
	defer server.Close()

	schedule, _, err := loadMeetingScheduleWithOptions(
		context.Background(),
		writeSessionCSV(t, "議程 ID,Session ID\nmissing-meeting,MISSING\ninvalid-meeting,INVALID\n"),
		scheduleLoadOptions{url: server.URL, client: server.Client()},
	)
	if err != nil {
		t.Fatalf("loadMeetingScheduleWithOptions() error = %v, want unmatched mapping to be ignored", err)
	}
	if len(schedule.starts) != 1 {
		t.Fatalf("schedule starts = %#v, want only the matched mapping", schedule.starts)
	}
	if schedule.opassIDs["invalid-meeting"] != "INVALID" {
		t.Fatalf("schedule opass IDs = %#v, want invalid-meeting mapping", schedule.opassIDs)
	}
}

func TestLoadMeetingScheduleRejectsInvalidMappedStart(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("content-type", "application/json")
		_, _ = writer.Write([]byte(`{"sessions":[{"id":"INVALID","start":"not-a-time"}]}`))
	}))
	defer server.Close()

	_, _, err := loadMeetingScheduleWithOptions(
		context.Background(),
		writeSessionCSV(t, "議程 ID,Session ID\ninvalid-meeting,INVALID\n"),
		scheduleLoadOptions{url: server.URL, client: server.Client()},
	)
	if err == nil || !strings.Contains(err.Error(), "has invalid start time") {
		t.Fatalf("loadMeetingScheduleWithOptions() error = %v, want invalid start error", err)
	}
}

func TestLoadMeetingScheduleRejectsDuplicateOPASSSessions(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("content-type", "application/json")
		_, _ = writer.Write([]byte(`{"sessions":[{"id":"SAME","start":"2026-08-08T09:00:00+08:00"},{"id":"SAME","start":"2026-08-08T10:00:00+08:00"}]}`))
	}))
	defer server.Close()

	_, _, err := loadMeetingScheduleWithOptions(
		context.Background(),
		writeSessionCSV(t, "議程 ID,Session ID\nmeeting-a,SAME\n"),
		scheduleLoadOptions{url: server.URL, client: server.Client()},
	)
	if err == nil || !strings.Contains(err.Error(), "duplicates session ID") {
		t.Fatalf("loadMeetingScheduleWithOptions() error = %v, want duplicate opass ID", err)
	}
}

func TestMeetingSchedulePrepareMeetings(t *testing.T) {
	first := time.Date(2026, time.August, 8, 1, 0, 0, 0, time.UTC)
	second := first.Add(time.Hour)
	schedule := meetingSchedule{
		enabled: true,
		starts: map[string]time.Time{
			"known-b": first,
			"known-a": first,
			"later":   second,
		},
	}
	meetings := []roomMeetingView{
		{ID: "unknown-z", Title: "Unknown Z"},
		{ID: "later", Title: "Later"},
		{ID: "known-b", Title: "Same"},
		{ID: "unknown-a", Title: "Unknown A"},
		{ID: "known-a", Title: "Same"},
	}

	prepared := schedule.prepareMeetings(meetings)
	want := []string{"known-a", "known-b", "later", "unknown-z", "unknown-a"}
	for index, meetingID := range want {
		if prepared[index].ID != meetingID {
			t.Fatalf("prepared[%d].ID = %q, want %q; all = %#v", index, prepared[index].ID, meetingID, prepared)
		}
	}
	if meetings[1].ScheduledStart != nil {
		t.Fatal("prepareMeetings() mutated source meetings")
	}
}

func TestMeetingSchedulePrepareMeetingsFallsBackToTitleAndID(t *testing.T) {
	meetings := []roomMeetingView{
		{ID: "z", Title: "Beta"},
		{ID: "b", Title: "Alpha"},
		{ID: "a", Title: "Alpha"},
	}
	prepared := (meetingSchedule{}).prepareMeetings(meetings)
	want := []string{"a", "b", "z"}
	for index, meetingID := range want {
		if prepared[index].ID != meetingID {
			t.Fatalf("prepared[%d].ID = %q, want %q", index, prepared[index].ID, meetingID)
		}
	}
}

func TestMeetingScheduleValidateMeetingsRequiresUniqueOPASSStarts(t *testing.T) {
	start := time.Date(2026, time.August, 8, 1, 0, 0, 0, time.UTC)
	schedule := meetingSchedule{
		enabled:   true,
		starts:    map[string]time.Time{"meeting-a": start, "meeting-b": start},
		snapshots: make(map[string][]roomMeetingView),
	}
	_, err := schedule.validateStartupMeetings("room-a", []roomMeetingView{{ID: "meeting-a"}, {ID: "meeting-b"}})
	if err == nil || !strings.Contains(err.Error(), "same opass start time") {
		t.Fatalf("validateStartupMeetings() error = %v, want duplicate start error", err)
	}
	withDifferentOffsets := meetingSchedule{
		enabled: true,
		starts: map[string]time.Time{
			"meeting-a": time.Date(2026, time.August, 8, 1, 0, 0, 0, time.FixedZone("UTC+8", 8*60*60)),
			"meeting-b": time.Date(2026, time.August, 7, 17, 0, 0, 0, time.UTC),
		},
		snapshots: make(map[string][]roomMeetingView),
	}
	_, err = withDifferentOffsets.validateStartupMeetings("room-a", []roomMeetingView{{ID: "meeting-a"}, {ID: "meeting-b"}})
	if err == nil || !strings.Contains(err.Error(), "same opass start time") {
		t.Fatalf("validateStartupMeetings() with offsets error = %v, want duplicate instant error", err)
	}
}

func TestMeetingScheduleKeepsStartupOrderWhileUpdatingStatus(t *testing.T) {
	first := time.Date(2026, time.August, 8, 1, 0, 0, 0, time.UTC)
	second := first.Add(time.Hour)
	schedule := meetingSchedule{
		enabled:   true,
		starts:    map[string]time.Time{"meeting-a": first, "meeting-b": second},
		snapshots: make(map[string][]roomMeetingView),
	}
	startup, err := schedule.validateStartupMeetings("room-a", []roomMeetingView{{ID: "meeting-b", Title: "B"}, {ID: "meeting-a", Title: "A"}})
	if err != nil {
		t.Fatalf("startup validation error = %v", err)
	}
	if startup[0].ID != "meeting-a" || startup[1].ID != "meeting-b" {
		t.Fatalf("startup order = %#v, want meeting-a then meeting-b", startup)
	}

	live, err := schedule.validateMeetings("room-a", []roomMeetingView{
		{ID: "meeting-b", Title: "B", Status: "paused"},
		{ID: "meeting-a", Title: "A", Status: "in_progress"},
	})
	if err != nil {
		t.Fatalf("live validation error = %v", err)
	}
	if live[0].ID != "meeting-a" || live[0].Status != "in_progress" || live[1].ID != "meeting-b" || live[1].Status != "paused" {
		t.Fatalf("live meetings = %#v, want fixed order with updated statuses", live)
	}
}

func TestMeetingScheduleIgnoresUnmatchedRozetaMeetings(t *testing.T) {
	schedule := meetingSchedule{
		enabled:   true,
		starts:    map[string]time.Time{"meeting-a": time.Date(2026, time.August, 8, 1, 0, 0, 0, time.UTC)},
		snapshots: make(map[string][]roomMeetingView),
	}
	startup, err := schedule.validateStartupMeetings("room-a", []roomMeetingView{{ID: "unmatched"}, {ID: "meeting-a", Title: "A"}})
	if err != nil {
		t.Fatalf("startup validation error = %v", err)
	}
	if len(startup) != 1 || startup[0].ID != "meeting-a" {
		t.Fatalf("startup meetings = %#v, want only matched meeting", startup)
	}

	live, err := schedule.validateMeetings("room-a", []roomMeetingView{{ID: "unmatched"}})
	if err != nil {
		t.Fatalf("live validation error = %v, want unmatched meeting ignored", err)
	}
	if len(live) != 0 {
		t.Fatalf("live meetings = %#v, want empty result", live)
	}
}

func TestMeetingScheduleRejectsChangedMeetingIdentity(t *testing.T) {
	schedule := meetingSchedule{
		enabled:   true,
		starts:    map[string]time.Time{"meeting-a": time.Date(2026, time.August, 8, 1, 0, 0, 0, time.UTC)},
		snapshots: make(map[string][]roomMeetingView),
	}
	if _, err := schedule.validateStartupMeetings("room-a", []roomMeetingView{{ID: "meeting-a", Title: "Original"}}); err != nil {
		t.Fatalf("startup validation error = %v", err)
	}
	_, err := schedule.validateMeetings("room-a", []roomMeetingView{{ID: "meeting-a", Title: "Changed"}})
	if err == nil || !strings.Contains(err.Error(), "immutable metadata changed") {
		t.Fatalf("validateMeetings() error = %v, want identity error", err)
	}
}

func TestMeetingScheduleNextMeetingUsesScheduledOrder(t *testing.T) {
	first := time.Date(2026, time.August, 8, 1, 0, 0, 0, time.UTC)
	schedule := meetingSchedule{enabled: true, starts: map[string]time.Time{
		"current": first,
		"same-a":  first,
		"same-b":  first,
		"later":   first.Add(time.Hour),
	}}
	meetings := []roomMeetingView{
		{ID: "later", Title: "Later"},
		{ID: "current", Title: "A Current"},
		{ID: "same-b", Title: "Beta"},
		{ID: "same-a", Title: "Zulu"},
	}

	next, err := schedule.nextMeeting(meetings, "current")
	if err != nil {
		t.Fatal(err)
	}
	if next.ID != "same-b" {
		t.Fatalf("next meeting = %#v, want same-b", next)
	}
}

func TestMeetingScheduleNextMeetingRejectsUnscheduledOrFinalCurrent(t *testing.T) {
	schedule := meetingSchedule{enabled: true, starts: map[string]time.Time{
		"current": time.Unix(1, 0),
	}}
	meetings := []roomMeetingView{{ID: "current"}}
	if _, err := schedule.nextMeeting(meetings, "missing"); !errors.Is(err, errCurrentMeetingUnscheduled) {
		t.Fatalf("unscheduled error = %v", err)
	}
	if _, err := schedule.nextMeeting(meetings, "current"); !errors.Is(err, errNextMeetingNotFound) {
		t.Fatalf("final error = %v", err)
	}
}

func writeSessionCSV(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "session.csv")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write session CSV: %v", err)
	}
	return path
}
