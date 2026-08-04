package main

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
)

const (
	adminSessionCookie = "rozeta_admin_session"
	adminSessionTTL    = 72 * time.Hour
	loginWindow        = 5 * time.Minute
	loginAttemptLimit  = 10
	loginClientLimit   = 4096
)

type loginLimiter struct {
	mu       sync.Mutex
	attempts map[string][]time.Time
}

func newLoginLimiter() *loginLimiter {
	return &loginLimiter{attempts: make(map[string][]time.Time)}
}

func (l *loginLimiter) reserve(key string, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.pruneAllLocked(now)
	if _, exists := l.attempts[key]; !exists && len(l.attempts) >= loginClientLimit {
		return false
	}
	if len(l.attempts[key]) >= loginAttemptLimit {
		return false
	}
	// Reserving before password verification closes the old race where parallel
	// attempts all passed the limit check before any failure had been recorded.
	l.attempts[key] = append(l.attempts[key], now)
	return true
}

func (l *loginLimiter) clear(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.attempts, key)
}

func (l *loginLimiter) pruneLocked(key string, now time.Time) {
	cutoff := now.Add(-loginWindow)
	entries := l.attempts[key]
	kept := entries[:0]
	for _, entry := range entries {
		if entry.After(cutoff) {
			kept = append(kept, entry)
		}
	}
	if len(kept) == 0 {
		delete(l.attempts, key)
		return
	}
	l.attempts[key] = kept
}

func (l *loginLimiter) pruneAllLocked(now time.Time) {
	for key := range l.attempts {
		l.pruneLocked(key, now)
	}
}

func (a *app) securityHeaders() gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Header("Content-Security-Policy", "default-src 'self'; connect-src 'self' ws: wss:; img-src 'self' data:; style-src 'self'; script-src 'self'; base-uri 'none'; frame-ancestors 'none'; form-action 'self'")
		c.Header("Referrer-Policy", "no-referrer")
		c.Header("X-Content-Type-Options", "nosniff")
		c.Header("X-Frame-Options", "DENY")
		c.Header("Cache-Control", "no-store")
		c.Next()
	}
}

func (a *app) requireAdmin(c *gin.Context) {
	if a.adminAuthenticated(c.Request) {
		c.Next()
		return
	}
	if strings.HasPrefix(c.Request.URL.Path, "/api/") || strings.HasPrefix(c.Request.URL.Path, "/ws/") {
		c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "authentication required"})
		return
	}
	c.Redirect(http.StatusSeeOther, "/login")
	c.Abort()
}

func (a *app) requireSameOrigin(c *gin.Context) {
	origin := strings.TrimSpace(c.GetHeader("Origin"))
	if origin == "" {
		c.Next()
		return
	}
	parsed, err := url.Parse(origin)
	if err != nil || !strings.EqualFold(parsed.Host, c.Request.Host) {
		c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"error": "cross-origin request rejected"})
		return
	}
	c.Next()
}

func (a *app) requireExternalAPI(c *gin.Context) {
	// WHY: the browser session cookie is intentionally not accepted for machine callers.
	// Previously there was no external control boundary; this dedicated Bearer check keeps
	// the destructive advance operation separate from the admin UI authentication flow.
	const scheme = "Bearer "
	authorization := c.GetHeader("Authorization")
	if len(authorization) <= len(scheme) || !strings.EqualFold(authorization[:len(scheme)], scheme) ||
		!hmac.Equal([]byte(strings.TrimSpace(authorization[len(scheme):])), []byte(a.externalAPIToken)) {
		c.AbortWithStatusJSON(http.StatusUnauthorized, externalAPIErrorResponse{Error: externalAPIError{
			Code: "authentication_required", Message: "external API authentication is required",
		}})
		return
	}
	c.Next()
}

func (a *app) handleLogin(c *gin.Context) {
	if a.adminAuthenticated(c.Request) {
		c.Redirect(http.StatusSeeOther, "/")
		return
	}
	data, err := webAssets.ReadFile("web/login.html")
	if err != nil {
		c.String(http.StatusInternalServerError, "failed to load login page")
		return
	}
	c.Data(http.StatusOK, "text/html; charset=utf-8", data)
}

func (a *app) handleLoginRequest(c *gin.Context) {
	now := time.Now().UTC()
	clientIP := c.ClientIP()
	if !a.loginLimiter.reserve(clientIP, now) {
		c.Header("Retry-After", strconv.Itoa(int(loginWindow.Seconds())))
		c.JSON(http.StatusTooManyRequests, gin.H{"error": "too many login attempts"})
		return
	}

	var request struct {
		Password string `json:"password"`
	}
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 4096)
	if err := c.ShouldBindJSON(&request); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid login request"})
		return
	}
	want := sha256.Sum256([]byte(a.adminPassword))
	got := sha256.Sum256([]byte(request.Password))
	if subtle.ConstantTimeCompare(got[:], want[:]) != 1 {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid password"})
		return
	}

	a.loginLimiter.clear(clientIP)
	session, err := a.newAdminSession(now)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to create session"})
		return
	}
	http.SetCookie(c.Writer, &http.Cookie{
		Name:     adminSessionCookie,
		Value:    session,
		Path:     "/",
		MaxAge:   int(adminSessionTTL.Seconds()),
		Expires:  now.Add(adminSessionTTL),
		HttpOnly: true,
		Secure:   true,
		SameSite: http.SameSiteStrictMode,
	})
	c.JSON(http.StatusOK, gin.H{"status": "authenticated"})
}

func (a *app) handleLogout(c *gin.Context) {
	http.SetCookie(c.Writer, &http.Cookie{
		Name:     adminSessionCookie,
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		Expires:  time.Unix(0, 0),
		HttpOnly: true,
		Secure:   true,
		SameSite: http.SameSiteStrictMode,
	})
	c.Status(http.StatusNoContent)
}

func (a *app) newAdminSession(now time.Time) (string, error) {
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return "", err
	}
	payload := strconv.FormatInt(now.Add(adminSessionTTL).Unix(), 10) + "." + hex.EncodeToString(nonce[:])
	signature := a.signSession(payload)
	return payload + "." + signature, nil
}

func (a *app) adminAuthenticated(request *http.Request) bool {
	_, ok := a.adminSessionExpiry(request)
	return ok
}

func (a *app) adminSessionExpiry(request *http.Request) (time.Time, bool) {
	cookie, err := request.Cookie(adminSessionCookie)
	if err != nil {
		return time.Time{}, false
	}
	parts := strings.Split(cookie.Value, ".")
	if len(parts) != 3 {
		return time.Time{}, false
	}
	payload := parts[0] + "." + parts[1]
	want, err := base64.RawURLEncoding.DecodeString(a.signSession(payload))
	if err != nil {
		return time.Time{}, false
	}
	got, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil || !hmac.Equal(got, want) {
		return time.Time{}, false
	}
	expiresAt, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil {
		return time.Time{}, false
	}
	expires := time.Unix(expiresAt, 0)
	return expires, time.Now().UTC().Before(expires)
}

func (a *app) signSession(payload string) string {
	mac := hmac.New(sha256.New, a.sessionSecret)
	_, _ = mac.Write([]byte(payload))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}
