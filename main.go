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
	"io"
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
	if err := run(os.Args[1:], os.Stdout); err != nil {
		log.Print(err)
		os.Exit(1)
	}
}

type runtimeConfig struct {
	accountFile string
	sessionFile string
	stateFile   string
}

// serverSecrets are only needed by the HTTP authentication and external API routes;
// the direct reset command operates on the controller and account tokens locally.
type serverSecrets struct {
	adminPassword    string
	sessionSecret    []byte
	externalAPIToken string
}

// applicationRuntime owns the shared controller setup so the server and reset CLI use
// the same startup validation and remote-account wiring instead of maintaining two paths.
type applicationRuntime struct {
	app            *app
	controller     *controller
	startupSummary string
	startupErr     error
}

func (r *applicationRuntime) close() {
	r.controller.close()
	r.app.state.closeAdmins()
}

func run(args []string, output io.Writer) error {
	// The generic account filename replaces the previous room-token-specific CLI name
	// so operators now provide the required credentials with -account.
	flags := flag.NewFlagSet("caption", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	accountFile := flags.String("account", "", "path to required account CSV file")
	sessionFile := flags.String("session", "", "path to required session CSV file")
	stateFile := flags.String("state", "controller-state.json", "path to persistent controller state")
	resetSpec := flags.String("reset", "", "reset selected agendas using DATE|all,ROOM|all")
	resetMaxAge := flags.Int("reset-max-age", defaultResetJobMaxAge, "maximum reset workflow job age")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("unexpected arguments: %s", strings.Join(flags.Args(), " "))
	}
	resetRequested := false
	flags.Visit(func(parsedFlag *flag.Flag) {
		if parsedFlag.Name == "reset" {
			resetRequested = true
		}
	})
	if resetRequested && strings.TrimSpace(*resetSpec) == "" {
		return errors.New("-reset requires a selector")
	}
	if strings.TrimSpace(*accountFile) == "" {
		return errors.New("-account is required")
	}
	if *resetMaxAge < 0 {
		return errors.New("-reset-max-age must not be negative")
	}
	secrets := serverSecrets{}
	if !resetRequested {
		var err error
		secrets, err = loadServerSecrets()
		if err != nil {
			return err
		}
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	runtime, err := newApplicationRuntime(ctx, runtimeConfig{
		accountFile: *accountFile,
		sessionFile: *sessionFile,
		stateFile:   *stateFile,
	}, secrets)
	if err != nil {
		return err
	}
	defer runtime.close()

	if resetRequested {
		if runtime.startupErr != nil {
			return runtime.startupErr
		}
		return runResetWorkflow(ctx, runtime.controller, *resetSpec, *resetMaxAge, output)
	}
	return runServer(runtime)
}

func loadServerSecrets() (serverSecrets, error) {
	adminPassword := os.Getenv("ADMIN_PASSWORD")
	if adminPassword == "" {
		return serverSecrets{}, errors.New("ADMIN_PASSWORD is required")
	}
	sessionSecret := []byte(os.Getenv("SESSION_SECRET"))
	if len(sessionSecret) < 32 {
		return serverSecrets{}, errors.New("SESSION_SECRET must contain at least 32 bytes")
	}
	externalAPIToken := strings.TrimSpace(os.Getenv("EXTERNAL_API_TOKEN"))
	if externalAPIToken == "" {
		return serverSecrets{}, errors.New("EXTERNAL_API_TOKEN is required")
	}
	return serverSecrets{
		adminPassword:    adminPassword,
		sessionSecret:    sessionSecret,
		externalAPIToken: externalAPIToken,
	}, nil
}

func newApplicationRuntime(ctx context.Context, config runtimeConfig, secrets serverSecrets) (*applicationRuntime, error) {
	tokens, err := loadRoomTokens(config.accountFile)
	if err != nil {
		return nil, fmt.Errorf("load token file: %w", err)
	}
	if _, err := loadDesiredStateFile(config.stateFile); err != nil {
		return nil, fmt.Errorf("load controller state: %w", err)
	}

	schedule := meetingSchedule{starts: make(map[string]time.Time), snapshots: make(map[string][]roomMeetingView)}
	// Startup schedule loading is disabled while /setup is the only required flow.
	// var startupScheduleErr error
	// if strings.TrimSpace(config.sessionFile) == "" {
	// 	startupScheduleErr = errors.New("-session is required for strict meeting schedule validation")
	// } else {
	// 	schedule, _, startupScheduleErr = loadMeetingSchedule(ctx, config.sessionFile)
	// }

	gin.SetMode(gin.ReleaseMode)
	a := newApp(ctx, tokens, secrets.adminPassword, secrets.sessionSecret)
	a.externalAPIToken = secrets.externalAPIToken
	a.meetingSchedule = schedule
	controller, err := newController(ctx, a, tokens, schedule, config.stateFile)
	if err != nil {
		return nil, fmt.Errorf("load controller state: %w", err)
	}
	a.controller = controller
	runtime := &applicationRuntime{app: a, controller: controller}
	// Startup Rozeta meeting validation is disabled while /setup is the only required flow.
	// if startupScheduleErr != nil {
	// 	runtime.startupSummary = "startup schedule validation failed"
	// 	runtime.startupErr = startupScheduleErr
	// 	return runtime, nil
	// }
	// if err := controller.validateStartupMeetings(ctx); err != nil {
	// 	runtime.startupSummary = "startup Rozeta meeting validation failed"
	// 	runtime.startupErr = err
	// }
	return runtime, nil
}

func runServer(runtime *applicationRuntime) error {
	if runtime.startupErr != nil {
		runtime.app.setMajorError(runtime.startupSummary, runtime.startupErr)
	}
	router, err := runtime.app.router()
	if err != nil {
		return fmt.Errorf("configure router: %w", err)
	}

	// Reconciliation used to start for every room during process startup. Lifecycle
	// is now process-local and explicitly operator-controlled, so startup exposes
	// persisted desired state while every room remains stopped.
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
	case <-runtime.app.ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdownCtx); err != nil {
			log.Printf("shutdown server: %v", err)
		}
	case err := <-serverErrors:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			return fmt.Errorf("serve: %w", err)
		}
	}
	return nil
}

