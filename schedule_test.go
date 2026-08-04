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
	_, _, err = loadMeetingScheduleWithOptions(context.Background(), filepath.Join(t.TempDir(), "missing.csv"), scheduleLoadOptions{})
	if errors.As(err, &remoteErr) {
		t.Fatalf("local schedule error = %v, must not be scheduleRemoteError", err)
	}
}

func TestLoadMeetingScheduleDowngradesMissingAndInvalidStarts(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("content-type", "application/json")
		_, _ = writer.Write([]byte(`{"sessions":[{"id":"INVALID","start":"not-a-time"}]}`))
	}))
	defer server.Close()

	schedule, warnings, err := loadMeetingScheduleWithOptions(
		context.Background(),
		writeSessionCSV(t, "議程 ID,Session ID\nmissing-meeting,MISSING\ninvalid-meeting,INVALID\n"),
		scheduleLoadOptions{url: server.URL, client: server.Client()},
	)
	if err != nil {
		t.Fatalf("loadMeetingScheduleWithOptions() error = %v", err)
	}
	if !schedule.enabled || len(schedule.starts) != 0 {
		t.Fatalf("schedule = %#v, want enabled schedule without known starts", schedule)
	}
	if len(warnings) != 2 {
		t.Fatalf("warnings = %#v, want missing and invalid warnings", warnings)
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

func writeSessionCSV(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "session.csv")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write session CSV: %v", err)
	}
	return path
}
