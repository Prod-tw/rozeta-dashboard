package main

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
)

func TestProtectedRouteRequiresAdminSession(t *testing.T) {
	gin.SetMode(gin.TestMode)
	a := newApp(context.Background(), map[string]string{"room-a": "token-a"}, "password", []byte("01234567890123456789012345678901"))
	router, err := a.router()
	if err != nil {
		t.Fatalf("router() error = %v", err)
	}

	request := httptest.NewRequest(http.MethodGet, "/api/rooms", nil)
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated status = %d, want %d", recorder.Code, http.StatusUnauthorized)
	}

	session, err := a.newAdminSession(time.Now().UTC())
	if err != nil {
		t.Fatalf("newAdminSession() error = %v", err)
	}
	request = httptest.NewRequest(http.MethodGet, "/api/rooms", nil)
	request.AddCookie(&http.Cookie{Name: adminSessionCookie, Value: session})
	recorder = httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("authenticated status = %d, want %d", recorder.Code, http.StatusOK)
	}
}

func TestMajorErrorGateBlocksEveryRoute(t *testing.T) {
	gin.SetMode(gin.TestMode)
	a := newApp(context.Background(), map[string]string{"room-a": "token-a"}, "password", []byte("01234567890123456789012345678901"))
	a.setMajorError("startup validation failed", errors.New("private remote response must not be exposed"))
	router, err := a.router()
	if err != nil {
		t.Fatalf("router() error = %v", err)
	}

	for _, path := range []string{"/", "/api/login", "/assets/app.js", "/ws/admin"} {
		recorder := httptest.NewRecorder()
		router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, path, nil))
		if recorder.Code != http.StatusServiceUnavailable {
			t.Errorf("%s status = %d, want %d", path, recorder.Code, http.StatusServiceUnavailable)
		}
		if !strings.Contains(recorder.Body.String(), "startup validation failed") {
			t.Errorf("%s body = %q, want safe major-error summary", path, recorder.Body.String())
		}
		if strings.Contains(recorder.Body.String(), "private remote response") {
			t.Errorf("%s exposed the detailed remote error", path)
		}
	}
}

func TestExternalAPIRequiresBearerToken(t *testing.T) {
	gin.SetMode(gin.TestMode)
	a := newApp(context.Background(), map[string]string{"room-a": "token-a"}, "password", []byte("01234567890123456789012345678901"))
	a.externalAPIToken = "external-secret"
	router := gin.New()
	router.POST("/api/v1/rooms/:roomName/actions/advance-and-start", a.requireExternalAPI, func(c *gin.Context) {
		c.Status(http.StatusAccepted)
	})
	for _, authorization := range []string{"", "Bearer wrong", "Basic external-secret"} {
		request := httptest.NewRequest(http.MethodPost, "/api/v1/rooms/room-a/actions/advance-and-start", nil)
		request.Header.Set("Authorization", authorization)
		recorder := httptest.NewRecorder()
		router.ServeHTTP(recorder, request)
		if recorder.Code != http.StatusUnauthorized {
			t.Fatalf("authorization %q status = %d, want %d", authorization, recorder.Code, http.StatusUnauthorized)
		}
	}
	request := httptest.NewRequest(http.MethodPost, "/api/v1/rooms/room-a/actions/advance-and-start", nil)
	request.Header.Set("Authorization", "Bearer external-secret")
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusAccepted {
		t.Fatalf("valid token status = %d, want %d", recorder.Code, http.StatusAccepted)
	}
}

func TestPageAndWebSocketRequireAdminSession(t *testing.T) {
	gin.SetMode(gin.TestMode)
	a := newApp(context.Background(), map[string]string{"room-a": "token-a"}, "password", []byte("01234567890123456789012345678901"))
	router, err := a.router()
	if err != nil {
		t.Fatalf("router() error = %v", err)
	}

	tests := []struct {
		name       string
		path       string
		wantStatus int
	}{
		{name: "admin page redirects", path: "/", wantStatus: http.StatusSeeOther},
		{name: "setup page redirects", path: "/setup", wantStatus: http.StatusSeeOther},
		{name: "websocket rejects", path: "/ws/admin", wantStatus: http.StatusUnauthorized},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodGet, test.path, nil)
			recorder := httptest.NewRecorder()
			router.ServeHTTP(recorder, request)
			if recorder.Code != test.wantStatus {
				t.Fatalf("status = %d, want %d", recorder.Code, test.wantStatus)
			}
		})
	}
}

