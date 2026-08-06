package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
)

func newTestApp(t *testing.T, tokens map[string]string) *app {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	return newApp(ctx, tokens, "password", []byte("01234567890123456789012345678901"))
}

func newTestController(t *testing.T, a *app, statePath string) *controller {
	t.Helper()
	c, err := newController(context.Background(), a, a.tokenStore, meetingSchedule{starts: make(map[string]time.Time)}, statePath)
	if err != nil {
		t.Fatal(err)
	}
	a.controller = c
	t.Cleanup(c.close)
	return c
}

func updateTestDesired(t *testing.T, c *controller, roomName, meetingID string) roomView {
	t.Helper()
	view := roomViewByName(t, c.snapshotRooms(), roomName)
	updated, err := c.updateDesired(roomName, desiredStateUpdate{
		MeetingID: meetingID, ExpectedEpoch: c.app.epoch,
		ExpectedRun: view.ReconciliationRun, ExpectedGeneration: view.Generation,
	})
	if err != nil {
		t.Fatal(err)
	}
	return updated
}

func activateTestRoom(t *testing.T, c *controller, roomName string) (*controllerRoom, uint64, uint64) {
	t.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()
	room := c.rooms[roomName]
	if room == nil {
		t.Fatalf("unknown room %q", roomName)
	}
	room.reconciliationRun++
	room.lifecycle = reconciliationActive
	room.resumeAuthorized = room.desired.Generation
	room.runCtx, room.runCancel = context.WithCancel(c.ctx)
	room.conditions = activeConditions(true, false)
	return room, room.reconciliationRun, room.desired.Generation
}

func roomViewByName(t *testing.T, rooms []roomView, roomName string) roomView {
	t.Helper()
	for _, room := range rooms {
		if room.RoomName == roomName {
			return room
		}
	}
	t.Fatalf("room %q not found in %#v", roomName, rooms)
	return roomView{}
}

func waitFor(t *testing.T, timeout time.Duration, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("condition was not satisfied before timeout")
}

func writeJSON(t *testing.T, writer http.ResponseWriter, value any) {
	t.Helper()
	writer.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(writer).Encode(value); err != nil {
		t.Fatal(err)
	}
}

func meetingPayload(id, status string, updatedAt int64) map[string]any {
	meeting := map[string]any{
		"id": id, "title": id, "status": status,
		"languages": map[string]string{"source": "en", "target": "zh-TW"},
	}
	if updatedAt != 0 {
		meeting["updated_at"] = updatedAt
	}
	return meeting
}

func startPreflightFacts(destructive bool) *preflightFacts {
	return &preflightFacts{DestructiveResume: &destructive}
}

func stopPreflightFacts(activeMeetingIDs []string) *preflightFacts {
	activeMeetingIDs = append([]string{}, activeMeetingIDs...)
	return &preflightFacts{ActiveMeetingIDs: &activeMeetingIDs}
}

func TestDesiredStateFileV2AndAtomicV1Migration(t *testing.T) {
	t.Run("loads v2 consumed resume", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "state.json")
		data := `{"version":2,"rooms":{"room-a":{"meeting_id":"meeting-a","generation":3,"consumed_resume":{"generation":3,"completed_updated_at":"2026-08-04T01:02:03Z"}}}}`
		if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
		file, err := loadDesiredStateFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if consumed := file.Rooms["room-a"].ConsumedResume; consumed == nil || consumed.Generation != 3 {
			t.Fatalf("consumed resume = %#v", consumed)
		}
	})

	t.Run("migrates v1 without restoring running", func(t *testing.T) {
		directory := t.TempDir()
		path := filepath.Join(directory, "state.json")
		legacy := `{"version":1,"rooms":{"room-a":{"meeting_id":"meeting-a","generation":7,"running":true}}}`
		if err := os.WriteFile(path, []byte(legacy), 0o600); err != nil {
			t.Fatal(err)
		}
		file, err := loadDesiredStateFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if file.Version != 2 || file.Rooms["room-a"].MeetingID != "meeting-a" || file.Rooms["room-a"].Generation != 7 {
			t.Fatalf("migrated state = %#v", file)
		}
		persisted, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(persisted, []byte(`"running"`)) || !bytes.Contains(persisted, []byte(`"version": 2`)) {
			t.Fatalf("persisted migration = %s", persisted)
		}
		entries, err := os.ReadDir(directory)
		if err != nil {
			t.Fatal(err)
		}
		if len(entries) != 1 || entries[0].Name() != "state.json" {
			t.Fatalf("migration left temporary files: %#v", entries)
		}
	})

	t.Run("rejects malformed and unsupported state", func(t *testing.T) {
		for name, data := range map[string]string{
			"malformed":    `{"version":2,"rooms":`,
			"unsupported":  `{"version":99,"rooms":{}}`,
			"invalid room": `{"version":2,"rooms":{"room-a":{"meeting_id":"","generation":0}}}`,
		} {
			t.Run(name, func(t *testing.T) {
				path := filepath.Join(t.TempDir(), "state.json")
				if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
					t.Fatal(err)
				}
				if _, err := loadDesiredStateFile(path); err == nil {
					t.Fatal("loadDesiredStateFile() error = nil")
				}
			})
		}
	})
}

func TestSelectCurrentMeeting(t *testing.T) {
	opassIDs := map[string]string{"meeting-a": "OPASS-A", "meeting-b": "OPASS-B"}
	active := []roomMeetingView{
		{ID: "meeting-a", Title: "A"},
		{ID: "meeting-b", Title: "B"},
	}

	tests := []struct {
		name    string
		active  []roomMeetingView
		desired string
		want    *currentMeetingResponse
	}{
		{name: "empty", active: nil},
		{name: "single", active: active[:1], want: &currentMeetingResponse{Name: "A", OPASSID: "OPASS-A"}},
		{name: "multiple selects desired", active: active, desired: "meeting-b", want: &currentMeetingResponse{Name: "B", OPASSID: "OPASS-B"}},
		{name: "multiple without desired", active: active, desired: "meeting-c"},
		{name: "missing opass mapping", active: []roomMeetingView{{ID: "meeting-c", Title: "C"}}},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := selectCurrentMeeting(test.active, test.desired, opassIDs); !equalCurrentMeeting(got, test.want) {
				t.Fatalf("selectCurrentMeeting() = %#v, want %#v", got, test.want)
			}
		})
	}
}

func equalCurrentMeeting(left, right *currentMeetingResponse) bool {
	if left == nil || right == nil {
		return left == right
	}
	return *left == *right
}

func TestCurrentMeetingEndpointIsPublic(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Query().Get("status") != "in_progress" {
			t.Errorf("status query = %q, want in_progress", request.URL.Query().Get("status"))
		}
		writeJSON(t, writer, map[string]any{
			"data":  []map[string]any{meetingPayload("meeting-a", "in_progress", 0)},
			"links": map[string]any{"next": nil},
		})
	}))
	defer server.Close()

	a := newTestApp(t, map[string]string{"room-a": "token-a"})
	a.rozetaBaseURL = server.URL
	schedule := meetingSchedule{
		enabled:  true,
		starts:   map[string]time.Time{"meeting-a": time.Now().UTC()},
		opassIDs: map[string]string{"meeting-a": "OPASS-A"},
		snapshots: map[string][]roomMeetingView{
			"room-a": {{ID: "meeting-a", Title: "meeting-a"}},
		},
	}
	c, err := newController(context.Background(), a, a.tokenStore, schedule, filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	defer c.close()
	a.controller = c
	updateTestDesired(t, c, "room-a", "meeting-a")

	router, err := a.router()
	if err != nil {
		t.Fatal(err)
	}
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/v1/rooms/room-a/in-progress", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d: %s", recorder.Code, http.StatusOK, recorder.Body.String())
	}
	var got currentMeetingResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got != (currentMeetingResponse{Name: "meeting-a", OPASSID: "OPASS-A"}) {
		t.Fatalf("response = %#v", got)
	}
}

