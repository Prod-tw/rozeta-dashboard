package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/gin-gonic/gin"
)

func TestLoadRoomTokens(t *testing.T) {
	tempDir := t.TempDir()
	path := filepath.Join(tempDir, "room.csv")
	content := []byte("account,User ID,Token\nTR409-2@coscup.org,cTMsWD4FqJ,token-a\nroom-b@coscup.org,ignored,token-b\n")
	if err := os.WriteFile(path, content, 0o600); err != nil {
		t.Fatalf("write csv: %v", err)
	}

	tokens, err := loadRoomTokens(path)
	if err != nil {
		t.Fatalf("load tokens: %v", err)
	}

	if got := tokens["TR409-2"]; got != "token-a" {
		t.Fatalf("token for TR409-2 = %q, want %q", got, "token-a")
	}
	if got := tokens["room-b"]; got != "token-b" {
		t.Fatalf("token for room-b = %q, want %q", got, "token-b")
	}
}

func TestHandleTokenLookup(t *testing.T) {
	gin.SetMode(gin.TestMode)
	a := &app{tokenStore: map[string]string{"TR409-2": "token-a"}}

	router := gin.New()
	router.GET("/api/token", a.handleTokenLookup)

	req := httptest.NewRequest(http.MethodGet, "/api/token?room_id=TR409-2", nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "*" {
		t.Fatalf("CORS header = %q, want %q", got, "*")
	}
	if got := rec.Body.String(); got == "" || got == "\n" {
		t.Fatal("expected JSON body")
	}
}
