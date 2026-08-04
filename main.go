package main

import (
	"context"
	"crypto/rand"
	"embed"
	"encoding/csv"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"html"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
)

//go:embed web/*
var webAssets embed.FS

const (
	roomSyncConcurrency = 6
)

type app struct {
	ctx              context.Context
	state            *state
	tokenStore       map[string]string
	meetingSchedule  meetingSchedule
	rozetaBaseURL    string
	httpClient       *http.Client
	adminPassword    string
	sessionSecret    []byte
	externalAPIToken string
	loginLimiter     *loginLimiter
	upgrader         websocket.Upgrader
	controller       *controller
	epoch            string
	majorErrorMu     sync.RWMutex
	majorError       *majorErrorState
}

type majorErrorState struct {
	summary    string
	occurredAt time.Time
}

func main() {
	// The generic account filename replaces the previous room-token-specific CLI name
	// so operators now provide the required credentials with -account.
	accountFile := flag.String("account", "", "path to required account CSV file")
	sessionFile := flag.String("session", "", "path to required session CSV file")
	stateFile := flag.String("state", "controller-state.json", "path to persistent controller state")
	flag.Parse()

	if strings.TrimSpace(*accountFile) == "" {
		log.Fatal("-account is required")
	}
	adminPassword := os.Getenv("ADMIN_PASSWORD")
	if adminPassword == "" {
		log.Fatal("ADMIN_PASSWORD is required")
	}
	sessionSecret := []byte(os.Getenv("SESSION_SECRET"))
	if len(sessionSecret) < 32 {
		log.Fatal("SESSION_SECRET must contain at least 32 bytes")
	}
	externalAPIToken := strings.TrimSpace(os.Getenv("EXTERNAL_API_TOKEN"))
	if externalAPIToken == "" {
		log.Fatal("EXTERNAL_API_TOKEN is required")
	}
	tokens, err := loadRoomTokens(*accountFile)
	if err != nil {
		log.Fatalf("load token file: %v", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	_, err = loadDesiredStateFile(*stateFile)
	if err != nil {
		log.Fatalf("load controller state: %v", err)
	}
	schedule := meetingSchedule{starts: make(map[string]time.Time), snapshots: make(map[string][]roomMeetingView)}
	if strings.TrimSpace(*sessionFile) == "" {
		err = errors.New("-session is required for strict meeting schedule validation")
	} else {
		schedule, _, err = loadMeetingSchedule(ctx, *sessionFile)
	}
	// Previously OPASS and session failures degraded to an unscheduled admin list.
	// Keep the server reachable for diagnosis, but defer all normal handlers behind
	// the major-error gate when this startup validation cannot complete.
	startupScheduleErr := err

	gin.SetMode(gin.ReleaseMode)
	a := newApp(ctx, tokens, adminPassword, sessionSecret)
	a.externalAPIToken = externalAPIToken
	a.meetingSchedule = schedule
	controller, err := newController(ctx, a, tokens, schedule, *stateFile)
	if err != nil {
		log.Fatalf("load controller state: %v", err)
	}
	a.controller = controller
	if startupScheduleErr != nil {
		a.setMajorError("startup schedule validation failed", startupScheduleErr)
	} else if err := controller.validateStartupMeetings(ctx); err != nil {
		a.setMajorError("startup Rozeta meeting validation failed", err)
	}
	router, err := a.router()
	if err != nil {
		log.Fatalf("configure router: %v", err)
	}

	// Reconciliation used to start for every room during process startup. Lifecycle
	// is now process-local and explicitly operator-controlled, so startup exposes
	// persisted desired state while every room remains stopped.
	defer a.controller.close()
	defer a.state.closeAdmins()
	server := &http.Server{
		Addr:              ":8080",
		Handler:           router,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
	serverErrors := make(chan error, 1)
	go func() {
		log.Printf("starting server on %s", server.Addr)
		serverErrors <- server.ListenAndServe()
	}()

	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdownCtx); err != nil {
			log.Printf("shutdown server: %v", err)
		}
	case err := <-serverErrors:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("serve: %v", err)
		}
	}
}