func TestPublicAssetAllowlist(t *testing.T) {
	gin.SetMode(gin.TestMode)
	a := newApp(context.Background(), map[string]string{"room-a": "token-a"}, "password", []byte("01234567890123456789012345678901"))
	router, err := a.router()
	if err != nil {
		t.Fatalf("router() error = %v", err)
	}

	tests := []struct {
		name       string
		path       string
		wantStatus int
	}{
		{name: "tooltip script is public", path: "/assets/tooltips.js", wantStatus: http.StatusOK},
		{name: "setup script is public", path: "/assets/setup.js", wantStatus: http.StatusOK},
		{name: "setup stylesheet is public", path: "/assets/setup.css", wantStatus: http.StatusOK},
		{name: "missing favicon is empty", path: "/favicon.ico", wantStatus: http.StatusNoContent},
		{name: "embedded admin page stays protected", path: "/assets/index.html", wantStatus: http.StatusNotFound},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodGet, test.path, nil)
			recorder := httptest.NewRecorder()
			router.ServeHTTP(recorder, request)
			if recorder.Code != test.wantStatus {
				t.Fatalf("status = %d, want %d", recorder.Code, test.wantStatus)
			}
		})
	}
}

func TestLoginCreatesSecureFixedSession(t *testing.T) {
	gin.SetMode(gin.TestMode)
	a := newApp(context.Background(), map[string]string{"room-a": "token-a"}, "password", []byte("01234567890123456789012345678901"))
	router, err := a.router()
	if err != nil {
		t.Fatalf("router() error = %v", err)
	}
	request := httptest.NewRequest(http.MethodPost, "/api/login", bytes.NewBufferString(`{"password":"password"}`))
	request.Header.Set("content-type", "application/json")
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d: %s", recorder.Code, http.StatusOK, recorder.Body.String())
	}
	setCookie := recorder.Header().Get("Set-Cookie")
	for _, attribute := range []string{"HttpOnly", "Secure", "SameSite=Strict", "Max-Age=259200"} {
		if !strings.Contains(setCookie, attribute) {
			t.Errorf("Set-Cookie = %q, missing %q", setCookie, attribute)
		}
	}
}

func TestLoginUsesSafeRedirect(t *testing.T) {
	gin.SetMode(gin.TestMode)
	a := newApp(context.Background(), map[string]string{"room-a": "token-a"}, "password", []byte("01234567890123456789012345678901"))
	router, err := a.router()
	if err != nil {
		t.Fatalf("router() error = %v", err)
	}

	tests := []struct {
		name     string
		redirect string
		want     string
	}{
		{name: "requested local page", redirect: "/setup", want: "/setup"},
		{name: "missing redirect", want: "/"},
		{name: "external redirect", redirect: "https://example.com", want: "/"},
		{name: "protocol relative redirect", redirect: "//example.com", want: "/"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			requestBody := `{"password":"password"}`
			if test.redirect != "" {
				requestBody = `{"password":"password","redirect":"` + test.redirect + `"}`
			}
			request := httptest.NewRequest(http.MethodPost, "/api/login", strings.NewReader(requestBody))
			request.Header.Set("content-type", "application/json")
			recorder := httptest.NewRecorder()
			router.ServeHTTP(recorder, request)
			if recorder.Code != http.StatusOK {
				t.Fatalf("status = %d, want %d", recorder.Code, http.StatusOK)
			}
			if !strings.Contains(recorder.Body.String(), `"redirect":"`+test.want+`"`) {
				t.Fatalf("body = %s, want redirect %q", recorder.Body.String(), test.want)
			}
		})
	}
}

func TestSetupArtifactsSkipPreparationMeeting(t *testing.T) {
	gin.SetMode(gin.TestMode)
	a := newApp(context.Background(), map[string]string{"room-a": "token-a"}, "password", []byte("01234567890123456789012345678901"))
	a.controller = &controller{
		app: a,
		rooms: map[string]*controllerRoom{
			"room-a": {
				name:     "room-a",
				meetings: []roomMeetingView{preparationMeeting(), {ID: "meeting-first"}, {ID: "meeting-second"}},
			},
		},
	}
	router, err := a.router()
	if err != nil {
		t.Fatalf("router() error = %v", err)
	}
	session, err := a.newAdminSession(time.Now().UTC())
	if err != nil {
		t.Fatalf("newAdminSession() error = %v", err)
	}
	request := httptest.NewRequest(http.MethodPost, "/api/setup/artifacts", strings.NewReader(`{"room_name":"room-a"}`))
	request.Header.Set("content-type", "application/json")
	request.AddCookie(&http.Cookie{Name: adminSessionCookie, Value: session})
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d: %s", recorder.Code, http.StatusOK, recorder.Body.String())
	}
	body := recorder.Body.String()
	for _, expected := range []string{
		`"meeting_id":"meeting-first"`,
		`auth_token=token-a`,
		`https://rozeta.app/api/web/meetings/meeting-first/embed?clientId=obs\u0026token=token-a`,
	} {
		if !strings.Contains(body, expected) {
			t.Errorf("body = %s, missing %q", body, expected)
		}
	}
	if strings.Contains(body, "meeting-second") {
		t.Fatalf("body = %s, included a non-first meeting", body)
	}
}

