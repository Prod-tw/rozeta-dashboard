package main

import (
	"bytes"
	"context"
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