func newApp(ctx context.Context, tokens map[string]string, adminPassword string, sessionSecret []byte) *app {
	if ctx == nil {
		ctx = context.Background()
	}
	a := &app{
		ctx:             ctx,
		state:           newState(),
		tokenStore:      tokens,
		meetingSchedule: meetingSchedule{starts: make(map[string]time.Time), snapshots: make(map[string][]roomMeetingView)},
		rozetaBaseURL:   "https://rozeta.app",
		httpClient:      &http.Client{Timeout: 15 * time.Second},
		adminPassword:   adminPassword,
		sessionSecret:   sessionSecret,
		loginLimiter:    newLoginLimiter(),
		epoch:           newProcessEpoch(),
		upgrader: websocket.Upgrader{
			ReadBufferSize:  1024,
			WriteBufferSize: 1024,
			CheckOrigin: func(request *http.Request) bool {
				origin := strings.TrimSpace(request.Header.Get("Origin"))
				if origin == "" {
					return true
				}
				parsed, err := url.Parse(origin)
				return err == nil && strings.EqualFold(parsed.Host, request.Host)
			},
		},
	}
	return a
}

func newProcessEpoch() string {
	var value [16]byte
	if _, err := rand.Read(value[:]); err == nil {
		return hex.EncodeToString(value[:])
	}
	return strconv.FormatInt(time.Now().UnixNano(), 36)
}

func (a *app) router() (*gin.Engine, error) {
	router := gin.New()
	router.Use(a.majorErrorGate, gin.Recovery(), gin.Logger(), a.securityHeaders())
	// Gin previously trusted forwarded client-IP headers from every peer. Only local
	// reverse proxies may now supply them, so public clients cannot evade login limits.
	if err := router.SetTrustedProxies([]string{"127.0.0.1", "::1"}); err != nil {
		return nil, err
	}
	// StaticFS previously exposed embedded index.html under /assets without the
	// admin middleware. Serving only the public CSS and script allowlist keeps the
	// authenticated page itself behind requireAdmin.
	router.GET("/assets/:name", a.handleAsset)
	router.GET("/login", a.handleLogin)
	router.POST("/api/login", a.requireSameOrigin, a.handleLoginRequest)

	protected := router.Group("/")
	protected.Use(a.requireAdmin)
	protected.GET("/", a.handleIndex)
	protected.POST("/api/logout", a.requireSameOrigin, a.handleLogout)
	router.POST("/api/v1/rooms/:roomName/actions/advance-and-start", a.requireExternalAPI, a.handleAdvanceAndStart)
	protected.GET("/api/rooms", a.handleListRooms)
	protected.GET("/api/rooms/:roomName/meetings", a.handleRoomMeetings)
	protected.PUT("/api/rooms/:roomName/desired-state", a.requireSameOrigin, a.handleDesiredState)
	protected.POST("/api/rooms/:roomName/reconciliation/:action/preflight", a.requireSameOrigin, a.handleRoomReconciliationPreflight)
	protected.POST("/api/rooms/:roomName/observe", a.requireSameOrigin, a.handleObserveRoom)
	protected.POST("/api/rooms/:roomName/reconciliation/:action", a.requireSameOrigin, a.handleRoomReconciliation)
	protected.POST("/api/reconciliation/:action/preflight", a.requireSameOrigin, a.handleBulkReconciliationPreflight)
	protected.POST("/api/reconciliation/:action", a.requireSameOrigin, a.handleBulkReconciliation)
	protected.GET("/ws/admin", a.handleAdminWS)
	return router, nil
}

func (a *app) setMajorError(summary string, err error) {
	if err != nil {
		log.Printf("major error: %s: %v", summary, err)
	}
	a.majorErrorMu.Lock()
	defer a.majorErrorMu.Unlock()
	if a.majorError == nil {
		a.majorError = &majorErrorState{summary: summary, occurredAt: time.Now().UTC()}
	}
}

func (a *app) majorErrorGate(c *gin.Context) {
	state := a.majorErrorSnapshot()
	if state == nil {
		c.Next()
		return
	}
	a.writeMajorError(c, state)
	c.Abort()
}

