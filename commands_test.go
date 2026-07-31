package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
)

func TestHandleCommandRejectsUnsupportedAction(t *testing.T) {
	gin.SetMode(gin.TestMode)
	a := newTestApp(t, map[string]string{"room-a": "token-a"})
	router := gin.New()
	router.POST("/api/rooms/:roomName/commands", a.handleCommand)

	req := httptest.NewRequest(http.MethodPost, "/api/rooms/room-a/commands", bytes.NewBufferString(`{"action":"goto_and_start"}`))
	req.Header.Set("content-type", "application/json")
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, req)

	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusBadRequest)
	}
}

func TestStartCommandConfirmsExpectedMeetingStatus(t *testing.T) {
	gin.SetMode(gin.TestMode)
	var commandCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Header.Get("Authorization") != "Bearer token-a" {
			t.Errorf("Authorization = %q", request.Header.Get("Authorization"))
		}
		switch request.URL.Path {
		case "/api/v1/commands":
			commandCalls.Add(1)
			var body map[string]string
			if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
				t.Errorf("decode command: %v", err)
			}
			if body["action"] != "start_meeting" || body["target_id"] != "meeting-a" {
				t.Errorf("command body = %#v", body)
			}
			writer.Header().Set("content-type", "application/json")
			_, _ = writer.Write([]byte(`{"success":true}`))
		case "/api/v1/meetings/meeting-a":
			writer.Header().Set("content-type", "application/json")
			_, _ = writer.Write([]byte(`{"id":"meeting-a","title":"Meeting","status":"in_progress","languages":{"source":"en","target":"zh-TW"}}`))
		default:
			writer.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	a := newTestApp(t, map[string]string{"room-a": "token-a"})
	a.rozetaBaseURL = server.URL
	a.httpClient = server.Client()
	a.state.applyMeetingSync("room-a", []roomMeetingView{{ID: "meeting-a", Status: "paused"}})
	router := gin.New()
	router.POST("/api/rooms/:roomName/commands", a.handleCommand)

	req := httptest.NewRequest(http.MethodPost, "/api/rooms/room-a/commands", bytes.NewBufferString(`{"action":"start"}`))
	req.Header.Set("content-type", "application/json")
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, req)
	if recorder.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want %d: %s", recorder.Code, http.StatusAccepted, recorder.Body.String())
	}

	waitForRoomResult(t, a, "room-a", "confirmed")
	room, _ := a.state.snapshotRoom("room-a")
	if room.Status != "in_progress" {
		t.Fatalf("room status = %q, want in_progress", room.Status)
	}
	if commandCalls.Load() != 1 {
		t.Fatalf("command calls = %d, want 1", commandCalls.Load())
	}
}

func TestCommandAPIFailureStillSucceedsWhenStatusMatches(t *testing.T) {
	gin.SetMode(gin.TestMode)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/api/v1/commands" {
			writer.WriteHeader(http.StatusBadGateway)
			return
		}
		writer.Header().Set("content-type", "application/json")
		_, _ = writer.Write([]byte(`{"id":"meeting-a","title":"Meeting","status":"paused","languages":{"source":"en","target":"zh-TW"}}`))
	}))
	defer server.Close()

	a := newTestApp(t, map[string]string{"room-a": "token-a"})
	a.rozetaBaseURL = server.URL
	a.httpClient = server.Client()
	a.state.applyMeetingSync("room-a", []roomMeetingView{{ID: "meeting-a", Status: "in_progress"}})
	router := gin.New()
	router.POST("/api/rooms/:roomName/commands", a.handleCommand)

	req := httptest.NewRequest(http.MethodPost, "/api/rooms/room-a/commands", bytes.NewBufferString(`{"action":"pause"}`))
	req.Header.Set("content-type", "application/json")
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, req)
	waitForRoomResult(t, a, "room-a", "confirmed")
}

func TestResumeVerifiesCompletedMeetingAndConfirmsReady(t *testing.T) {
	gin.SetMode(gin.TestMode)
	var resumed atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("content-type", "application/json")
		switch {
		case request.Method == http.MethodPost && request.URL.Path == "/api/v1/meetings/meeting-a/resume":
			resumed.Store(true)
			_, _ = writer.Write([]byte(`{"id":"meeting-a","title":"Meeting","status":"ready","languages":{"source":"en","target":"zh-TW"}}`))
		case request.Method == http.MethodGet && request.URL.Path == "/api/v1/meetings/meeting-a":
			status := "completed"
			if resumed.Load() {
				status = "ready"
			}
			_, _ = writer.Write([]byte(`{"id":"meeting-a","title":"Meeting","status":"` + status + `","languages":{"source":"en","target":"zh-TW"}}`))
		default:
			writer.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	a := newTestApp(t, map[string]string{"room-a": "token-a"})
	a.rozetaBaseURL = server.URL
	a.httpClient = server.Client()
	a.state.applyMeetingSync("room-a", []roomMeetingView{{ID: "meeting-a", Status: "completed"}})
	router := gin.New()
	router.POST("/api/rooms/:roomName/commands", a.handleCommand)

	request := httptest.NewRequest(http.MethodPost, "/api/rooms/room-a/commands", bytes.NewBufferString(`{"action":"resume","target_meeting_id":"meeting-a"}`))
	request.Header.Set("content-type", "application/json")
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want %d: %s", recorder.Code, http.StatusAccepted, recorder.Body.String())
	}
	waitForRoomResult(t, a, "room-a", "confirmed")
	if !resumed.Load() {
		t.Fatal("resume endpoint was not called")
	}
}

