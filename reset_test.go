package main

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
)

func TestResetReadyRoomResetsOnlyNonActiveMeetings(t *testing.T) {
	var mu sync.Mutex
	statuses := map[string]string{
		"ready-meeting":     "ready",
		"paused-meeting":    "paused",
		"completed-meeting": "completed",
		"active-meeting":    "in_progress",
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
	view := roomViewByName(t, c.snapshotRooms(), "room-a")

	if _, _, err := c.resetReadyPreflight(t.Context(), "room-a", a.epoch, view.ReconciliationRun, view.Generation); !errors.Is(err, errResetActive) {
		t.Fatalf("preflight error = %v, want active-set rejection", err)
	}
	mu.Lock()
	statuses["active-meeting"] = "paused"
	mu.Unlock()
	view, meetings, err := c.resetReadyPreflight(t.Context(), "room-a", a.epoch, view.ReconciliationRun, view.Generation)
	if err != nil {
		t.Fatal(err)
	}
	if !view.ResetReady || len(meetings) != 4 {
		t.Fatalf("preflight view/meetings = %#v/%d", view, len(meetings))
	}
	ids := meetingIDs(meetings)
	view, results, err := c.resetReadyRoom(t.Context(), "room-a", a.epoch, view.ReconciliationRun, view.Generation, ids)
	if err != nil {
		t.Fatal(err)
	}
	if !view.ResetReady || resumeRequests.Load() != 3 {
		t.Fatalf("reset view/resume requests = %#v/%d", view, resumeRequests.Load())
	}
	outcomes := make(map[string]string, len(results))
	for _, result := range results {
		outcomes[result.MeetingID] = result.Outcome
	}
	want := map[string]string{
		"ready-meeting":     "already_ready",
		"paused-meeting":    "reset",
		"completed-meeting": "reset",
		"active-meeting":    "reset",
	}
	if !slices.Equal([]string{outcomes["ready-meeting"], outcomes["paused-meeting"], outcomes["completed-meeting"], outcomes["active-meeting"]}, []string{want["ready-meeting"], want["paused-meeting"], want["completed-meeting"], want["active-meeting"]}) {
		t.Fatalf("outcomes = %#v, want %#v", outcomes, want)
	}
}
