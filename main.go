package main

import (
	"context"
	"embed"
	"encoding/csv"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/signal"
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
	roomSyncInterval        = 10 * time.Second
	commandPollInterval     = 500 * time.Millisecond
	commandConfirmationTime = 15 * time.Second
	roomSyncConcurrency     = 6
	roomSyncRequestTimeout  = 2 * time.Second
	roomSyncCycleTimeout    = 9 * time.Second
)

type app struct {
	ctx              context.Context
	state            *state
	tokenStore       map[string]string
	rozetaBaseURL    string
	httpClient       *http.Client
	adminPassword    string
	sessionSecret    []byte
	loginLimiter     *loginLimiter
	confirmationTime time.Duration
	pollInterval     time.Duration
	upgrader         websocket.Upgrader
}

func main() {
	tokenFile := flag.String("token-file", "", "path to required room token CSV file")
	flag.Parse()

	if strings.TrimSpace(*tokenFile) == "" {
		log.Fatal("-token-file is required")
	}
	adminPassword := os.Getenv("ADMIN_PASSWORD")
	if adminPassword == "" {
		log.Fatal("ADMIN_PASSWORD is required")
	}
	sessionSecret := []byte(os.Getenv("SESSION_SECRET"))
	if len(sessionSecret) < 32 {
		log.Fatal("SESSION_SECRET must contain at least 32 bytes")
	}
	tokens, err := loadRoomTokens(*tokenFile)
	if err != nil {
		log.Fatalf("load token file: %v", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	gin.SetMode(gin.ReleaseMode)
	a := newApp(ctx, tokens, adminPassword, sessionSecret)
	router, err := a.router()
	if err != nil {
		log.Fatalf("configure router: %v", err)
	}

	go a.runRoomSync(ctx)
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
		ctx:              ctx,
		state:            newState(),
		tokenStore:       tokens,
		rozetaBaseURL:    "https://rozeta.app",
		httpClient:       &http.Client{Timeout: 15 * time.Second},
		adminPassword:    adminPassword,
		sessionSecret:    sessionSecret,
		loginLimiter:     newLoginLimiter(),
		confirmationTime: commandConfirmationTime,
		pollInterval:     commandPollInterval,
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
	roomNames := make([]string, 0, len(tokens))
	for roomName := range tokens {
		roomNames = append(roomNames, roomName)
	}
	a.state.seedRooms(roomNames)
	return a
}

func (a *app) router() (*gin.Engine, error) {
	router := gin.New()
	router.Use(gin.Recovery(), gin.Logger(), a.securityHeaders())
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
	protected.GET("/api/rooms", a.handleListRooms)
	protected.GET("/api/rooms/:roomName/meetings", a.handleRoomMeetings)
	protected.POST("/api/rooms/:roomName/commands", a.requireSameOrigin, a.handleCommand)
	protected.GET("/ws/admin", a.handleAdminWS)
	return router, nil
}

func (a *app) handleAsset(c *gin.Context) {
	contentTypes := map[string]string{
		"app.js":     "text/javascript; charset=utf-8",
		"login.js":   "text/javascript; charset=utf-8",
		"styles.css": "text/css; charset=utf-8",
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
	c.JSON(http.StatusOK, a.state.snapshotRooms())
}

type roomMeetingView struct {
	ID        string    `json:"id"`
	Title     string    `json:"title"`
	Status    string    `json:"status"`
	Source    string    `json:"source_language,omitempty"`
	Target    string    `json:"target_language,omitempty"`
	StartedAt time.Time `json:"started_at,omitempty"`
	PausedAt  time.Time `json:"paused_at,omitempty"`
	UpdatedAt time.Time `json:"updated_at,omitempty"`
}

type roomMeetingsResponse struct {
	RoomName string            `json:"room_name"`
	Meetings []roomMeetingView `json:"meetings"`
}

func (a *app) handleRoomMeetings(c *gin.Context) {
	roomName := strings.TrimSpace(c.Param("roomName"))
	token, ok := a.tokenStore[roomName]
	if !ok {
		c.JSON(http.StatusNotFound, gin.H{"error": "unknown room"})
		return
	}
	meetings, err := a.fetchRozetaMeetings(c.Request.Context(), token)
	if err != nil {
		a.markRoomAPIError(roomName, err)
		c.JSON(http.StatusBadGateway, gin.H{"error": "failed to load Rozeta meetings"})
		return
	}
	before, _ := a.state.snapshotRoom(roomName)
	room, changed := a.state.applyMeetingSync(roomName, meetings)
	if changed {
		a.broadcastRoom(room)
	}
	a.broadcastLateConfirmation(before, room)
	c.JSON(http.StatusOK, roomMeetingsResponse{RoomName: roomName, Meetings: meetings})
}

type commandRequest struct {
	Action          string `json:"action"`
	TargetMeetingID string `json:"target_meeting_id"`
}

func (a *app) handleCommand(c *gin.Context) {
	roomName := strings.TrimSpace(c.Param("roomName"))
	if _, ok := a.tokenStore[roomName]; !ok {
		c.JSON(http.StatusNotFound, gin.H{"error": "unknown room"})
		return
	}
	var request commandRequest
	if err := c.ShouldBindJSON(&request); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid command request"})
		return
	}
	request.Action = strings.TrimSpace(request.Action)
	request.TargetMeetingID = strings.TrimSpace(request.TargetMeetingID)
	expectedStatus, ok := expectedCommandStatus(request.Action)
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "unsupported action"})
		return
	}
	if (request.Action == "goto" || request.Action == "resume") && request.TargetMeetingID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "target meeting is required"})
		return
	}
	if request.Action == "resume" {
		// Resume permanently deletes transcript data. The UI confirmation is not an
		// authorization boundary, so the server rechecks the selected meeting state
		// before allowing the destructive endpoint call.
		meeting, err := a.fetchRozetaMeeting(c.Request.Context(), a.tokenStore[roomName], request.TargetMeetingID)
		if err != nil {
			a.markRoomAPIError(roomName, err)
			c.JSON(http.StatusBadGateway, gin.H{"error": "failed to verify completed meeting"})
			return
		}
		if meeting.Status != "completed" {
			c.JSON(http.StatusConflict, gin.H{"error": "only completed meetings can be resumed"})
			return
		}
	}

	cmd, room, err := a.state.beginCommand(roomName, request.Action, request.TargetMeetingID, expectedStatus)
	if err != nil {
		status := http.StatusConflict
		if err.Error() == "unknown room" {
			status = http.StatusNotFound
		}
		c.JSON(status, gin.H{"error": err.Error()})
		return
	}
	a.broadcastRoom(room)
	go a.executeCommand(cmd)
	c.JSON(http.StatusAccepted, gin.H{
		"command_id": cmd.CommandID,
		"room_name":  cmd.RoomName,
		"action":     cmd.Action,
		"status":     "pending",
	})
}

