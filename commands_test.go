package main

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
)

func TestHandleCommandRejectsUnsupportedAction(t *testing.T) {
	gin.SetMode(gin.TestMode)
	a := &app{state: newState()}

	router := gin.New()
	router.POST("/api/rooms/:roomName/commands", a.handleCommand)

	req := httptest.NewRequest(http.MethodPost, "/api/rooms/TR409-2/commands", bytes.NewBufferString(`{"action":"goto_and_start"}`))
	req.Header.Set("content-type", "application/json")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusBadRequest)
	}
}