func TestSetupArtifactsAllowRoomWithoutMeetings(t *testing.T) {
	gin.SetMode(gin.TestMode)
	a := newApp(context.Background(), map[string]string{"room-a": "token-a"}, "password", []byte("01234567890123456789012345678901"))
	a.controller = &controller{app: a, rooms: map[string]*controllerRoom{"room-a": {name: "room-a"}}}
	request := httptest.NewRequest(http.MethodPost, "/api/setup/artifacts", strings.NewReader(`{"room_name":"room-a"}`))
	request.Header.Set("content-type", "application/json")
	recorder := httptest.NewRecorder()
	ginContext := gin.CreateTestContextOnly(recorder, gin.New())
	ginContext.Request = request
	a.handleSetupArtifacts(ginContext)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusOK)
	}
	if strings.Contains(recorder.Body.String(), "obs") {
		t.Fatalf("body = %s, want no OBS URL", recorder.Body.String())
	}
}

func TestLoginRateLimit(t *testing.T) {
	gin.SetMode(gin.TestMode)
	a := newApp(context.Background(), map[string]string{"room-a": "token-a"}, "password", []byte("01234567890123456789012345678901"))
	router, err := a.router()
	if err != nil {
		t.Fatalf("router() error = %v", err)
	}

	for range loginAttemptLimit {
		request := httptest.NewRequest(http.MethodPost, "/api/login", bytes.NewBufferString(`{"password":"wrong"}`))
		request.Header.Set("content-type", "application/json")
		request.RemoteAddr = "192.0.2.10:1234"
		recorder := httptest.NewRecorder()
		router.ServeHTTP(recorder, request)
		if recorder.Code != http.StatusUnauthorized {
			t.Fatalf("failed login status = %d, want %d", recorder.Code, http.StatusUnauthorized)
		}
	}

	request := httptest.NewRequest(http.MethodPost, "/api/login", bytes.NewBufferString(`{"password":"password"}`))
	request.Header.Set("content-type", "application/json")
	request.RemoteAddr = "192.0.2.10:1234"
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusTooManyRequests {
		t.Fatalf("rate-limited status = %d, want %d", recorder.Code, http.StatusTooManyRequests)
	}
}

func TestLoginLimiterReservesParallelAttemptsAtomically(t *testing.T) {
	limiter := newLoginLimiter()
	var allowed atomic.Int32
	var workers sync.WaitGroup
	for range 50 {
		workers.Go(func() {
			if limiter.reserve("client", time.Now().UTC()) {
				allowed.Add(1)
			}
		})
	}
	workers.Wait()
	if got := allowed.Load(); got != loginAttemptLimit {
		t.Fatalf("allowed attempts = %d, want %d", got, loginAttemptLimit)
	}
}

func TestLoginRejectsOversizedBody(t *testing.T) {
	gin.SetMode(gin.TestMode)
	a := newApp(context.Background(), map[string]string{"room-a": "token-a"}, "password", []byte("01234567890123456789012345678901"))
	router, err := a.router()
	if err != nil {
		t.Fatalf("router() error = %v", err)
	}
	request := httptest.NewRequest(http.MethodPost, "/api/login", strings.NewReader(`{"password":"`+strings.Repeat("x", 5000)+`"}`))
	request.Header.Set("content-type", "application/json")
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusBadRequest)
	}
}

func TestLoginLimiterBoundsTrackedClients(t *testing.T) {
	limiter := newLoginLimiter()
	now := time.Now().UTC()
	for index := range loginClientLimit {
		if !limiter.reserve(strconv.Itoa(index), now) {
			t.Fatalf("client %d was unexpectedly rejected", index)
		}
	}
	if limiter.reserve("overflow", now) {
		t.Fatal("limiter accepted more than the bounded client count")
	}
	if !limiter.reserve("after-expiry", now.Add(loginWindow+time.Second)) {
		t.Fatal("limiter did not prune expired clients")
	}
}