func expectedCommandStatus(action string) (string, bool) {
	switch action {
	case "goto":
		return "", true
	case "start":
		return "in_progress", true
	case "pause":
		return "paused", true
	case "resume":
		return "ready", true
	default:
		return "", false
	}
}

func (a *app) executeCommand(cmd command) {
	token := a.tokenStore[cmd.RoomName]
	timeout := a.confirmationTime
	if timeout <= 0 {
		timeout = commandConfirmationTime
	}
	ctx, cancel := context.WithTimeout(a.ctx, timeout)
	defer cancel()

	var sendErr error
	switch cmd.Action {
	case "goto":
		sendErr = a.sendRozetaCommand(ctx, token, "goto_meeting", cmd.TargetMeetingID)
		if sendErr != nil {
			a.completeCommand(cmd, "failed", "", sendErr.Error())
			return
		}
		a.completeCommand(cmd, "confirmed", "", "")
		return
	case "start":
		sendErr = a.sendRozetaCommand(ctx, token, "start_meeting", cmd.TargetMeetingID)
	case "pause":
		sendErr = a.sendRozetaCommand(ctx, token, "pause_meeting", cmd.TargetMeetingID)
	case "resume":
		sendErr = a.resumeRozetaMeeting(ctx, token, cmd.TargetMeetingID)
	}

	interval := a.pollInterval
	if interval <= 0 {
		interval = commandPollInterval
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		meeting, err := a.fetchRozetaMeeting(ctx, token, cmd.TargetMeetingID)
		if err == nil && meeting.Status == expectedStatusForCommand(cmd.Action) {
			a.completeCommand(cmd, "confirmed", meeting.Status, "")
			return
		}
		a.markRoomAPIError(cmd.RoomName, err)
		select {
		case <-ctx.Done():
			message := "command confirmation timed out"
			if sendErr != nil {
				message += ": " + sendErr.Error()
			}
			a.completeCommand(cmd, "confirmation_timeout", "", message)
			return
		case <-ticker.C:
		}
	}
}

func expectedStatusForCommand(action string) string {
	status, _ := expectedCommandStatus(action)
	return status
}

func (a *app) completeCommand(cmd command, result, status, message string) {
	room, ok := a.state.finishCommand(cmd.RoomName, cmd.CommandID, result, status, message)
	if !ok {
		return
	}
	a.broadcastRoom(room)
	level := "info"
	if result == "failed" || result == "confirmation_timeout" {
		level = "error"
	}
	a.broadcastToAdmins(adminEnvelope{
		Type:      "alert",
		Level:     level,
		Message:   fmt.Sprintf("%s %s for %s", cmd.Action, result, cmd.RoomName),
		Room:      room,
		Timestamp: time.Now().UTC(),
	})
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
	client.sendJSON(adminEnvelope{Type: "snapshot", Rooms: a.state.snapshotRooms(), Timestamp: time.Now().UTC()})
	client.readPump(a)
}