func TestFetchRozetaMeetingsUsesFilteredPagination(t *testing.T) {
	var server *httptest.Server
	var queries []string
	server = httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		queries = append(queries, request.URL.RawQuery)
		if request.URL.Query().Get("status") != "in_progress" {
			t.Errorf("status filter = %q", request.URL.Query().Get("status"))
		}
		switch request.URL.Query().Get("page") {
		case "1":
			writeJSON(t, writer, map[string]any{
				"data":  []any{meetingPayload("meeting-a", "in_progress", 1)},
				"links": map[string]any{"next": server.URL + "/api/v1/meetings?page=2&status=in_progress"},
			})
		case "2":
			writeJSON(t, writer, map[string]any{
				"data":  []any{meetingPayload("meeting-b", "in_progress", 2)},
				"links": map[string]any{"next": nil},
			})
		default:
			writer.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	a := newTestApp(t, map[string]string{"room-a": "token-a"})
	a.rozetaBaseURL, a.httpClient = server.URL, server.Client()
	meetings, err := a.fetchRozetaMeetings(context.Background(), "token-a", "in_progress")
	if err != nil {
		t.Fatal(err)
	}
	if ids := meetingIDs(meetings); !slices.Equal(ids, []string{"meeting-a", "meeting-b"}) || len(queries) != 2 {
		t.Fatalf("meetings/queries = %#v/%#v", ids, queries)
	}
}

func TestFetchRozetaMeetingsRejectsBrokenFilteredResults(t *testing.T) {
	tests := []struct {
		name string
		body func(string) string
	}{
		{name: "pagination drops filter", body: func(base string) string {
			return fmt.Sprintf(`{"data":[],"links":{"next":%q}}`, base+"/api/v1/meetings?page=2")
		}},
		{name: "result has wrong status", body: func(string) string {
			return `{"data":[{"id":"meeting-a","status":"paused","languages":{}}],"links":{"next":null}}`
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var server *httptest.Server
			server = httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
				writer.Header().Set("Content-Type", "application/json")
				_, _ = writer.Write([]byte(test.body(server.URL)))
			}))
			defer server.Close()
			a := newTestApp(t, map[string]string{"room-a": "token-a"})
			a.rozetaBaseURL, a.httpClient = server.URL, server.Client()
			_, err := a.fetchRozetaMeetings(context.Background(), "token-a", "in_progress")
			var protocolErr *rozetaProtocolError
			if !errors.As(err, &protocolErr) {
				t.Fatalf("error = %v, want protocol error", err)
			}
		})
	}
}

func TestRoomActorIsSerialAndWakeIsCoalesced(t *testing.T) {
	var concurrent atomic.Int32
	var maximum atomic.Int32
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		current := concurrent.Add(1)
		defer concurrent.Add(-1)
		for {
			old := maximum.Load()
			if current <= old || maximum.CompareAndSwap(old, current) {
				break
			}
		}
		requests.Add(1)
		time.Sleep(3 * time.Millisecond)
		writeJSON(t, writer, map[string]any{
			"data":  []any{meetingPayload("meeting-a", "in_progress", 1)},
			"links": map[string]any{"next": nil},
		})
	}))
	defer server.Close()

	a := newTestApp(t, map[string]string{"room-a": "token-a"})
	a.rozetaBaseURL, a.httpClient = server.URL, server.Client()
	statePath := filepath.Join(t.TempDir(), "state.json")
	c := newTestController(t, a, statePath)
	view := updateTestDesired(t, c, "room-a", "meeting-a")
	_, results, err := c.reconcileLifecycle(a.epoch, "start", []reconciliationTarget{{
		RoomName: "room-a", ExpectedReconciliationRun: view.ReconciliationRun, ExpectedGeneration: view.Generation,
	}}, true)
	if err != nil || len(results) != 1 || !results[0].Applied {
		t.Fatalf("start result/error = %#v/%v", results, err)
	}
	room := c.rooms["room-a"]
	c.mu.RLock()
	authorizedGeneration := room.resumeAuthorized
	c.mu.RUnlock()
	if authorizedGeneration != view.Generation {
		t.Fatalf("Start authorized generation = %d, want %d", authorizedGeneration, view.Generation)
	}
	for range 100 {
		c.notify(room)
	}
	if len(room.wake) != 1 {
		t.Fatalf("coalesced wake length = %d", len(room.wake))
	}
	waitFor(t, time.Second, func() bool { return requests.Load() >= 2 })
	if maximum.Load() != 1 {
		t.Fatalf("maximum concurrent per-room requests = %d, want 1", maximum.Load())
	}
}

func TestRequestSchedulerServesThirtyObservations(t *testing.T) {
	scheduler := newRequestScheduler(context.Background())
	defer scheduler.close()

	var concurrent atomic.Int32
	var maximum atomic.Int32
	var completed atomic.Int32
	var workers sync.WaitGroup
	for range 30 {
		workers.Go(func() {
			_, err := runScheduled(context.Background(), scheduler, observationRequest, func(context.Context) (struct{}, error) {
				current := concurrent.Add(1)
				defer concurrent.Add(-1)
				for {
					old := maximum.Load()
					if current <= old || maximum.CompareAndSwap(old, current) {
						break
					}
				}
				time.Sleep(2 * time.Millisecond)
				completed.Add(1)
				return struct{}{}, nil
			})
			if err != nil {
				t.Errorf("observation failed: %v", err)
			}
		})
	}
	workers.Wait()

	if completed.Load() != 30 {
		t.Fatalf("completed observations = %d, want 30", completed.Load())
	}
	if maximum.Load() > observationWorkerCount {
		t.Fatalf("maximum observation concurrency = %d, want at most %d", maximum.Load(), observationWorkerCount)
	}
}

func TestRequestSchedulerControlsDoNotWaitForObservations(t *testing.T) {
	scheduler := newRequestScheduler(context.Background())
	defer scheduler.close()

	releaseObservations := make(chan struct{})
	enteredObservations := make(chan struct{}, observationWorkerCount)
	var observationWorkers sync.WaitGroup
	for range observationWorkerCount {
		observationWorkers.Go(func() {
			_, err := runScheduled(context.Background(), scheduler, observationRequest, func(context.Context) (struct{}, error) {
				enteredObservations <- struct{}{}
				<-releaseObservations
				return struct{}{}, nil
			})
			if err != nil {
				t.Errorf("observation failed: %v", err)
			}
		})
	}
	waitUntil := func(count func() bool) bool {
		deadline := time.Now().Add(time.Second)
		for time.Now().Before(deadline) {
			if count() {
				return true
			}
			time.Sleep(time.Millisecond)
		}
		return count()
	}
	if !waitUntil(func() bool { return len(enteredObservations) == observationWorkerCount }) {
		close(releaseObservations)
		observationWorkers.Wait()
		t.Fatal("observation workers did not all start")
	}

	controlsStarted := make(chan struct{}, controlWorkerCount)
	var controlWorkers sync.WaitGroup
	for range controlWorkerCount {
		controlWorkers.Go(func() {
			_, err := runScheduled(context.Background(), scheduler, controlRequest, func(context.Context) (struct{}, error) {
				controlsStarted <- struct{}{}
				return struct{}{}, nil
			})
			if err != nil {
				t.Errorf("control failed: %v", err)
			}
		})
	}
	if !waitUntil(func() bool { return len(controlsStarted) == controlWorkerCount }) {
		close(releaseObservations)
		observationWorkers.Wait()
		controlWorkers.Wait()
		t.Fatal("control workers waited for observations")
	}

	close(releaseObservations)
	observationWorkers.Wait()
	controlWorkers.Wait()
}