func (a *app) majorErrorSnapshot() *majorErrorState {
	a.majorErrorMu.RLock()
	defer a.majorErrorMu.RUnlock()
	return a.majorError
}

func (a *app) writeMajorError(c *gin.Context, state *majorErrorState) {
	page := fmt.Sprintf(`<!doctype html><html lang="en"><head><meta charset="utf-8"><title>Major error</title></head><body><h1>Major error</h1><p>%s</p><p>Occurred at: %s</p><p>Check the server log and resolve the configuration or remote-data problem before retrying.</p></body></html>`, html.EscapeString(state.summary), html.EscapeString(state.occurredAt.Format(time.RFC3339)))
	c.Data(http.StatusServiceUnavailable, "text/html; charset=utf-8", []byte(page))
}

func (a *app) handleAsset(c *gin.Context) {
	contentTypes := map[string]string{
		"app.js":      "text/javascript; charset=utf-8",
		"login.js":    "text/javascript; charset=utf-8",
		"styles.css":  "text/css; charset=utf-8",
		"tooltips.js": "text/javascript; charset=utf-8",
		"state.js":    "text/javascript; charset=utf-8",
	}
	name := c.Param("name")
	contentType, ok := contentTypes[name]
	if !ok {
		c.Status(http.StatusNotFound)
		return
	}
	data, err := webAssets.ReadFile("web/" + name)
	if err != nil {
		c.Status(http.StatusNotFound)
		return
	}
	c.Data(http.StatusOK, contentType, data)
}

func (a *app) handleIndex(c *gin.Context) {
	data, err := webAssets.ReadFile("web/index.html")
	if err != nil {
		c.String(http.StatusInternalServerError, "failed to load index")
		return
	}
	c.Data(http.StatusOK, "text/html; charset=utf-8", data)
}

func (a *app) handleListRooms(c *gin.Context) {
	if a.controller == nil {
		c.JSON(http.StatusOK, roomsResponse{Epoch: a.epoch, Rooms: []roomView{}})
		return
	}
	c.JSON(http.StatusOK, roomsResponse{Epoch: a.epoch, Rooms: a.controller.snapshotRooms()})
}

type roomsResponse struct {
	Epoch string     `json:"epoch"`
	Rooms []roomView `json:"rooms"`
}

type externalAPIError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type externalAPIErrorResponse struct {
	Error externalAPIError `json:"error"`
}

type advanceAndStartResponse struct {
	RoomName   string `json:"room_name"`
	MeetingID  string `json:"meeting_id"`
	Generation uint64 `json:"generation"`
	Lifecycle  string `json:"lifecycle"`
	Status     string `json:"status"`
}

func (a *app) handleAdvanceAndStart(c *gin.Context) {
	roomName := strings.TrimSpace(c.Param("roomName"))
	result, err := a.controller.advanceAndStart(c.Request.Context(), roomName)
	if err == nil {
		c.JSON(http.StatusAccepted, advanceAndStartResponse{
			RoomName: roomName, MeetingID: result.MeetingID, Generation: result.Generation,
			Lifecycle: result.Room.Lifecycle, Status: "accepted",
		})
		return
	}
	status := http.StatusInternalServerError
	code := "advance_and_start_failed"
	var apiErr *rozetaAPIError
	switch {
	case errors.Is(err, errUnknownRoom):
		status, code = http.StatusNotFound, "room_not_found"
	case errors.Is(err, errCurrentMeetingUnset):
		status, code = http.StatusConflict, "current_meeting_unset"
	case errors.Is(err, errCurrentMeetingUnscheduled):
		status, code = http.StatusConflict, "current_meeting_unscheduled"
	case errors.Is(err, errNextMeetingNotFound):
		status, code = http.StatusConflict, "next_meeting_not_found"
	case errors.Is(err, errRoomStopping):
		status, code = http.StatusConflict, "room_stopping"
	case errors.Is(err, errStaleControllerState), errors.Is(err, errGenerationConflict):
		status, code = http.StatusConflict, "stale_controller_state"
	case errors.Is(err, errScheduleUnavailable):
		status, code = http.StatusServiceUnavailable, "schedule_unavailable"
	case errors.Is(err, context.DeadlineExceeded), errors.As(err, &apiErr):
		status, code = http.StatusServiceUnavailable, "preflight_unavailable"
	default:
		status, code = http.StatusServiceUnavailable, "preflight_unavailable"
	}
	c.JSON(status, externalAPIErrorResponse{Error: externalAPIError{Code: code, Message: err.Error()}})
}