func (a *app) runRoomSync(ctx context.Context) {
	a.syncAllRooms(ctx)
	ticker := time.NewTicker(roomSyncInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			a.syncAllRooms(ctx)
		}
	}
}

func (a *app) syncAllRooms(ctx context.Context) {
	cycleCtx, cancel := context.WithTimeout(ctx, roomSyncCycleTimeout)
	defer cancel()
	rooms := a.state.snapshotRooms()
	semaphore := make(chan struct{}, roomSyncConcurrency)
	var workers sync.WaitGroup
	for _, room := range rooms {
		roomName := room.RoomName
		workers.Go(func() {
			select {
			case semaphore <- struct{}{}:
				defer func() { <-semaphore }()
			case <-cycleCtx.Done():
				return
			}
			roomCtx, cancel := context.WithTimeout(cycleCtx, roomSyncRequestTimeout)
			defer cancel()
			a.syncRoom(roomCtx, roomName)
		})
	}
	workers.Wait()
}

func (a *app) syncRoom(ctx context.Context, roomName string) {
	token := a.tokenStore[roomName]
	meetings, err := a.fetchRozetaMeetings(ctx, token)
	if err != nil {
		apiStatus := "stale"
		var apiErr *rozetaAPIError
		if errors.As(err, &apiErr) && (apiErr.StatusCode == http.StatusUnauthorized || apiErr.StatusCode == http.StatusForbidden) {
			apiStatus = "authentication_error"
		}
		room, changed := a.state.markSyncError(roomName, apiStatus, err.Error())
		if changed {
			a.broadcastRoom(room)
		}
		return
	}
	before, _ := a.state.snapshotRoom(roomName)
	room, changed := a.state.applyMeetingSync(roomName, meetings)
	if changed {
		a.broadcastRoom(room)
	}
	a.broadcastLateConfirmation(before, room)
}

func (a *app) markRoomAPIError(roomName string, err error) {
	if err == nil {
		return
	}
	apiStatus := "stale"
	var apiErr *rozetaAPIError
	if errors.As(err, &apiErr) && (apiErr.StatusCode == http.StatusUnauthorized || apiErr.StatusCode == http.StatusForbidden) {
		apiStatus = "authentication_error"
	}
	room, changed := a.state.markSyncError(roomName, apiStatus, err.Error())
	if changed {
		a.broadcastRoom(room)
	}
}

func (a *app) broadcastLateConfirmation(before, after roomView) {
	if before.LastCommandResult == "confirmed_late" || after.LastCommandResult != "confirmed_late" {
		return
	}
	a.broadcastToAdmins(adminEnvelope{
		Type:      "alert",
		Level:     "info",
		Message:   fmt.Sprintf("%s confirmed late for %s", after.LastCommandAction, after.RoomName),
		Room:      after,
		Timestamp: time.Now().UTC(),
	})
}

func (a *app) broadcastRoom(room roomView) {
	a.broadcastToAdmins(adminEnvelope{Type: "room_snapshot", Room: room, Timestamp: time.Now().UTC()})
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
		tokens[roomName] = token
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
	Type      string     `json:"type"`
	Level     string     `json:"level,omitempty"`
	Message   string     `json:"message,omitempty"`
	Room      roomView   `json:"room,omitempty"`
	Rooms     []roomView `json:"rooms,omitempty"`
	Timestamp time.Time  `json:"timestamp,omitempty"`
}

type command struct {
	CommandID       string
	RoomName        string
	Action          string
	TargetMeetingID string
	Revision        int
	IssuedAt        time.Time
}

type roomView struct {
	RoomName             string    `json:"room_name"`
	Status               string    `json:"status"`
	APIStatus            string    `json:"api_status"`
	CurrentMeetingID     string    `json:"current_meeting_id,omitempty"`
	CurrentMeetingName   string    `json:"current_meeting_name,omitempty"`
	LastSyncedAt         time.Time `json:"last_synced_at,omitempty"`
	LastCommandID        string    `json:"last_command_id,omitempty"`
	LastCommandAction    string    `json:"last_command_action,omitempty"`
	LastCommandResult    string    `json:"last_command_result,omitempty"`
	LastError            string    `json:"last_error,omitempty"`
	PendingCommandID     string    `json:"pending_command_id,omitempty"`
	PendingCommandAction string    `json:"pending_command_action,omitempty"`
	PendingCommandTarget string    `json:"pending_command_target,omitempty"`
	UpdatedAt            time.Time `json:"updated_at,omitempty"`
}