func TestDifferentRoomsDispatchControlsConcurrently(t *testing.T) {
	var concurrent atomic.Int32
	var maximum atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodPost || request.URL.Path != "/api/v1/commands" {
			http.NotFound(writer, request)
			return
		}
		current := concurrent.Add(1)
		defer concurrent.Add(-1)
		for {
			old := maximum.Load()
			if current <= old || maximum.CompareAndSwap(old, current) {
				break
			}
		}
		time.Sleep(20 * time.Millisecond)
		writeJSON(t, writer, map[string]any{"success": true})
	}))
	defer server.Close()

	a := newTestApp(t, map[string]string{"room-a": "token-a", "room-b": "token-b"})
	a.rozetaBaseURL, a.httpClient = server.URL, server.Client()
	c := newTestController(t, a, filepath.Join(t.TempDir(), "state.json"))
	roomA, runA, generationA := activateTestRoom(t, c, "room-a")
	roomB, runB, generationB := activateTestRoom(t, c, "room-b")

	var workers sync.WaitGroup
	workers.Go(func() {
		if err := c.controlOnce(roomA, runA, generationA, reconciliationActive, "start_meeting", "meeting-a"); err != nil {
			t.Errorf("room-a control failed: %v", err)
		}
	})
	workers.Go(func() {
		if err := c.controlOnce(roomB, runB, generationB, reconciliationActive, "start_meeting", "meeting-b"); err != nil {
			t.Errorf("room-b control failed: %v", err)
		}
	})
	workers.Wait()

	if maximum.Load() != 2 {
		t.Fatalf("maximum cross-room control concurrency = %d, want 2", maximum.Load())
	}
}

func TestSuspendedDesiredUpdatePersistsWithoutRemoteRequest(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		requests.Add(1)
	}))
	defer server.Close()
	a := newTestApp(t, map[string]string{"room-a": "token-a"})
	a.rozetaBaseURL, a.httpClient = server.URL, server.Client()
	path := filepath.Join(t.TempDir(), "state.json")
	c := newTestController(t, a, path)
	view := updateTestDesired(t, c, "room-a", "meeting-a")
	if requests.Load() != 0 || view.Lifecycle != "suspended" || view.Generation != 1 {
		t.Fatalf("requests/view = %d/%#v", requests.Load(), view)
	}
	file, err := loadDesiredStateFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if file.Rooms["room-a"].MeetingID != "meeting-a" {
		t.Fatalf("persisted state = %#v", file)
	}
}

func TestRefreshRoomMeetingsEnablesInitialSelectionWithoutOnlineCount(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if strings.Contains(request.URL.Path, "online-count") {
			t.Fatal("online-count endpoint was called")
		}
		writeJSON(t, writer, map[string]any{
			"data":  []any{meetingPayload("meeting-a", "ready", 1)},
			"links": map[string]any{"next": nil},
		})
	}))
	defer server.Close()
	a := newTestApp(t, map[string]string{"room-a": "token-a"})
	a.rozetaBaseURL, a.httpClient = server.URL, server.Client()
	statePath := filepath.Join(t.TempDir(), "state.json")
	c := newTestController(t, a, statePath)
	view, meetings, err := c.refreshRoomMeetings(context.Background(), "room-a")
	if err != nil {
		t.Fatal(err)
	}
	if view.Lifecycle != "suspended" || len(meetings) != 1 || meetings[0].ID != "meeting-a" || len(view.Meetings) != 1 {
		t.Fatalf("view/meetings = %#v/%#v", view, meetings)
	}
}

func TestAdvanceAndStartCommitsNextMeetingAfterCompletePreflight(t *testing.T) {
	first := time.Date(2026, time.August, 8, 1, 0, 0, 0, time.UTC)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodGet {
			writer.WriteHeader(http.StatusNotFound)
			return
		}
		if request.URL.Path == "/api/v1/meetings/meeting-next" {
			writeJSON(t, writer, meetingPayload("meeting-next", "ready", 2))
			return
		}
		if request.URL.Path != "/api/v1/meetings" {
			writer.WriteHeader(http.StatusNotFound)
			return
		}
		if request.URL.Query().Get("status") == "in_progress" {
			writeJSON(t, writer, map[string]any{
				"data":  []any{meetingPayload("meeting-old", "in_progress", 1)},
				"links": map[string]any{"next": nil},
			})
			return
		}
		writeJSON(t, writer, map[string]any{
			"data": []any{
				meetingPayload("meeting-current", "ready", 1),
				meetingPayload("meeting-next", "ready", 2),
			},
			"links": map[string]any{"next": nil},
		})
	}))
	defer server.Close()

	a := newTestApp(t, map[string]string{"room-a": "token-a"})
	a.rozetaBaseURL, a.httpClient = server.URL, server.Client()
	statePath := filepath.Join(t.TempDir(), "state.json")
	c := newTestController(t, a, statePath)
	c.schedule = meetingSchedule{enabled: true, starts: map[string]time.Time{
		"meeting-current": first,
		"meeting-next":    first.Add(time.Hour),
	}}
	updateTestDesired(t, c, "room-a", "meeting-current")
	_, run, generation := activateTestRoom(t, c, "room-a")

	result, err := c.advanceAndStart(context.Background(), "room-a")
	if err != nil {
		t.Fatal(err)
	}
	if result.MeetingID != "meeting-next" || result.Generation != generation+1 || result.Room.Lifecycle != "active" {
		t.Fatalf("advance result = %#v, want active next generation", result)
	}
	view := roomViewByName(t, c.snapshotRooms(), "room-a")
	if view.ReconciliationRun != run || view.DesiredMeetingID != "meeting-next" || view.ResumeConsumed {
		t.Fatalf("committed room = %#v", view)
	}
	file, err := loadDesiredStateFile(statePath)
	if err != nil || file.Rooms["room-a"].MeetingID != "meeting-next" {
		t.Fatalf("persisted state = %#v/%v", file, err)
	}
}

func TestAdvanceAndStartRejectsStoppingWithoutRemotePreflight(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		writer.WriteHeader(http.StatusBadGateway)
	}))
	defer server.Close()
	a := newTestApp(t, map[string]string{"room-a": "token-a"})
	a.rozetaBaseURL, a.httpClient = server.URL, server.Client()
	c := newTestController(t, a, filepath.Join(t.TempDir(), "state.json"))
	updateTestDesired(t, c, "room-a", "meeting-current")
	c.mu.Lock()
	c.rooms["room-a"].lifecycle = reconciliationStopping
	c.mu.Unlock()
	if _, err := c.advanceAndStart(context.Background(), "room-a"); !errors.Is(err, errRoomStopping) {
		t.Fatalf("stopping error = %v", err)
	}
	if requests.Load() != 0 {
		t.Fatalf("remote requests = %d, want none", requests.Load())
	}
}

func TestAdvanceAndStartRetriesTransientPreflightFailure(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		attempt := requests.Add(1)
		if attempt <= 2 {
			writer.WriteHeader(http.StatusBadGateway)
			return
		}
		if request.URL.Path == "/api/v1/meetings/meeting-next" {
			writeJSON(t, writer, meetingPayload("meeting-next", "ready", 2))
			return
		}
		data := []any{meetingPayload("meeting-current", "ready", 1), meetingPayload("meeting-next", "ready", 2)}
		if request.URL.Query().Get("status") == "in_progress" {
			data = []any{}
		}
		writeJSON(t, writer, map[string]any{"data": data, "links": map[string]any{"next": nil}})
	}))
	defer server.Close()

	a := newTestApp(t, map[string]string{"room-a": "token-a"})
	a.rozetaBaseURL, a.httpClient = server.URL, server.Client()
	c := newTestController(t, a, filepath.Join(t.TempDir(), "state.json"))
	start := time.Unix(1, 0)
	c.schedule = meetingSchedule{enabled: true, starts: map[string]time.Time{
		"meeting-current": start,
		"meeting-next":    start.Add(time.Hour),
	}}
	updateTestDesired(t, c, "room-a", "meeting-current")

	if _, err := c.advanceAndStart(context.Background(), "room-a"); err != nil {
		t.Fatal(err)
	}
	if requests.Load() < 5 {
		t.Fatalf("preflight requests = %d, want at least two failed attempts plus three successful reads", requests.Load())
	}
	view := roomViewByName(t, c.snapshotRooms(), "room-a")
	for _, condition := range view.Conditions {
		if condition.Type == "AdvanceAndStartReady" {
			t.Fatalf("successful advance retained alert = %#v", condition)
		}
	}
}

