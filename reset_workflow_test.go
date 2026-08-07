package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestRunResetDoesNotRequireServerSecrets(t *testing.T) {
	t.Setenv("ADMIN_PASSWORD", "")
	t.Setenv("SESSION_SECRET", "")
	t.Setenv("EXTERNAL_API_TOKEN", "")

	missingAccount := filepath.Join(t.TempDir(), "missing-account.csv")
	err := run([]string{"-account", missingAccount, "-reset", "all,all"}, io.Discard)
	if err == nil || strings.Contains(err.Error(), "ADMIN_PASSWORD") || strings.Contains(err.Error(), "SESSION_SECRET") || strings.Contains(err.Error(), "EXTERNAL_API_TOKEN") {
		t.Fatalf("run() error = %v, want account loading error without server-secret validation", err)
	}
}

func TestParseResetSelection(t *testing.T) {
	tests := []struct {
		name     string
		value    string
		wantDate string
		wantRoom string
		wantErr  bool
	}{
		{name: "date and all rooms", value: "2026/8/8,all", wantDate: "2026/8/8", wantRoom: "all"},
		{name: "all dates and exact room", value: "all,RB101", wantRoom: "RB101"},
		{name: "all dates and rooms", value: "all,all", wantRoom: "all"},
		{name: "missing comma", value: "all", wantErr: true},
		{name: "empty date", value: ",RB101", wantErr: true},
		{name: "empty room", value: "2026/8/8,", wantErr: true},
		{name: "multiple commas", value: "all,RB,101", wantErr: true},
		{name: "invalid date", value: "2026/2/30,all", wantErr: true},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			selection, err := parseResetSelection(test.value)
			if (err != nil) != test.wantErr {
				t.Fatalf("parseResetSelection() error = %v, want error %t", err, test.wantErr)
			}
			if test.wantErr {
				return
			}
			if selection.roomName != test.wantRoom {
				t.Fatalf("room = %q, want %q", selection.roomName, test.wantRoom)
			}
			if test.wantDate == "" {
				if selection.date != nil {
					t.Fatalf("date = %v, want nil", selection.date)
				}
				return
			}
			if selection.date == nil || selection.date.Format(resetDateLayout) != test.wantDate {
				t.Fatalf("date = %v, want %s", selection.date, test.wantDate)
			}
		})
	}
}

func TestResetTargetsFiltersDateAndRoom(t *testing.T) {
	location, err := time.LoadLocation("Asia/Taipei")
	if err != nil {
		t.Fatal(err)
	}
	first := time.Date(2026, time.August, 8, 10, 0, 0, 0, location)
	second := first.Add(24 * time.Hour)
	c := &controller{rooms: map[string]*controllerRoom{
		"RB101": {name: "RB101", meetings: []roomMeetingView{
			{ID: "day-one", Title: "Day one", ScheduledStart: &first},
			{ID: "day-two", Title: "Day two", ScheduledStart: &second},
			{ID: preparationMeetingID, Virtual: true},
		}},
		"RB102": {name: "RB102", meetings: []roomMeetingView{
			{ID: "other-room", Title: "Other room", ScheduledStart: &first},
		}},
	}}

	selection, err := parseResetSelection("2026/8/8,RB101")
	if err != nil {
		t.Fatal(err)
	}
	targets, err := c.resetTargets(selection)
	if err != nil {
		t.Fatal(err)
	}
	if len(targets) != 1 || targets[0].meetingID != "day-one" {
		t.Fatalf("targets = %#v, want only day-one", targets)
	}
}

func TestResetSelectedMeetingOnlyResetsRequestedMeeting(t *testing.T) {
	var mu sync.Mutex
	statuses := map[string]string{
		"selected": "paused",
		"other":    "paused",
	}
	var resumeRequests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/api/v1/meetings" {
			statusFilter := request.URL.Query().Get("status")
			mu.Lock()
			data := make([]any, 0, len(statuses))
			for id, status := range statuses {
				if statusFilter == "" || status == statusFilter {
					data = append(data, meetingPayload(id, status, 1))
				}
			}
			mu.Unlock()
			writeJSON(t, writer, map[string]any{"data": data, "links": map[string]any{"next": nil}})
			return
		}
		if request.Method == http.MethodGet {
			id, err := url.PathUnescape(request.URL.Path[len("/api/v1/meetings/"):])
			if err != nil {
				t.Fatalf("decode meeting ID: %v", err)
			}
			mu.Lock()
			status := statuses[id]
			mu.Unlock()
			writeJSON(t, writer, meetingPayload(id, status, 1))
			return
		}
		if request.Method == http.MethodPost && len(request.URL.Path) > len("/api/v1/meetings/") && request.URL.Path[len(request.URL.Path)-len("/resume"):] == "/resume" {
			id := request.URL.Path[len("/api/v1/meetings/") : len(request.URL.Path)-len("/resume")]
			mu.Lock()
			statuses[id] = "ready"
			mu.Unlock()
			resumeRequests.Add(1)
			writeJSON(t, writer, meetingPayload(id, "ready", 1))
			return
		}
		http.NotFound(writer, request)
	}))
	defer server.Close()

	a := newTestApp(t, map[string]string{"room-a": "token-a"})
	a.rozetaBaseURL, a.httpClient = server.URL, server.Client()
	c := newTestController(t, a, filepath.Join(t.TempDir(), "state.json"))

	result, err := c.resetSelectedMeeting(t.Context(), "room-a", "selected")
	if err != nil {
		t.Fatal(err)
	}
	if result.Outcome != "reset" || resumeRequests.Load() != 1 {
		t.Fatalf("result/resume requests = %#v/%d, want reset/1", result, resumeRequests.Load())
	}
	mu.Lock()
	got := []string{statuses["selected"], statuses["other"]}
	mu.Unlock()
	if !slices.Equal(got, []string{"ready", "paused"}) {
		t.Fatalf("statuses = %#v, want selected ready and other paused", got)
	}
}

func TestResetJobsAgeOutRetryingObservation(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/api/v1/meetings" {
			writeJSON(t, writer, map[string]any{"data": []any{}, "links": map[string]any{"next": nil}})
			return
		}
		writer.WriteHeader(http.StatusBadGateway)
	}))
	defer server.Close()

	a := newTestApp(t, map[string]string{"room-a": "token-a"})
	a.rozetaBaseURL, a.httpClient = server.URL, server.Client()
	c := newTestController(t, a, filepath.Join(t.TempDir(), "state.json"))
	reports := runResetJobs(t.Context(), c, []resetTarget{{roomName: "room-a", meetingID: "missing"}}, 0)
	if len(reports) != 1 || reports[0].success {
		t.Fatalf("reports = %#v, want one failed report", reports)
	}
}
