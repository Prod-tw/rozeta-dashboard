package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

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

	a := &app{
		tokenStore:    map[string]string{"TR409-2": "token-a"},
		rozetaBaseURL: baseURL,
		httpClient:    server.Client(),
	}

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