func TestReconcileStartsDesiredDespiteGotoFailureThenPausesOld(t *testing.T) {
	var mu sync.Mutex
	active := []string{"meeting-old"}
	actions := []string{}
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if request.Method == http.MethodGet && request.URL.Path == "/api/v1/meetings" {
			data := []any{}
			if request.URL.Query().Get("status") == "in_progress" {
				for _, id := range active {
					data = append(data, meetingPayload(id, "in_progress", 1))
				}
			} else {
				data = append(data, meetingPayload("meeting-old", "in_progress", 1), meetingPayload("meeting-new", "ready", 2))
			}
			writeJSON(t, writer, map[string]any{"data": data, "links": map[string]any{"next": nil}})
			return
		}
		if request.Method == http.MethodPost && request.URL.Path == "/api/v1/commands" {
			var command struct {
				Action   string `json:"action"`
				TargetID string `json:"target_id"`
			}
			if err := json.NewDecoder(request.Body).Decode(&command); err != nil {
				t.Fatal(err)
			}
			actions = append(actions, command.Action+":"+command.TargetID)
			switch command.Action {
			case "goto_meeting", "goto_meeting_embed":
				writer.WriteHeader(http.StatusBadGateway)
				return
			case "start_meeting":
				active = append(active, command.TargetID)
			case "pause_meeting":
				active = slices.DeleteFunc(active, func(id string) bool { return id == command.TargetID })
			}
			writeJSON(t, writer, map[string]any{"success": true})
			return
		}
		writer.WriteHeader(http.StatusNotFound)
	}))
	defer server.Close()

	a := newTestApp(t, map[string]string{"room-a": "token-a"})
	a.rozetaBaseURL, a.httpClient = server.URL, server.Client()
	c := newTestController(t, a, filepath.Join(t.TempDir(), "state.json"))
	updateTestDesired(t, c, "room-a", "meeting-new")
	room, run, _ := activateTestRoom(t, c, "room-a")

	c.reconcileRound(room, run)
	mu.Lock()
	firstActions := append([]string{}, actions...)
	firstActive := append([]string{}, active...)
	mu.Unlock()
	if !slices.Equal(firstActions, []string{
		"goto_meeting:meeting-new", "goto_meeting_embed:meeting-new", "start_meeting:meeting-new",
	}) || !slices.Equal(firstActive, []string{"meeting-old", "meeting-new"}) {
		t.Fatalf("first round actions/active = %#v/%#v", firstActions, firstActive)
	}

	c.reconcileRound(room, run)
	mu.Lock()
	secondActions := append([]string{}, actions...)
	secondActive := append([]string{}, active...)
	mu.Unlock()
	if !slices.Equal(secondActions, append(firstActions, "pause_meeting:meeting-old")) || !slices.Equal(secondActive, []string{"meeting-new"}) {
		t.Fatalf("second round actions/active = %#v/%#v", secondActions, secondActive)
	}
	c.reconcileRound(room, run)
	view := roomViewByName(t, c.snapshotRooms(), "room-a")
	if view.Summary != "Converged" || view.SummaryReason != "DesiredMeetingSoleInProgress" || !slices.Equal(view.ActiveMeetingIDs, []string{"meeting-new"}) {
		t.Fatalf("converged view = %#v", view)
	}
}

func TestMissingDesiredPreservesOldActiveMeeting(t *testing.T) {
	var commands atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/api/v1/commands" {
			commands.Add(1)
			writeJSON(t, writer, map[string]any{"success": true})
			return
		}
		if request.URL.Path == "/api/v1/meetings" {
			data := []any{meetingPayload("meeting-old", "in_progress", 1)}
			writeJSON(t, writer, map[string]any{"data": data, "links": map[string]any{"next": nil}})
			return
		}
		writer.WriteHeader(http.StatusNotFound)
	}))
	defer server.Close()
	a := newTestApp(t, map[string]string{"room-a": "token-a"})
	a.rozetaBaseURL, a.httpClient = server.URL, server.Client()
	c := newTestController(t, a, filepath.Join(t.TempDir(), "state.json"))
	updateTestDesired(t, c, "room-a", "meeting-missing")
	room, run, _ := activateTestRoom(t, c, "room-a")
	c.reconcileRound(room, run)
	view := roomViewByName(t, c.snapshotRooms(), "room-a")
	if commands.Load() != 0 || view.SummaryReason != "DesiredMeetingMissing" || !slices.Equal(view.ActiveMeetingIDs, []string{"meeting-old"}) {
		t.Fatalf("commands/view = %d/%#v", commands.Load(), view)
	}
}

func TestStopPausesUntilFreshActiveSetIsEmpty(t *testing.T) {
	var mu sync.Mutex
	active := []string{"meeting-a", "meeting-b"}
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if request.Method == http.MethodGet {
			data := make([]any, 0, len(active))
			for _, id := range active {
				data = append(data, meetingPayload(id, "in_progress", 1))
			}
			writeJSON(t, writer, map[string]any{"data": data, "links": map[string]any{"next": nil}})
			return
		}
		var command struct {
			Action   string `json:"action"`
			TargetID string `json:"target_id"`
		}
		if err := json.NewDecoder(request.Body).Decode(&command); err != nil {
			t.Fatal(err)
		}
		if command.Action == "pause_meeting" {
			active = slices.DeleteFunc(active, func(id string) bool { return id == command.TargetID })
		}
		writeJSON(t, writer, map[string]any{"success": true})
	}))
	defer server.Close()
	a := newTestApp(t, map[string]string{"room-a": "token-a"})
	a.rozetaBaseURL, a.httpClient = server.URL, server.Client()
	c := newTestController(t, a, filepath.Join(t.TempDir(), "state.json"))
	updateTestDesired(t, c, "room-a", "meeting-a")
	room, run, generation := activateTestRoom(t, c, "room-a")
	c.mu.Lock()
	room.lifecycle = reconciliationStopping
	room.stopDeadline = time.Now().Add(time.Second)
	c.mu.Unlock()
	for range 3 {
		c.reconcileRound(room, run)
	}
	view := roomViewByName(t, c.snapshotRooms(), "room-a")
	if generation != view.Generation || view.Lifecycle != "suspended" || view.SummaryReason != "LastStopConfirmedEmpty" || !view.ActiveSetStale {
		t.Fatalf("stopped view = %#v", view)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(active) != 0 {
		t.Fatalf("remaining active meetings = %#v", active)
	}
}

func TestStopAutomaticallyForceStopsThirtySecondPolicy(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.WriteHeader(http.StatusBadGateway)
	}))
	defer server.Close()
	a := newTestApp(t, map[string]string{"room-a": "token-a"})
	a.rozetaBaseURL, a.httpClient = server.URL, server.Client()
	c := newTestController(t, a, filepath.Join(t.TempDir(), "state.json"))
	c.stopTimeout = 20 * time.Millisecond
	view := updateTestDesired(t, c, "room-a", "meeting-a")
	_, _, err := c.reconcileLifecycle(a.epoch, "start", []reconciliationTarget{{
		RoomName: "room-a", ExpectedReconciliationRun: 0, ExpectedGeneration: view.Generation,
	}}, true)
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, time.Second, func() bool {
		return roomViewByName(t, c.snapshotRooms(), "room-a").Lifecycle == "active"
	})
	activeView := roomViewByName(t, c.snapshotRooms(), "room-a")
	acceptedAt := time.Now()
	_, _, err = c.reconcileLifecycle(a.epoch, "stop", []reconciliationTarget{{
		RoomName: "room-a", ExpectedReconciliationRun: activeView.ReconciliationRun, ExpectedGeneration: activeView.Generation,
	}}, true)
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, time.Second, func() bool {
		view := roomViewByName(t, c.snapshotRooms(), "room-a")
		return view.Lifecycle == "suspended" && view.SummaryReason == "RemoteOutcomeUnknown"
	})
	if elapsed := time.Since(acceptedAt); elapsed > 250*time.Millisecond {
		t.Fatalf("automatic force-stop took %v", elapsed)
	}
}