func newApp(ctx context.Context, tokens map[string]string, adminPassword string, sessionSecret []byte) *app {
	if ctx == nil {
		ctx = context.Background()
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.MaxConnsPerHost = 64
	transport.MaxIdleConns = 64
	transport.MaxIdleConnsPerHost = 32
	transport.ResponseHeaderTimeout = 10 * time.Second
	a := &app{
		ctx:             ctx,
		state:           newState(),
		tokenStore:      tokens,
		meetingSchedule: meetingSchedule{starts: make(map[string]time.Time), snapshots: make(map[string][]roomMeetingView)},
		rozetaBaseURL:   "https://rozeta.app",
		// The controller has separate bounded pools for observations and controls.
		// This transport keeps enough reusable connections for both pools without
		// allowing an unbounded number of sockets to hide remote backpressure.
		httpClient:    &http.Client{Transport: transport, Timeout: 15 * time.Second},
		adminPassword: adminPassword,
		sessionSecret: sessionSecret,
		loginLimiter:  newLoginLimiter(),
		epoch:         newProcessEpoch(),
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
	router.GET("/service-worker.js", a.handleServiceWorker)
	router.GET("/favicon.ico", func(c *gin.Context) { c.Status(http.StatusNoContent) })
	router.GET("/login", a.handleLogin)
	router.POST("/api/login", a.requireSameOrigin, a.handleLoginRequest)

	protected := router.Group("/")
	protected.Use(a.requireAdmin)
	protected.GET("/", a.handleIndex)
	protected.GET("/setup", a.handleSetup)
	protected.GET("/debug", a.handleDebug)
	// The timeline was not previously available as a separate view. It is now protected like the control console because
	// the read-only page exposes live room state and errors without providing mutation controls.
	protected.GET("/timeline", a.handleTimeline)
	protected.POST("/api/logout", a.requireSameOrigin, a.handleLogout)
	router.GET("/api/v1/rooms/:roomName/in-progress", a.handleCurrentMeeting)
	router.POST("/api/v1/rooms/:roomName/actions/advance-and-start", a.requireExternalAPI, a.handleAdvanceAndStart)
	protected.GET("/api/rooms", a.handleListRooms)
	protected.POST("/api/setup/artifacts", a.requireSameOrigin, a.handleSetupArtifacts)
	protected.GET("/api/rooms/:roomName/meetings", a.handleRoomMeetings)
	protected.PUT("/api/rooms/:roomName/desired-state", a.requireSameOrigin, a.handleDesiredState)
	protected.PUT("/api/rooms/:roomName/schedule-offset", a.requireSameOrigin, a.handleScheduleOffset)
	protected.POST("/api/rooms/:roomName/reconciliation/:action/preflight", a.requireSameOrigin, a.handleRoomReconciliationPreflight)
	protected.POST("/api/rooms/:roomName/observe", a.requireSameOrigin, a.handleObserveRoom)
	protected.POST("/api/rooms/:roomName/reconciliation/:action", a.requireSameOrigin, a.handleRoomReconciliation)
	protected.POST("/api/rooms/:roomName/reset-ready/preflight", a.requireSameOrigin, a.handleResetReadyPreflight)
	protected.POST("/api/rooms/:roomName/reset-ready", a.requireSameOrigin, a.handleResetReady)
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
		"app.js":            "text/javascript; charset=utf-8",
		"control.js":        "text/javascript; charset=utf-8",
		"control.css":       "text/css; charset=utf-8",
		"login.js":          "text/javascript; charset=utf-8",
		"setup.css":         "text/css; charset=utf-8",
		"setup.js":          "text/javascript; charset=utf-8",
		"styles.css":        "text/css; charset=utf-8",
		"timeline.css":      "text/css; charset=utf-8",
		"timeline.js":       "text/javascript; charset=utf-8",
		"timeline-model.js": "text/javascript; charset=utf-8",
		"tooltips.js":       "text/javascript; charset=utf-8",
		"state.js":          "text/javascript; charset=utf-8",
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

func (a *app) handleServiceWorker(c *gin.Context) {
	data, err := webAssets.ReadFile("web/service-worker.js")
	if err != nil {
		c.Status(http.StatusNotFound)
		return
	}
	c.Data(http.StatusOK, "text/javascript; charset=utf-8", data)
}

func (a *app) handleIndex(c *gin.Context) {
	a.handlePage(c, "index.html")
}

func (a *app) handleSetup(c *gin.Context) {
	a.handlePage(c, "setup.html")
}

func (a *app) handleDebug(c *gin.Context) {
	a.handlePage(c, "debug.html")
}

func (a *app) handleTimeline(c *gin.Context) {
	a.handlePage(c, "timeline.html")
}

func (a *app) handlePage(c *gin.Context, name string) {
	data, err := webAssets.ReadFile("web/" + name)
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

type setupRequest struct {
	RoomName string `json:"room_name"`
}

type setupResponse struct {
	RoomName     string `json:"room_name"`
	MeetingID    string `json:"meeting_id,omitempty"`
	CookieScript string `json:"cookie_script"`
	OBSURL       string `json:"obs_url,omitempty"`
}

// The setup flow uses the fixed 8-9 schedule because startup no longer fetches
// Rozeta meetings to discover the selected meeting for each room.
var setupMeetingIDs = map[string]string{
	"AU":      "1bwR87XwWc",
	"RB101":   "4PvO12UXY5",
	"RB102":   "vHdhN2EGlQ",
	"RB105":   "z9v1ThGQ3M",
	"TR209":   "KDfjhpviuo",
	"TR210":   "SiwxiV8qYk",
	"TR211":   "gokZyXYns6",
	"TR212":   "HCCeCHRxzS",
	"TR213":   "0VlzYmalE1",
	"TR214":   "MOkCpNrVOS",
	"TR310-2": "QH7vnZ3lxb",
	"TR311":   "LPZGoIUdQ9",
	"TR313":   "jD60mcAWHU",
	"TR409-2": "ZWxi0gO9BM",
	"TR410":   "LPOGoIUdQ9",
	"TR411":   "XOIcs5Ulmv",
	"TR412-1": "OPbLnkgLX5",
	"TR412-2": "4tQO12UXY5",
	"TR509":   "G8EK9LOCsr",
	"TR510":   "EYrmMkvhes",
	"TR511":   "YQJklDKQ0o",
	"TR512":   "gouZyXYns6",
	"TR513":   "OPkLnkgLX5",
	"TR514":   "xBCdNpKG8R",
	"TR515":   "yZJAAsMWGR",
}

func (a *app) handleSetupArtifacts(c *gin.Context) {
	var request setupRequest
	if err := c.ShouldBindJSON(&request); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid setup request"})
		return
	}
	roomName := strings.TrimSpace(request.RoomName)
	token, found := a.tokenStore[roomName]
	if !found {
		c.JSON(http.StatusNotFound, gin.H{"error": "unknown room"})
		return
	}

	meetingID := setupMeetingIDs[roomName]

	// WHY: setup results are deliberately generated only for the selected room, rather
	// than exposing every account token through the room list or a bulk response.
	cookieScript := "(() => {\n\tdocument.cookie = " + strconv.Quote("auth_token="+token+"; Path=/; Secure; SameSite=Lax") + "\n\twindow.location.assign(" + strconv.Quote("https://rozeta.app/en/meetings") + ")\n})()"
	response := setupResponse{RoomName: roomName, MeetingID: meetingID, CookieScript: cookieScript}
	if meetingID != "" {
		query := url.Values{"clientId": {"obs"}, "token": {token}}
		response.OBSURL = a.rozetaURL("/api/web/meetings/" + url.PathEscape(meetingID) + "/embed?" + query.Encode())
	}
	c.JSON(http.StatusOK, response)
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

type currentMeetingResponse struct {
	Name    string `json:"name"`
	OPASSID string `json:"opass_id"`
}

func (a *app) handleCurrentMeeting(c *gin.Context) {
	roomName := strings.TrimSpace(c.Param("roomName"))
	if a.controller == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "controller unavailable"})
		return
	}
	meeting, err := a.controller.currentMeeting(c.Request.Context(), roomName)
	if errors.Is(err, errUnknownRoom) {
		c.JSON(http.StatusNotFound, gin.H{"error": "unknown room"})
		return
	}
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": "failed to load current meeting"})
		return
	}
	c.JSON(http.StatusOK, meeting)
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
	case errors.Is(err, errReconciliationNotActive):
		status, code = http.StatusConflict, "room_not_active"
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
	Virtual        bool       `json:"virtual,omitempty"`
	Source         string     `json:"source_language,omitempty"`
	Target         string     `json:"target_language,omitempty"`
	StartedAt      time.Time  `json:"started_at,omitempty,omitzero"`
	PausedAt       time.Time  `json:"paused_at,omitempty,omitzero"`
	UpdatedAt      time.Time  `json:"updated_at,omitempty,omitzero"`
	ScheduledStart *time.Time `json:"scheduled_start,omitempty"`
	ScheduledEnd   *time.Time `json:"scheduled_end,omitempty"`
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

type scheduleOffsetRequest struct {
	Minutes                   int     `json:"minutes"`
	Epoch                     string  `json:"epoch"`
	ExpectedReconciliationRun *uint64 `json:"expected_reconciliation_run"`
	ExpectedGeneration        *uint64 `json:"expected_generation"`
	ExpectedRevision          *uint64 `json:"expected_revision"`
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

func (a *app) handleScheduleOffset(c *gin.Context) {
	roomName := strings.TrimSpace(c.Param("roomName"))
	var request scheduleOffsetRequest
	if err := c.ShouldBindJSON(&request); err != nil || strings.TrimSpace(request.Epoch) == "" ||
		request.ExpectedReconciliationRun == nil || request.ExpectedGeneration == nil || request.ExpectedRevision == nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "epoch, expected_reconciliation_run, expected_generation, and expected_revision are required"})
		return
	}
	room, err := a.controller.updateScheduleOffset(roomName, scheduleOffsetUpdate{
		Minutes: request.Minutes, ExpectedEpoch: request.Epoch,
		ExpectedRun: *request.ExpectedReconciliationRun, ExpectedGeneration: *request.ExpectedGeneration,
		ExpectedRevision: *request.ExpectedRevision,
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
		case errors.Is(err, errInvalidScheduleOffset):
			status = http.StatusBadRequest
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
	Epoch                 string               `json:"epoch"`
	RoomName              string               `json:"room_name"`
	DesiredMeetingID      string               `json:"desired_meeting_id,omitempty"`
	Generation            uint64               `json:"generation"`
	ScheduleOffsetMinutes int                  `json:"schedule_offset_minutes"`
	ResumeConsumed        bool                 `json:"resume_consumed"`
	Revision              uint64               `json:"revision"`
	DesiredStatus         string               `json:"desired_status,omitempty"`
	Lifecycle             string               `json:"lifecycle"`
	ReconciliationRun     uint64               `json:"reconciliation_run"`
	ActiveMeetingIDs      []string             `json:"active_meeting_ids"`
	ActiveObservedAt      time.Time            `json:"active_observed_at,omitempty,omitzero"`
	ActiveSetStale        bool                 `json:"active_set_stale"`
	ResetReady            bool                 `json:"reset_ready"`
	Resetting             bool                 `json:"resetting"`
	Summary               string               `json:"summary"`
	SummaryReason         string               `json:"summary_reason"`
	Conditions            []reconcileCondition `json:"conditions"`
	RecentActions         []recentAction       `json:"recent_actions"`
	LastError             string               `json:"last_error,omitempty"`
	Meetings              []roomMeetingView    `json:"meetings"`
	UpdatedAt             time.Time            `json:"updated_at,omitempty,omitzero"`
}
