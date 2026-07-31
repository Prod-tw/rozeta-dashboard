package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
)

func TestHandleRoomMeetingsFlattensPagination(t *testing.T) {
	gin.SetMode(gin.TestMode)
	var nextPageURL string

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Cookie"); got != "auth_token=token-a" {
			t.Fatalf("cookie = %q, want auth_token=token-a", got)
		}

		switch r.URL.Query().Get("page") {
		case "", "1":
			w.Header().Set("content-type", "application/json")
			_, _ = w.Write([]byte(`{"data":[{"id":"m1","title":"First","status":"ready","languages":{"source":"zh-TW","target":"en"}}],"links":{"next":"` + nextPageURL + `?page=2"}}`))
		case "2":
			w.Header().Set("content-type", "application/json")
			_, _ = w.Write([]byte(`{"data":[{"id":"m2","title":"Second","status":"paused","languages":{"source":"en","target":"ja"}}],"links":{"next":""}}`))
		default:
			w.WriteHeader(http.StatusBadRequest)
		}
	}))
	defer server.Close()

	baseURL := server.URL
	nextPageURL = baseURL + "/api/v1/meetings"

	a := newApp(context.Background(), map[string]string{"TR409-2": "token-a"}, "password", []byte("01234567890123456789012345678901"))
	a.rozetaBaseURL = baseURL
	a.httpClient = server.Client()

	router := gin.New()
	router.GET("/api/rooms/:roomName/meetings", a.handleRoomMeetings)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/rooms/TR409-2/meetings", nil)
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}

	var body roomMeetingsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if body.RoomName != "TR409-2" {
		t.Fatalf("room_name = %q, want %q", body.RoomName, "TR409-2")
	}
	if got := len(body.Meetings); got != 2 {
		t.Fatalf("meetings = %d, want 2", got)
	}
	if body.Meetings[0].ID != "m1" || body.Meetings[1].ID != "m2" {
		t.Fatalf("unexpected meetings: %#v", body.Meetings)
	}
}

func TestSyncAllRoomsLimitsConcurrency(t *testing.T) {
	var current atomic.Int32
	var maximum atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		active := current.Add(1)
		defer current.Add(-1)
		for {
			observed := maximum.Load()
			if active <= observed || maximum.CompareAndSwap(observed, active) {
				break
			}
		}
		time.Sleep(20 * time.Millisecond)
		writer.Header().Set("content-type", "application/json")
		_, _ = writer.Write([]byte(`{"data":[],"links":{"next":""}}`))
	}))
	defer server.Close()

	tokens := make(map[string]string)
	for index := range 8 {
		tokens[string(rune('a'+index))] = "token"
	}
	a := newApp(context.Background(), tokens, "password", []byte("01234567890123456789012345678901"))
	a.rozetaBaseURL = server.URL
	a.httpClient = server.Client()
	a.syncAllRooms(context.Background())
	if got := maximum.Load(); got != roomSyncConcurrency {
		t.Fatalf("maximum concurrent requests = %d, want %d", got, roomSyncConcurrency)
	}
}

func TestFetchRozetaMeetingsRejectsPaginationCycle(t *testing.T) {
	var serverURL string
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("content-type", "application/json")
		_, _ = writer.Write([]byte(`{"data":[],"links":{"next":"` + serverURL + `/api/v1/meetings?page=1"}}`))
	}))
	defer server.Close()
	serverURL = server.URL

	a := newApp(context.Background(), map[string]string{"room-a": "token-a"}, "password", []byte("01234567890123456789012345678901"))
	a.rozetaBaseURL = server.URL
	a.httpClient = server.Client()
	if _, err := a.fetchRozetaMeetings(context.Background(), "token-a"); err == nil {
		t.Fatal("expected pagination cycle error")
	}
}

func TestFetchRozetaMeetingsUpgradesSameHostPaginationToHTTPS(t *testing.T) {
	var serverURL string
	server := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("content-type", "application/json")
		switch request.URL.Query().Get("page") {
		case "1":
			httpURL := strings.Replace(serverURL, "https://", "http://", 1)
			_, _ = writer.Write([]byte(`{"data":[{"id":"one","title":"One","status":"ready","languages":{"source":"en","target":"zh-TW"}}],"links":{"next":"` + httpURL + `/api/v1/meetings?page=2"}}`))
		case "2":
			_, _ = writer.Write([]byte(`{"data":[{"id":"two","title":"Two","status":"paused","languages":{"source":"en","target":"zh-TW"}}],"links":{"next":""}}`))
		default:
			writer.WriteHeader(http.StatusBadRequest)
		}
	}))
	defer server.Close()
	serverURL = server.URL

	a := newApp(context.Background(), map[string]string{"room-a": "token-a"}, "password", []byte("01234567890123456789012345678901"))
	a.rozetaBaseURL = server.URL
	a.httpClient = server.Client()
	meetings, err := a.fetchRozetaMeetings(context.Background(), "token-a")
	if err != nil {
		t.Fatalf("fetchRozetaMeetings() error = %v", err)
	}
	if len(meetings) != 2 || meetings[0].ID != "one" || meetings[1].ID != "two" {
		t.Fatalf("meetings = %#v, want both HTTPS pages", meetings)
	}
}

func TestFetchRozetaMeetingsRejectsDifferentHost(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("content-type", "application/json")
		_, _ = writer.Write([]byte(`{"data":[],"links":{"next":"https://example.com/api/v1/meetings?page=2"}}`))
	}))
	defer server.Close()

	a := newApp(context.Background(), map[string]string{"room-a": "token-a"}, "password", []byte("01234567890123456789012345678901"))
	a.rozetaBaseURL = server.URL
	a.httpClient = server.Client()
	_, err := a.fetchRozetaMeetings(context.Background(), "token-a")
	if err == nil || !strings.Contains(err.Error(), "changed origin") {
		t.Fatalf("fetchRozetaMeetings() error = %v, want changed origin", err)
	}
}

func TestHandleRoomMeetingsMarksTransientFailureStale(t *testing.T) {
	gin.SetMode(gin.TestMode)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.WriteHeader(http.StatusBadGateway)
	}))
	defer server.Close()

	a := newApp(context.Background(), map[string]string{"room-a": "token-a"}, "password", []byte("01234567890123456789012345678901"))
	a.rozetaBaseURL = server.URL
	a.httpClient = server.Client()
	a.state.applyMeetingSync("room-a", []roomMeetingView{{ID: "meeting-a", Status: "paused"}})
	router := gin.New()
	router.GET("/api/rooms/:roomName/meetings", a.handleRoomMeetings)
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/api/rooms/room-a/meetings", nil)
	router.ServeHTTP(recorder, request)

	room, _ := a.state.snapshotRoom("room-a")
	if room.APIStatus != "stale" {
		t.Fatalf("API status = %q, want stale", room.APIStatus)
	}
}