func TestStopFencesOldActiveRoundBeforeStartDispatch(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	var commands atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method == http.MethodGet && request.URL.Query().Get("status") == "in_progress" {
			close(entered)
			<-release
			writeJSON(t, writer, map[string]any{"data": []any{}, "links": map[string]any{"next": nil}})
			return
		}
		if request.Method == http.MethodGet {
			writeJSON(t, writer, map[string]any{
				"data": []any{meetingPayload("meeting-a", "ready", 1)}, "links": map[string]any{"next": nil},
			})
			return
		}
		commands.Add(1)
		writeJSON(t, writer, map[string]any{"success": true})
	}))
	defer server.Close()
	a := newTestApp(t, map[string]string{"room-a": "token-a"})
	a.rozetaBaseURL, a.httpClient = server.URL, server.Client()
	c := newTestController(t, a, filepath.Join(t.TempDir(), "state.json"))
	updateTestDesired(t, c, "room-a", "meeting-a")
	room, run, generation := activateTestRoom(t, c, "room-a")
	done := make(chan struct{})
	go func() {
		c.reconcileRound(room, run)
		close(done)
	}()
	<-entered
	_, _, err := c.reconcileLifecycle(a.epoch, "stop", []reconciliationTarget{{
		RoomName: "room-a", ExpectedReconciliationRun: run, ExpectedGeneration: generation,
	}}, true)
	if err != nil {
		t.Fatal(err)
	}
	close(release)
	<-done
	if commands.Load() != 0 {
		t.Fatalf("old active round dispatched %d commands after Stop", commands.Load())
	}
}

func TestResumeConsumedIsPersistedBeforeSingleDispatchAndRearm(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	var resumeCalls atomic.Int32
	var persistedBeforeDispatch atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch {
		case request.Method == http.MethodGet && request.URL.Path == "/api/v1/meetings":
			data := []any{}
			if request.URL.Query().Get("status") == "" {
				data = append(data, meetingPayload("meeting-a", "completed", 100))
			}
			writeJSON(t, writer, map[string]any{"data": data, "links": map[string]any{"next": nil}})
		case request.Method == http.MethodPost && request.URL.Path == "/api/v1/commands":
			writeJSON(t, writer, map[string]any{"success": true})
		case request.Method == http.MethodPost && request.URL.Path == "/api/v1/meetings/meeting-a/resume":
			resumeCalls.Add(1)
			file, err := loadDesiredStateFile(path)
			if err == nil && file.Rooms["room-a"].ConsumedResume != nil {
				persistedBeforeDispatch.Store(true)
			}
			writeJSON(t, writer, meetingPayload("meeting-a", "ready", 101))
		default:
			writer.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()
	a := newTestApp(t, map[string]string{"room-a": "token-a"})
	a.rozetaBaseURL, a.httpClient = server.URL, server.Client()
	c := newTestController(t, a, path)
	updateTestDesired(t, c, "room-a", "meeting-a")
	room, run, _ := activateTestRoom(t, c, "room-a")
	c.reconcileRound(room, run)
	c.reconcileRound(room, run)
	view := roomViewByName(t, c.snapshotRooms(), "room-a")
	if resumeCalls.Load() != 1 || !persistedBeforeDispatch.Load() || !view.ResumeConsumed || view.SummaryReason != "ResumeLimitReached" {
		t.Fatalf("resume calls/persisted/view = %d/%t/%#v", resumeCalls.Load(), persistedBeforeDispatch.Load(), view)
	}

	c.mu.Lock()
	room.lifecycle = reconciliationSuspended
	if room.runCancel != nil {
		room.runCancel()
	}
	c.mu.Unlock()
	rearmed, err := c.updateDesired("room-a", desiredStateUpdate{
		MeetingID: "meeting-a", ExpectedEpoch: a.epoch, ExpectedRun: run,
		ExpectedGeneration: view.Generation, Rearm: true, ConfirmDestructiveResume: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if rearmed.Generation != view.Generation+1 || rearmed.ResumeConsumed {
		t.Fatalf("rearmed state = %#v", rearmed)
	}
	c.mu.RLock()
	rearmAuthorization := room.resumeAuthorized
	c.mu.RUnlock()
	if rearmAuthorization != rearmed.Generation {
		t.Fatalf("rearm authorized generation = %d, want %d", rearmAuthorization, rearmed.Generation)
	}
	file, err := loadDesiredStateFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if desired := file.Rooms["room-a"]; desired.Generation != rearmed.Generation || desired.ConsumedResume != nil {
		t.Fatalf("persisted rearm = %#v", desired)
	}
}

func TestStartPreflightPartiallyAppliesObservableRooms(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Header.Get("Authorization") == "Bearer token-b" {
			writer.WriteHeader(http.StatusBadGateway)
			return
		}
		if request.URL.Path == "/api/v1/meetings/meeting-a" {
			writeJSON(t, writer, meetingPayload("meeting-a", "ready", 1))
			return
		}
		if request.URL.Path == "/api/v1/meetings" {
			data := []any{}
			if request.URL.Query().Get("status") == "" {
				data = append(data, meetingPayload("meeting-a", "ready", 1))
			}
			writeJSON(t, writer, map[string]any{"data": data, "links": map[string]any{"next": nil}})
			return
		}
		if request.URL.Path == "/api/v1/commands" {
			writeJSON(t, writer, map[string]any{"success": true})
			return
		}
		writer.WriteHeader(http.StatusNotFound)
	}))
	defer server.Close()
	a := newTestApp(t, map[string]string{"room-a": "token-a", "room-b": "token-b"})
	a.rozetaBaseURL, a.httpClient = server.URL, server.Client()
	c := newTestController(t, a, filepath.Join(t.TempDir(), "state.json"))
	aView := updateTestDesired(t, c, "room-a", "meeting-a")
	bView := updateTestDesired(t, c, "room-b", "meeting-b")
	rooms, results, err := c.confirmedLifecycle(context.Background(), a.epoch, "start", []reconciliationTarget{
		{RoomName: "room-a", ExpectedReconciliationRun: 0, ExpectedGeneration: aView.Generation, Preflight: startPreflightFacts(false)},
		{RoomName: "room-b", ExpectedReconciliationRun: 0, ExpectedGeneration: bView.Generation},
	}, true)
	if err != nil {
		t.Fatal(err)
	}
	resultByRoom := make(map[string]reconciliationResult, len(results))
	for _, result := range results {
		resultByRoom[result.RoomName] = result
	}
	if !resultByRoom["room-a"].Applied || resultByRoom["room-b"].Applied || resultByRoom["room-b"].Error == "" {
		t.Fatalf("partial results = %#v", results)
	}
	if roomViewByName(t, rooms, "room-b").Lifecycle != "suspended" {
		t.Fatalf("unobservable room changed: %#v", rooms)
	}
}

func TestOptimisticBulkConflictIsAtomicAndDoesNoPreflight(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		requests.Add(1)
	}))
	defer server.Close()
	a := newTestApp(t, map[string]string{"room-a": "token-a", "room-b": "token-b"})
	a.rozetaBaseURL, a.httpClient = server.URL, server.Client()
	c := newTestController(t, a, filepath.Join(t.TempDir(), "state.json"))
	aView := updateTestDesired(t, c, "room-a", "meeting-a")
	bView := updateTestDesired(t, c, "room-b", "meeting-b")
	rooms, _, err := c.lifecyclePreflight(context.Background(), a.epoch, "start", []reconciliationTarget{
		{RoomName: "room-a", ExpectedReconciliationRun: 0, ExpectedGeneration: aView.Generation},
		{RoomName: "room-b", ExpectedReconciliationRun: 99, ExpectedGeneration: bView.Generation},
	})
	if !errors.Is(err, errReconciliationConflict) || requests.Load() != 0 {
		t.Fatalf("error/requests = %v/%d", err, requests.Load())
	}
	for _, room := range rooms {
		if room.Lifecycle != "suspended" || room.ReconciliationRun != 0 {
			t.Fatalf("room changed after conflict: %#v", room)
		}
	}
}