type reconciliationResponse struct {
	Epoch   string                 `json:"epoch"`
	Rooms   []roomView             `json:"rooms"`
	Results []reconciliationResult `json:"results,omitempty"`
}

type roomReconciliationRequest struct {
	Epoch                     string          `json:"epoch"`
	ExpectedReconciliationRun *uint64         `json:"expected_reconciliation_run"`
	ExpectedGeneration        *uint64         `json:"expected_generation"`
	Preflight                 *preflightFacts `json:"preflight,omitempty"`
	Confirmed                 bool            `json:"confirmed"`
}

type bulkReconciliationRequest struct {
	Epoch     string                 `json:"epoch"`
	Rooms     []reconciliationTarget `json:"rooms"`
	Confirmed bool                   `json:"confirmed"`
}

type preflightResponse struct {
	Epoch   string            `json:"epoch"`
	Rooms   []roomView        `json:"rooms"`
	Results []preflightResult `json:"results"`
}

type roomMeetingView struct {
	ID             string     `json:"id"`
	Title          string     `json:"title"`
	Status         string     `json:"status"`
	Source         string     `json:"source_language,omitempty"`
	Target         string     `json:"target_language,omitempty"`
	StartedAt      time.Time  `json:"started_at,omitempty,omitzero"`
	PausedAt       time.Time  `json:"paused_at,omitempty,omitzero"`
	UpdatedAt      time.Time  `json:"updated_at,omitempty,omitzero"`
	ScheduledStart *time.Time `json:"scheduled_start,omitempty"`
}

type roomMeetingsResponse struct {
	Epoch           string            `json:"epoch"`
	RoomName        string            `json:"room_name"`
	ScheduleEnabled bool              `json:"schedule_enabled"`
	Generation      uint64            `json:"generation"`
	Revision        uint64            `json:"revision"`
	UpdatedAt       time.Time         `json:"updated_at"`
	Meetings        []roomMeetingView `json:"meetings"`
}

func (a *app) handleRoomMeetings(c *gin.Context) {
	roomName := strings.TrimSpace(c.Param("roomName"))
	room, meetings, err := a.controller.refreshRoomMeetings(c.Request.Context(), roomName)
	if errors.Is(err, errUnknownRoom) {
		c.JSON(http.StatusNotFound, gin.H{"error": "unknown room"})
		return
	}
	if err != nil {
		if state := a.majorErrorSnapshot(); state != nil {
			a.writeMajorError(c, state)
			return
		}
		c.JSON(http.StatusBadGateway, gin.H{"error": "failed to load Rozeta meetings"})
		return
	}
	c.JSON(http.StatusOK, roomMeetingsResponse{
		Epoch:           a.epoch,
		RoomName:        roomName,
		ScheduleEnabled: a.meetingSchedule.enabled,
		Generation:      room.Generation,
		Revision:        room.Revision,
		UpdatedAt:       room.UpdatedAt,
		Meetings:        a.meetingSchedule.prepareMeetings(meetings),
	})
}

type desiredStateRequest struct {
	MeetingID                 string  `json:"meeting_id"`
	Epoch                     string  `json:"epoch"`
	ExpectedReconciliationRun *uint64 `json:"expected_reconciliation_run"`
	ExpectedGeneration        *uint64 `json:"expected_generation"`
	ConfirmDestructiveResume  bool    `json:"confirm_destructive_resume"`
	Rearm                     bool    `json:"rearm"`
}