func TestGotoCompletesWhenRozetaAcceptsCommand(t *testing.T) {
	gin.SetMode(gin.TestMode)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/api/v1/commands" {
			writer.WriteHeader(http.StatusNotFound)
			return
		}
		writer.Header().Set("content-type", "application/json")
		_, _ = writer.Write([]byte(`{"success":true}`))
	}))
	defer server.Close()

	a := newTestApp(t, map[string]string{"room-a": "token-a"})
	a.rozetaBaseURL = server.URL
	a.httpClient = server.Client()
	router := gin.New()
	router.POST("/api/rooms/:roomName/commands", a.handleCommand)

	request := httptest.NewRequest(http.MethodPost, "/api/rooms/room-a/commands", bytes.NewBufferString(`{"action":"goto","target_meeting_id":"meeting-a"}`))
	request.Header.Set("content-type", "application/json")
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusAccepted)
	}
	waitForRoomResult(t, a, "room-a", "confirmed")
	room, _ := a.state.snapshotRoom("room-a")
	if room.CurrentMeetingID != "meeting-a" {
		t.Fatalf("current meeting = %q, want meeting-a", room.CurrentMeetingID)
	}
}

func TestResumeRejectsMeetingThatIsNotCompleted(t *testing.T) {
	gin.SetMode(gin.TestMode)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("content-type", "application/json")
		_, _ = writer.Write([]byte(`{"id":"meeting-a","title":"Meeting","status":"ready","languages":{"source":"en","target":"zh-TW"}}`))
	}))
	defer server.Close()

	a := newTestApp(t, map[string]string{"room-a": "token-a"})
	a.rozetaBaseURL = server.URL
	a.httpClient = server.Client()
	router := gin.New()
	router.POST("/api/rooms/:roomName/commands", a.handleCommand)
	request := httptest.NewRequest(http.MethodPost, "/api/rooms/room-a/commands", bytes.NewBufferString(`{"action":"resume","target_meeting_id":"meeting-a"}`))
	request.Header.Set("content-type", "application/json")
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusConflict {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusConflict)
	}
}

func TestCommandConfirmationTimeoutDoesNotRetryCommand(t *testing.T) {
	gin.SetMode(gin.TestMode)
	var commandCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("content-type", "application/json")
		if request.URL.Path == "/api/v1/commands" {
			commandCalls.Add(1)
			_, _ = writer.Write([]byte(`{"success":true}`))
			return
		}
		_, _ = writer.Write([]byte(`{"id":"meeting-a","title":"Meeting","status":"paused","languages":{"source":"en","target":"zh-TW"}}`))
	}))
	defer server.Close()

	a := newTestApp(t, map[string]string{"room-a": "token-a"})
	a.rozetaBaseURL = server.URL
	a.httpClient = server.Client()
	a.confirmationTime = 20 * time.Millisecond
	a.state.applyMeetingSync("room-a", []roomMeetingView{{ID: "meeting-a", Status: "paused"}})
	router := gin.New()
	router.POST("/api/rooms/:roomName/commands", a.handleCommand)

	request := httptest.NewRequest(http.MethodPost, "/api/rooms/room-a/commands", bytes.NewBufferString(`{"action":"start"}`))
	request.Header.Set("content-type", "application/json")
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	waitForRoomResult(t, a, "room-a", "confirmation_timeout")
	if commandCalls.Load() != 1 {
		t.Fatalf("command calls = %d, want exactly one", commandCalls.Load())
	}
}

func TestStateRejectsConcurrentRoomCommands(t *testing.T) {
	a := newTestApp(t, map[string]string{"room-a": "token-a"})
	a.state.applyMeetingSync("room-a", []roomMeetingView{{ID: "meeting-a", Status: "paused"}})
	if _, _, err := a.state.beginCommand("room-a", "start", "", "in_progress"); err != nil {
		t.Fatalf("first beginCommand() error = %v", err)
	}
	if _, _, err := a.state.beginCommand("room-a", "pause", "", "paused"); err != errCommandPending {
		t.Fatalf("second beginCommand() error = %v, want %v", err, errCommandPending)
	}
}

func TestDifferentRoomsCanHavePendingCommands(t *testing.T) {
	a := newTestApp(t, map[string]string{"room-a": "token-a", "room-b": "token-b"})
	for _, roomName := range []string{"room-a", "room-b"} {
		a.state.applyMeetingSync(roomName, []roomMeetingView{{ID: roomName + "-meeting", Status: "paused"}})
		if _, _, err := a.state.beginCommand(roomName, "start", "", "in_progress"); err != nil {
			t.Fatalf("beginCommand(%s) error = %v", roomName, err)
		}
	}
}

func newTestApp(t *testing.T, tokens map[string]string) *app {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	a := newApp(ctx, tokens, "password", []byte("01234567890123456789012345678901"))
	a.confirmationTime = 250 * time.Millisecond
	a.pollInterval = time.Millisecond
	return a
}

func waitForRoomResult(t *testing.T, a *app, roomName, result string) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		room, _ := a.state.snapshotRoom(roomName)
		if room.LastCommandResult == result {
			return
		}
		time.Sleep(time.Millisecond)
	}
	room, _ := a.state.snapshotRoom(roomName)
	t.Fatalf("room result = %q, want %q", room.LastCommandResult, result)
}