func TestForceStopRequiresConfirmationAndBypassesRemotePreflight(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		requests.Add(1)
	}))
	defer server.Close()
	a := newTestApp(t, map[string]string{"room-a": "token-a"})
	a.rozetaBaseURL, a.httpClient = server.URL, server.Client()
	c := newTestController(t, a, filepath.Join(t.TempDir(), "state.json"))
	updateTestDesired(t, c, "room-a", "meeting-a")
	room, run, generation := activateTestRoom(t, c, "room-a")
	c.mu.Lock()
	room.lifecycle = reconciliationStopping
	room.stopDeadline = time.Now().Add(time.Second)
	c.mu.Unlock()
	targets := []reconciliationTarget{{RoomName: "room-a", ExpectedReconciliationRun: run, ExpectedGeneration: generation}}
	if _, _, err := c.confirmedLifecycle(context.Background(), a.epoch, "force-stop", targets, false); !errors.Is(err, errConfirmationRequired) {
		t.Fatalf("unconfirmed force-stop error = %v", err)
	}
	_, results, err := c.confirmedLifecycle(context.Background(), a.epoch, "force-stop", targets, true)
	if err != nil || len(results) != 1 || !results[0].Applied {
		t.Fatalf("force-stop result/error = %#v/%v", results, err)
	}
	view := roomViewByName(t, c.snapshotRooms(), "room-a")
	if requests.Load() != 0 || view.Lifecycle != "suspended" || view.SummaryReason != "RemoteOutcomeUnknown" {
		t.Fatalf("requests/view = %d/%#v", requests.Load(), view)
	}
}

func TestActorHandlesStopAcceptedBeforeStartingTransition(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writeJSON(t, writer, map[string]any{"data": []any{}, "links": map[string]any{"next": nil}})
	}))
	defer server.Close()
	a := newTestApp(t, map[string]string{"room-a": "token-a"})
	a.rozetaBaseURL, a.httpClient = server.URL, server.Client()
	c := newTestController(t, a, filepath.Join(t.TempDir(), "state.json"))
	view := updateTestDesired(t, c, "room-a", "meeting-a")
	room := c.rooms["room-a"]
	c.mu.Lock()
	room.reconciliationRun = 1
	room.lifecycle = reconciliationStarting
	room.resumeAuthorized = view.Generation
	room.runCtx, room.runCancel = context.WithCancel(c.ctx)
	runCtx := room.runCtx
	c.mu.Unlock()
	_, results, err := c.reconcileLifecycle(a.epoch, "stop", []reconciliationTarget{{
		RoomName: "room-a", ExpectedReconciliationRun: 1, ExpectedGeneration: view.Generation,
	}}, true)
	if err != nil || !results[0].Applied {
		t.Fatalf("immediate Stop result/error = %#v/%v", results, err)
	}
	c.wg.Go(func() { c.runRoom(room, 1, runCtx) })
	waitFor(t, time.Second, func() bool {
		stopped := roomViewByName(t, c.snapshotRooms(), "room-a")
		return stopped.Lifecycle == "suspended" && stopped.SummaryReason == "LastStopConfirmedEmpty"
	})
}

func TestActorSurvivesDesiredUpdateBeforeStartingTransition(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method == http.MethodGet && request.URL.Path == "/api/v1/meetings/meeting-b" {
			writeJSON(t, writer, meetingPayload("meeting-b", "ready", 1))
			return
		}
		writer.WriteHeader(http.StatusNotFound)
	}))
	defer server.Close()
	a := newTestApp(t, map[string]string{"room-a": "token-a"})
	a.rozetaBaseURL, a.httpClient = server.URL, server.Client()
	c := newTestController(t, a, filepath.Join(t.TempDir(), "state.json"))
	initial := updateTestDesired(t, c, "room-a", "meeting-a")
	room := c.rooms["room-a"]
	runCtx, runCancel := context.WithCancel(c.ctx)
	c.mu.Lock()
	room.reconciliationRun = 1
	room.lifecycle = reconciliationStarting
	room.runCtx, room.runCancel = runCtx, runCancel
	c.mu.Unlock()

	updated, err := c.updateDesired("room-a", desiredStateUpdate{
		MeetingID: "meeting-b", ExpectedEpoch: a.epoch,
		ExpectedRun: 1, ExpectedGeneration: initial.Generation,
	})
	if err != nil {
		t.Fatal(err)
	}
	runCancel()
	c.runRoom(room, 1, runCtx)

	view := roomViewByName(t, c.snapshotRooms(), "room-a")
	if updated.Generation != initial.Generation+1 || view.Lifecycle != string(reconciliationActive) {
		t.Fatalf("updated/view = %#v/%#v", updated, view)
	}
}

func TestForceStopWatchdogFiresWhileActorRequestIsBlocked(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method == http.MethodGet && request.URL.Query().Get("status") == "in_progress" {
			close(entered)
			<-release
			writeJSON(t, writer, map[string]any{"data": []any{}, "links": map[string]any{"next": nil}})
			return
		}
		writer.WriteHeader(http.StatusNotFound)
	}))
	defer server.Close()
	a := newTestApp(t, map[string]string{"room-a": "token-a"})
	a.rozetaBaseURL, a.httpClient = server.URL, server.Client()
	c := newTestController(t, a, filepath.Join(t.TempDir(), "state.json"))
	c.stopTimeout = 20 * time.Millisecond
	view := updateTestDesired(t, c, "room-a", "meeting-a")
	_, _, err := c.reconcileLifecycle(a.epoch, "start", []reconciliationTarget{{
		RoomName: "room-a", ExpectedReconciliationRun: 0, ExpectedGeneration: view.Generation,
	}}, true)
	if err != nil {
		t.Fatal(err)
	}
	<-entered
	active := roomViewByName(t, c.snapshotRooms(), "room-a")
	acceptedAt := time.Now()
	_, _, err = c.reconcileLifecycle(a.epoch, "stop", []reconciliationTarget{{
		RoomName: "room-a", ExpectedReconciliationRun: active.ReconciliationRun, ExpectedGeneration: active.Generation,
	}}, true)
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, 250*time.Millisecond, func() bool {
		stopped := roomViewByName(t, c.snapshotRooms(), "room-a")
		return stopped.Lifecycle == "suspended" && stopped.SummaryReason == "RemoteOutcomeUnknown"
	})
	if elapsed := time.Since(acceptedAt); elapsed > 150*time.Millisecond {
		t.Fatalf("blocked actor delayed watchdog for %v", elapsed)
	}
	close(release)
}