func (a *app) handleDesiredState(c *gin.Context) {
	roomName := strings.TrimSpace(c.Param("roomName"))
	var request desiredStateRequest
	if err := c.ShouldBindJSON(&request); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid desired state request"})
		return
	}
	if strings.TrimSpace(request.Epoch) == "" || request.ExpectedReconciliationRun == nil || request.ExpectedGeneration == nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "epoch, expected_reconciliation_run, and expected_generation are required"})
		return
	}
	room, err := a.controller.updateDesired(roomName, desiredStateUpdate{
		MeetingID: request.MeetingID, ExpectedEpoch: request.Epoch,
		ExpectedRun: *request.ExpectedReconciliationRun, ExpectedGeneration: *request.ExpectedGeneration,
		ConfirmDestructiveResume: request.ConfirmDestructiveResume, Rearm: request.Rearm,
	})
	if errors.Is(err, errGenerationConflict) {
		c.JSON(http.StatusConflict, gin.H{"error": err.Error(), "room": room})
		return
	}
	if err != nil {
		status := http.StatusInternalServerError
		switch {
		case errors.Is(err, errUnknownRoom):
			status = http.StatusNotFound
		case errors.Is(err, errMeetingIDRequired):
			status = http.StatusBadRequest
		case errors.Is(err, errInvalidReconciliation):
			status = http.StatusBadRequest
		case errors.Is(err, errGenerationExhausted):
			status = http.StatusConflict
		case errors.Is(err, errReconciliationNotActive):
			status = http.StatusConflict
		case errors.Is(err, errDestructiveConfirmation):
			status = http.StatusUnprocessableEntity
		}
		c.JSON(status, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusAccepted, room)
}

func (a *app) handleObserveRoom(c *gin.Context) {
	roomName := strings.TrimSpace(c.Param("roomName"))
	var request roomReconciliationRequest
	if err := c.ShouldBindJSON(&request); err != nil || request.ExpectedReconciliationRun == nil || request.ExpectedGeneration == nil || request.Epoch == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "epoch, expected_reconciliation_run, and expected_generation are required"})
		return
	}
	if err := a.controller.requestObservation(request.Epoch, roomName, *request.ExpectedReconciliationRun, *request.ExpectedGeneration); err != nil {
		status := http.StatusConflict
		if errors.Is(err, errUnknownRoom) {
			status = http.StatusNotFound
		}
		c.JSON(status, gin.H{"error": err.Error()})
		return
	}
	c.Status(http.StatusAccepted)
}

func (a *app) handleRoomReconciliationPreflight(c *gin.Context) {
	a.handleReconciliationPreflight(c, true)
}

func (a *app) handleBulkReconciliationPreflight(c *gin.Context) {
	a.handleReconciliationPreflight(c, false)
}

func (a *app) handleReconciliationPreflight(c *gin.Context, single bool) {
	action := c.Param("action")
	if action != "start" && action != "stop" {
		c.JSON(http.StatusNotFound, gin.H{"error": "unknown preflight action"})
		return
	}
	var epoch string
	var targets []reconciliationTarget
	if single {
		var request roomReconciliationRequest
		if err := c.ShouldBindJSON(&request); err != nil || request.ExpectedReconciliationRun == nil || request.ExpectedGeneration == nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "epoch, expected_reconciliation_run, and expected_generation are required"})
			return
		}
		epoch = request.Epoch
		targets = []reconciliationTarget{{RoomName: strings.TrimSpace(c.Param("roomName")), ExpectedReconciliationRun: *request.ExpectedReconciliationRun, ExpectedGeneration: *request.ExpectedGeneration}}
	} else {
		var request bulkReconciliationRequest
		if err := c.ShouldBindJSON(&request); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid preflight request"})
			return
		}
		epoch, targets = request.Epoch, request.Rooms
	}
	rooms, results, err := a.controller.lifecyclePreflight(c.Request.Context(), epoch, action, targets)
	if err != nil {
		c.JSON(http.StatusConflict, gin.H{"error": err.Error(), "epoch": a.epoch, "rooms": rooms})
		return
	}
	c.JSON(http.StatusOK, preflightResponse{Epoch: a.epoch, Rooms: rooms, Results: results})
}

func (a *app) handleRoomReconciliation(c *gin.Context) {
	action := c.Param("action")
	if !validReconciliationAction(action) {
		c.JSON(http.StatusNotFound, gin.H{"error": "unknown reconciliation action"})
		return
	}
	var request roomReconciliationRequest
	if err := c.ShouldBindJSON(&request); err != nil || request.ExpectedReconciliationRun == nil || request.ExpectedGeneration == nil || request.Epoch == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "epoch, expected_reconciliation_run, and expected_generation are required"})
		return
	}
	roomName := strings.TrimSpace(c.Param("roomName"))
	rooms, results, err := a.controller.confirmedLifecycle(c.Request.Context(), request.Epoch, action, []reconciliationTarget{{
		RoomName: roomName, ExpectedReconciliationRun: *request.ExpectedReconciliationRun, ExpectedGeneration: *request.ExpectedGeneration,
		Preflight: request.Preflight,
	}}, request.Confirmed)
	if err != nil {
		status := http.StatusConflict
		if errors.Is(err, errInvalidReconciliation) {
			status = http.StatusBadRequest
		}
		c.JSON(status, gin.H{"error": err.Error(), "epoch": a.epoch, "rooms": rooms})
		return
	}
	c.JSON(http.StatusAccepted, reconciliationResponse{Epoch: a.epoch, Rooms: rooms, Results: results})
}

func (a *app) handleBulkReconciliation(c *gin.Context) {
	action := c.Param("action")
	if !validReconciliationAction(action) {
		c.JSON(http.StatusNotFound, gin.H{"error": "unknown reconciliation action"})
		return
	}
	var request bulkReconciliationRequest
	if err := c.ShouldBindJSON(&request); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid reconciliation request"})
		return
	}
	if strings.TrimSpace(request.Epoch) == "" || len(request.Rooms) == 0 {
		// A syntactically valid but incomplete bulk request previously had no
		// authoritative recovery payload. Treat it as an optimistic conflict so the
		// browser replaces its entire room set before another confirmation attempt.
		c.JSON(http.StatusConflict, gin.H{"error": errReconciliationConflict.Error(), "epoch": a.epoch, "rooms": a.controller.snapshotRooms()})
		return
	}
	rooms, results, err := a.controller.confirmedLifecycle(c.Request.Context(), request.Epoch, action, request.Rooms, request.Confirmed)
	if err != nil {
		status := http.StatusConflict
		if errors.Is(err, errInvalidReconciliation) {
			status = http.StatusBadRequest
		}
		c.JSON(status, gin.H{"error": err.Error(), "epoch": a.epoch, "rooms": rooms})
		return
	}
	c.JSON(http.StatusAccepted, reconciliationResponse{Epoch: a.epoch, Rooms: rooms, Results: results})
}

func validReconciliationAction(action string) bool {
	return action == "start" || action == "stop" || action == "force-stop"
}

func (a *app) handleAdminWS(c *gin.Context) {
	expiresAt, ok := a.adminSessionExpiry(c.Request)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "authentication required"})
		return
	}
	conn, err := a.upgrader.Upgrade(c.Writer, c.Request, nil)
	if err != nil {
		return
	}
	// Authentication used to be checked only during upgrade, allowing an open socket
	// to outlive the fixed session. The read deadline now closes it at cookie expiry.
	_ = conn.SetReadDeadline(expiresAt)
	client := newAdminClient(conn)
	a.state.registerAdmin(client)
	go client.writePump()
	rooms := []roomView{}
	if a.controller != nil {
		rooms = a.controller.snapshotRooms()
	}
	client.sendJSON(adminEnvelope{Type: "snapshot", Epoch: a.epoch, Rooms: rooms, Timestamp: time.Now().UTC()})
	client.readPump(a)
}

func (a *app) broadcastRoom(room roomView) {
	a.broadcastToAdmins(adminEnvelope{Type: "room_snapshot", Epoch: a.epoch, Room: &room, Timestamp: time.Now().UTC()})
}

func (a *app) broadcastToAdmins(message adminEnvelope) {
	data, err := json.Marshal(message)
	if err != nil {
		log.Printf("marshal admin envelope: %v", err)
		return
	}
	a.state.broadcastAdmins(data)
}