func TestConfirmedStartRejectsReadyToCompletedPreflightChange(t *testing.T) {
	var detailCalls atomic.Int32
	var commands atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch {
		case request.URL.Path == "/api/v1/meetings/meeting-a":
			status := "ready"
			if detailCalls.Add(1) > 1 {
				status = "completed"
			}
			writeJSON(t, writer, meetingPayload("meeting-a", status, 10))
		case request.URL.Path == "/api/v1/meetings":
			writeJSON(t, writer, map[string]any{"data": []any{}, "links": map[string]any{"next": nil}})
		case request.URL.Path == "/api/v1/commands":
			commands.Add(1)
			writeJSON(t, writer, map[string]any{"success": true})
		default:
			writer.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()
	a := newTestApp(t, map[string]string{"room-a": "token-a"})
	a.rozetaBaseURL, a.httpClient = server.URL, server.Client()
	c := newTestController(t, a, filepath.Join(t.TempDir(), "state.json"))
	view := updateTestDesired(t, c, "room-a", "meeting-a")
	target := reconciliationTarget{RoomName: "room-a", ExpectedGeneration: view.Generation}
	_, preflight, err := c.lifecyclePreflight(context.Background(), a.epoch, "start", []reconciliationTarget{target})
	if err != nil || len(preflight) != 1 || preflight[0].DestructiveResume {
		t.Fatalf("initial preflight/error = %#v/%v", preflight, err)
	}
	target.Preflight = startPreflightFacts(preflight[0].DestructiveResume)
	_, _, err = c.confirmedLifecycle(context.Background(), a.epoch, "start", []reconciliationTarget{target}, true)
	if !errors.Is(err, errPreflightChanged) {
		t.Fatalf("confirmation error = %v, want preflight changed", err)
	}
	if commands.Load() != 0 || roomViewByName(t, c.snapshotRooms(), "room-a").Lifecycle != "suspended" {
		t.Fatalf("changed risk dispatched commands or lifecycle: %d/%#v", commands.Load(), c.snapshotRooms())
	}
}

func TestConfirmedStopRejectsChangedActiveMeetingIDs(t *testing.T) {
	var observations atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		id := "meeting-old"
		if observations.Add(1) > 1 {
			id = "meeting-new"
		}
		writeJSON(t, writer, map[string]any{
			"data": []any{meetingPayload(id, "in_progress", 1)}, "links": map[string]any{"next": nil},
		})
	}))
	defer server.Close()
	a := newTestApp(t, map[string]string{"room-a": "token-a"})
	a.rozetaBaseURL, a.httpClient = server.URL, server.Client()
	c := newTestController(t, a, filepath.Join(t.TempDir(), "state.json"))
	updateTestDesired(t, c, "room-a", "meeting-a")
	_, run, generation := activateTestRoom(t, c, "room-a")
	target := reconciliationTarget{RoomName: "room-a", ExpectedReconciliationRun: run, ExpectedGeneration: generation}
	_, preflight, err := c.lifecyclePreflight(context.Background(), a.epoch, "stop", []reconciliationTarget{target})
	if err != nil {
		t.Fatal(err)
	}
	target.Preflight = stopPreflightFacts(preflight[0].ActiveMeetingIDs)
	_, _, err = c.confirmedLifecycle(context.Background(), a.epoch, "stop", []reconciliationTarget{target}, true)
	if !errors.Is(err, errPreflightChanged) || roomViewByName(t, c.snapshotRooms(), "room-a").Lifecycle != "active" {
		t.Fatalf("confirmation error/rooms = %v/%#v", err, c.snapshotRooms())
	}
}

func TestGotoTimeoutStillAttemptsEmbedAndStartInSameRound(t *testing.T) {
	var mu sync.Mutex
	actions := []string{}
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method == http.MethodGet && request.URL.Path == "/api/v1/meetings" {
			data := []any{}
			if request.URL.Query().Get("status") == "" {
				data = append(data, meetingPayload("meeting-a", "ready", 1))
			}
			writeJSON(t, writer, map[string]any{"data": data, "links": map[string]any{"next": nil}})
			return
		}
		var command struct {
			Action string `json:"action"`
		}
		if err := json.NewDecoder(request.Body).Decode(&command); err != nil {
			writer.WriteHeader(http.StatusBadRequest)
			return
		}
		mu.Lock()
		actions = append(actions, command.Action)
		mu.Unlock()
		if command.Action == "goto_meeting" {
			<-request.Context().Done()
			return
		}
		writeJSON(t, writer, map[string]any{"success": true})
	}))
	defer server.Close()
	a := newTestApp(t, map[string]string{"room-a": "token-a"})
	a.rozetaBaseURL, a.httpClient = server.URL, server.Client()
	c := newTestController(t, a, filepath.Join(t.TempDir(), "state.json"))
	c.controlTimeout = 20 * time.Millisecond
	updateTestDesired(t, c, "room-a", "meeting-a")
	room, run, _ := activateTestRoom(t, c, "room-a")
	c.reconcileRound(room, run)
	mu.Lock()
	defer mu.Unlock()
	if !slices.Equal(actions, []string{"goto_meeting", "goto_meeting_embed", "start_meeting"}) {
		t.Fatalf("actions = %#v", actions)
	}
}

func TestFreshActiveExclusionClearsStaleDesiredStatusBeforeFullReadFailure(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Query().Get("status") == "in_progress" {
			writeJSON(t, writer, map[string]any{"data": []any{}, "links": map[string]any{"next": nil}})
			return
		}
		writer.WriteHeader(http.StatusBadGateway)
	}))
	defer server.Close()
	a := newTestApp(t, map[string]string{"room-a": "token-a"})
	a.rozetaBaseURL, a.httpClient = server.URL, server.Client()
	c := newTestController(t, a, filepath.Join(t.TempDir(), "state.json"))
	updateTestDesired(t, c, "room-a", "meeting-a")
	room, run, _ := activateTestRoom(t, c, "room-a")
	c.mu.Lock()
	room.desiredStatus = "in_progress"
	c.mu.Unlock()
	c.reconcileRound(room, run)
	view := roomViewByName(t, c.snapshotRooms(), "room-a")
	if view.DesiredStatus != "unknown" || view.SummaryReason != "DesiredMeetingObservationFailed" || view.ActiveSetStale {
		t.Fatalf("fresh exclusion view = %#v", view)
	}
}

func TestStopLinearizesAfterDispatchedRequestAndBeforeLaterCommands(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		close(entered)
		<-release
		writeJSON(t, writer, map[string]any{"success": true})
	}))
	defer server.Close()
	a := newTestApp(t, map[string]string{"room-a": "token-a"})
	a.rozetaBaseURL, a.httpClient = server.URL, server.Client()
	c := newTestController(t, a, filepath.Join(t.TempDir(), "state.json"))
	c.stopTimeout = time.Second
	updateTestDesired(t, c, "room-a", "meeting-a")
	room, run, generation := activateTestRoom(t, c, "room-a")
	done := make(chan struct{})
	go func() {
		_ = c.dispatchControl(room, run, generation, reconciliationActive, "start_meeting", "meeting-a")
		close(done)
	}()
	<-entered
	_, _, err := c.reconcileLifecycle(a.epoch, "stop", []reconciliationTarget{{
		RoomName: "room-a", ExpectedReconciliationRun: run, ExpectedGeneration: generation,
	}}, true)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.dispatchControl(room, run, generation, reconciliationActive, "start_meeting", "meeting-a"); !errors.Is(err, context.Canceled) {
		t.Fatalf("post-Stop command error = %v", err)
	}
	if requests.Load() != 1 {
		t.Fatalf("requests after Stop = %d, want only already-dispatched request", requests.Load())
	}
	close(release)
	<-done
}

func TestActiveReadySwitchBlocksLaterAutomaticResume(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	var resumeCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch {
		case request.Method == http.MethodGet && request.URL.Path == "/api/v1/meetings/meeting-b":
			writeJSON(t, writer, meetingPayload("meeting-b", "ready", 1))
		case request.Method == http.MethodGet && request.URL.Path == "/api/v1/meetings":
			data := []any{}
			if request.URL.Query().Get("status") == "" {
				data = append(data, meetingPayload("meeting-b", "completed", 2))
			}
			writeJSON(t, writer, map[string]any{"data": data, "links": map[string]any{"next": nil}})
		case request.Method == http.MethodPost && request.URL.Path == "/api/v1/meetings/meeting-b/resume":
			resumeCalls.Add(1)
			writeJSON(t, writer, meetingPayload("meeting-b", "ready", 3))
		case request.Method == http.MethodPost && request.URL.Path == "/api/v1/commands":
			writeJSON(t, writer, map[string]any{"success": true})
		default:
			writer.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()
	a := newTestApp(t, map[string]string{"room-a": "token-a"})
	a.rozetaBaseURL, a.httpClient = server.URL, server.Client()
	c := newTestController(t, a, path)
	updateTestDesired(t, c, "room-a", "meeting-a")
	room, run, generation := activateTestRoom(t, c, "room-a")
	view, err := c.updateDesired("room-a", desiredStateUpdate{
		MeetingID: "meeting-b", ExpectedEpoch: a.epoch, ExpectedRun: run, ExpectedGeneration: generation,
	})
	if err != nil {
		t.Fatal(err)
	}
	c.reconcileRound(room, run)
	blocked := roomViewByName(t, c.snapshotRooms(), "room-a")
	if resumeCalls.Load() != 0 || blocked.SummaryReason != "ResumeAuthorizationRequired" {
		t.Fatalf("resume calls/blocked view = %d/%#v", resumeCalls.Load(), blocked)
	}
	c.mu.RLock()
	authorizedGeneration := room.resumeAuthorized
	c.mu.RUnlock()
	if authorizedGeneration != 0 {
		t.Fatalf("ready switch authorized generation = %d", authorizedGeneration)
	}
	file, err := loadDesiredStateFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if consumed := file.Rooms["room-a"].ConsumedResume; consumed != nil {
		t.Fatalf("ready switch persisted unexpected consumption = %#v for view %#v", consumed, view)
	}
}

func TestActivePausedSwitchDoesNotAuthorizeResume(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/api/v1/meetings/meeting-b" {
			writeJSON(t, writer, meetingPayload("meeting-b", "paused", 1))
			return
		}
		writer.WriteHeader(http.StatusNotFound)
	}))
	defer server.Close()
	a := newTestApp(t, map[string]string{"room-a": "token-a"})
	a.rozetaBaseURL, a.httpClient = server.URL, server.Client()
	c := newTestController(t, a, filepath.Join(t.TempDir(), "state.json"))
	updateTestDesired(t, c, "room-a", "meeting-a")
	room, run, generation := activateTestRoom(t, c, "room-a")
	view, err := c.updateDesired("room-a", desiredStateUpdate{
		MeetingID: "meeting-b", ExpectedEpoch: a.epoch, ExpectedRun: run, ExpectedGeneration: generation,
		ConfirmDestructiveResume: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	c.mu.RLock()
	authorizedGeneration := room.resumeAuthorized
	c.mu.RUnlock()
	if authorizedGeneration != 0 {
		t.Fatalf("paused switch authorized generation = %d for %#v", authorizedGeneration, view)
	}
}

func TestActiveDesiredChangeRequiresCompletedConfirmationAndFencesGeneration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	var resumeCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch {
		case request.Method == http.MethodGet && request.URL.Path == "/api/v1/meetings/meeting-b":
			writeJSON(t, writer, meetingPayload("meeting-b", "completed", 10))
		case request.Method == http.MethodGet && request.URL.Path == "/api/v1/meetings":
			data := []any{}
			if request.URL.Query().Get("status") == "" {
				data = append(data, meetingPayload("meeting-b", "completed", 10))
			}
			writeJSON(t, writer, map[string]any{"data": data, "links": map[string]any{"next": nil}})
		case request.Method == http.MethodPost && request.URL.Path == "/api/v1/meetings/meeting-b/resume":
			resumeCalls.Add(1)
			writeJSON(t, writer, meetingPayload("meeting-b", "ready", 11))
		case request.Method == http.MethodPost && request.URL.Path == "/api/v1/commands":
			writeJSON(t, writer, map[string]any{"success": true})
		default:
			writer.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()
	a := newTestApp(t, map[string]string{"room-a": "token-a"})
	a.rozetaBaseURL, a.httpClient = server.URL, server.Client()
	c := newTestController(t, a, path)
	updateTestDesired(t, c, "room-a", "meeting-a")
	room, run, generation := activateTestRoom(t, c, "room-a")
	update := desiredStateUpdate{
		MeetingID: "meeting-b", ExpectedEpoch: a.epoch, ExpectedRun: run, ExpectedGeneration: generation,
	}
	if _, err := c.updateDesired("room-a", update); !errors.Is(err, errDestructiveConfirmation) {
		t.Fatalf("unconfirmed update error = %v", err)
	}
	update.ConfirmDestructiveResume = true
	view, err := c.updateDesired("room-a", update)
	if err != nil {
		t.Fatal(err)
	}
	if view.Generation != generation+1 || view.DesiredMeetingID != "meeting-b" {
		t.Fatalf("updated desired = %#v", view)
	}
	c.mu.RLock()
	authorizedGeneration := c.rooms["room-a"].resumeAuthorized
	c.mu.RUnlock()
	if authorizedGeneration != view.Generation {
		t.Fatalf("confirmed completed switch authorized generation = %d, want %d", authorizedGeneration, view.Generation)
	}
	c.reconcileRound(room, run)
	if resumeCalls.Load() != 1 {
		t.Fatalf("confirmed completed switch resume calls = %d", resumeCalls.Load())
	}
	file, err := loadDesiredStateFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if consumed := file.Rooms["room-a"].ConsumedResume; consumed == nil || consumed.Generation != view.Generation {
		t.Fatalf("confirmed completed switch consumption = %#v", consumed)
	}
	if _, err := c.updateDesired("room-a", update); !errors.Is(err, errGenerationConflict) {
		t.Fatalf("stale generation error = %v", err)
	}
}

func TestHTTPJSONContractUsesEpochRunGenerationAndAuthoritativeSnapshots(t *testing.T) {
	gin.SetMode(gin.TestMode)
	a := newTestApp(t, map[string]string{"room-a": "token-a"})
	c := newTestController(t, a, filepath.Join(t.TempDir(), "state.json"))
	view := updateTestDesired(t, c, "room-a", "meeting-a")
	data, err := json.Marshal(roomsResponse{Epoch: a.epoch, Rooms: c.snapshotRooms()})
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{
		`"epoch"`, `"rooms"`, `"reconciliation_run"`, `"generation"`, `"revision"`,
		`"lifecycle"`, `"active_meeting_ids":[]`, `"active_set_stale"`, `"conditions"`, `"recent_actions"`,
	} {
		if !bytes.Contains(data, []byte(field)) {
			t.Fatalf("rooms response %s omitted %s", data, field)
		}
	}
	for _, removed := range []string{"online_count", "navigation_ready", "desired_running", "waiting_for_clients"} {
		if bytes.Contains(bytes.ToLower(data), []byte(removed)) {
			t.Fatalf("rooms response retained removed field %q: %s", removed, data)
		}
	}
	websocketSnapshot, err := json.Marshal(adminEnvelope{Type: "snapshot", Epoch: a.epoch, Rooms: c.snapshotRooms()})
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(websocketSnapshot, []byte(`"room":`)) {
		t.Fatalf("full WebSocket snapshot contains a fabricated room: %s", websocketSnapshot)
	}
	confirmationJSON, err := json.Marshal(bulkReconciliationRequest{
		Epoch: a.epoch,
		Rooms: []reconciliationTarget{{
			RoomName: "room-a", ExpectedReconciliationRun: view.ReconciliationRun, ExpectedGeneration: view.Generation,
			Preflight: startPreflightFacts(false),
		}},
		Confirmed: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(confirmationJSON, []byte(`"preflight":{"destructive_resume":false}`)) {
		t.Fatalf("confirmation request contract = %s", confirmationJSON)
	}

	router := gin.New()
	router.POST("/api/reconciliation/:action", a.handleBulkReconciliation)
	request := httptest.NewRequest(http.MethodPost, "/api/reconciliation/start", strings.NewReader(fmt.Sprintf(
		`{"epoch":"stale","rooms":[{"room_name":"room-a","expected_reconciliation_run":%d,"expected_generation":%d}],"confirmed":true}`,
		view.ReconciliationRun, view.Generation,
	)))
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	if response.Code != http.StatusConflict {
		t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
	}
	var conflict struct {
		Error string     `json:"error"`
		Epoch string     `json:"epoch"`
		Rooms []roomView `json:"rooms"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &conflict); err != nil {
		t.Fatal(err)
	}
	if conflict.Epoch != a.epoch || len(conflict.Rooms) != 1 || conflict.Rooms[0].Generation != view.Generation {
		t.Fatalf("conflict payload = %#v", conflict)
	}
}