func loadRoomTokens(path string) (map[string]string, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	records, err := csv.NewReader(file).ReadAll()
	if err != nil {
		return nil, err
	}
	if len(records) < 2 || len(records[0]) != 3 || !isTokenHeader(records[0]) {
		return nil, errors.New("token CSV must start with account/User ID/Token headers")
	}

	tokens := make(map[string]string, len(records)-1)
	// The previous loader only rejected duplicate room labels, allowing one account
	// identity or bearer token to be controlled by multiple room actors. Track both
	// ownership keys so every configured account remains exclusive.
	ownersByToken := make(map[string]string, len(records)-1)
	accountsByUserID := make(map[string]string, len(records)-1)
	for index, record := range records[1:] {
		line := index + 2
		if len(record) != 3 {
			return nil, fmt.Errorf("token CSV line %d must have exactly 3 fields", line)
		}
		account := strings.TrimSpace(record[0])
		userID := strings.TrimSpace(record[1])
		token := strings.TrimSpace(record[2])
		if account == "" || userID == "" || token == "" {
			return nil, fmt.Errorf("token CSV line %d has an empty account, user ID, or token", line)
		}
		roomName, found := strings.CutSuffix(account, "@coscup.org")
		if !found {
			roomName = account
		}
		roomName = strings.TrimSpace(roomName)
		if roomName == "" {
			return nil, fmt.Errorf("token CSV line %d has an empty room name", line)
		}
		if _, duplicate := tokens[roomName]; duplicate {
			return nil, fmt.Errorf("token CSV line %d duplicates room %q", line, roomName)
		}
		if owner, duplicate := ownersByToken[token]; duplicate {
			return nil, fmt.Errorf("token CSV line %d reuses token owned by room %q", line, owner)
		}
		if owner, duplicate := accountsByUserID[userID]; duplicate {
			return nil, fmt.Errorf("token CSV line %d reuses user ID owned by room %q", line, owner)
		}
		tokens[roomName] = token
		ownersByToken[token] = roomName
		accountsByUserID[userID] = roomName
	}
	if len(tokens) == 0 {
		return nil, errors.New("token CSV contains no rooms")
	}
	return tokens, nil
}

func isTokenHeader(record []string) bool {
	accountHeader := strings.TrimSpace(record[0])
	userIDHeader := strings.TrimSpace(record[1])
	tokenHeader := strings.TrimSpace(record[2])
	return (strings.EqualFold(accountHeader, "account") || accountHeader == "帳號") &&
		strings.EqualFold(userIDHeader, "User ID") && strings.EqualFold(tokenHeader, "token")
}

type adminEnvelope struct {
	Type    string `json:"type"`
	Epoch   string `json:"epoch,omitempty"`
	Level   string `json:"level,omitempty"`
	Message string `json:"message,omitempty"`
	// A value struct is never empty to encoding/json, so snapshot envelopes used to
	// include a fabricated zero-value room. A pointer now makes room exclusive to
	// room_snapshot while full snapshots carry only the authoritative rooms array.
	Room      *roomView  `json:"room,omitempty"`
	Rooms     []roomView `json:"rooms,omitempty"`
	Timestamp time.Time  `json:"timestamp,omitempty"`
}

type roomView struct {
	Epoch             string               `json:"epoch"`
	RoomName          string               `json:"room_name"`
	DesiredMeetingID  string               `json:"desired_meeting_id,omitempty"`
	Generation        uint64               `json:"generation"`
	ResumeConsumed    bool                 `json:"resume_consumed"`
	Revision          uint64               `json:"revision"`
	DesiredStatus     string               `json:"desired_status,omitempty"`
	Lifecycle         string               `json:"lifecycle"`
	ReconciliationRun uint64               `json:"reconciliation_run"`
	ActiveMeetingIDs  []string             `json:"active_meeting_ids"`
	ActiveObservedAt  time.Time            `json:"active_observed_at,omitempty,omitzero"`
	ActiveSetStale    bool                 `json:"active_set_stale"`
	Summary           string               `json:"summary"`
	SummaryReason     string               `json:"summary_reason"`
	Conditions        []reconcileCondition `json:"conditions"`
	RecentActions     []recentAction       `json:"recent_actions"`
	LastError         string               `json:"last_error,omitempty"`
	Meetings          []roomMeetingView    `json:"meetings"`
	UpdatedAt         time.Time            `json:"updated_at,omitempty,omitzero"`
}
