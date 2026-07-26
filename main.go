package main

import (
	"embed"
	"encoding/json"
	"fmt"
	"io/fs"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
)

//go:embed web/*
var webAssets embed.FS

type app struct {
	state *state
	upgrader websocket.Upgrader
}

func main() {
	gin.SetMode(gin.ReleaseMode)

	frontend, err := fs.Sub(webAssets, "web")
	if err != nil {
		log.Fatalf("load assets: %v", err)
	}

	a := &app{
		state: newState(),
		upgrader: websocket.Upgrader{
			ReadBufferSize:  1024,
			WriteBufferSize: 1024,
			CheckOrigin: func(r *http.Request) bool {
				return true
			},
		},
	}

	router := gin.New()
	router.Use(gin.Recovery(), gin.Logger())
	router.StaticFS("/assets", http.FS(frontend))
	router.GET("/", a.handleIndex)
	router.GET("/api/rooms", a.handleListRooms)
	router.POST("/api/rooms/:roomName/commands", a.handleCommand)
	router.GET("/ws/agent", a.handleAgentWS)
	router.GET("/ws/admin", a.handleAdminWS)

	go a.monitorLostHeartbeats()

	addr := ":8080"
	log.Printf("starting server on %s", addr)
	if err := router.Run(addr); err != nil {
		log.Fatal(err)
	}
}

func (a *app) handleIndex(c *gin.Context) {
	data, err := fs.ReadFile(mustSubFS(webAssets, "web"), "index.html")
	if err != nil {
		c.String(http.StatusInternalServerError, "failed to load index: %v", err)
		return
	}
	c.Data(http.StatusOK, "text/html; charset=utf-8", data)
}

func (a *app) handleListRooms(c *gin.Context) {
	c.JSON(http.StatusOK, a.state.snapshotRooms())
}

type commandRequest struct {
	Action          string `json:"action"`
	TargetMeetingID string `json:"target_meeting_id"`
}

func (a *app) handleCommand(c *gin.Context) {
	roomName := strings.TrimSpace(c.Param("roomName"))
	if roomName == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "missing room name"})
		return
	}

	var req commandRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	req.Action = strings.TrimSpace(req.Action)
	req.TargetMeetingID = strings.TrimSpace(req.TargetMeetingID)
	if req.Action == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "missing action"})
		return
	}

	cmd, room := a.state.issueCommand(roomName, req.Action, req.TargetMeetingID)
	a.broadcastToAgents(agentEnvelope{
		Type:            "command",
		RoomName:        cmd.RoomName,
		CommandID:       cmd.CommandID,
		Revision:        cmd.Revision,
		Action:          cmd.Action,
		TargetMeetingID: cmd.TargetMeetingID,
		Timestamp:       cmd.IssuedAt,
	})
	a.broadcastToAdmins(adminEnvelope{
		Type:      "room_snapshot",
		Room:      room.snapshot(a.state.meetingNames),
		Timestamp: time.Now().UTC(),
	})

	c.JSON(http.StatusAccepted, gin.H{
		"command_id":        cmd.CommandID,
		"revision":          cmd.Revision,
		"room_name":         cmd.RoomName,
		"status":            "sent",
		"current_status":    room.status,
		"current_meeting_id": room.currentMeetingID,
	})
}

func (a *app) handleAgentWS(c *gin.Context) {
	conn, err := a.upgrader.Upgrade(c.Writer, c.Request, nil)
	if err != nil {
		return
	}

	client := newAgentClient(conn)
	a.state.registerAgent(client)
	go client.writePump()
	client.readPump(a)
}

func (a *app) handleAdminWS(c *gin.Context) {
	conn, err := a.upgrader.Upgrade(c.Writer, c.Request, nil)
	if err != nil {
		return
	}

	client := newAdminClient(conn)
	a.state.registerAdmin(client)
	go client.writePump()
	client.sendJSON(adminEnvelope{
		Type:      "snapshot",
		Rooms:     a.state.snapshotRooms(),
		Timestamp: time.Now().UTC(),
	})
	client.readPump(a)
}

func (a *app) monitorLostHeartbeats() {
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()

	for range ticker.C {
		rooms := a.state.markLostRooms(3 * time.Second)
		for _, room := range rooms {
			a.broadcastToAdmins(adminEnvelope{
				Type:      "alert",
				Level:     "error",
				Message:   fmt.Sprintf("room %s lost heartbeat", room.roomName),
				Room:      room.snapshot(a.state.meetingNames),
				Timestamp: time.Now().UTC(),
			})
		}
	}
}

func (a *app) broadcastToAgents(msg agentEnvelope) {
	data, err := json.Marshal(msg)
	if err != nil {
		log.Printf("marshal agent envelope: %v", err)
		return
	}
	a.state.broadcastAgents(data)
}

func (a *app) broadcastToAdmins(msg adminEnvelope) {
	data, err := json.Marshal(msg)
	if err != nil {
		log.Printf("marshal admin envelope: %v", err)
		return
	}
	a.state.broadcastAdmins(data)
}

func mustSubFS(embedded embed.FS, dir string) fs.FS {
	sub, err := fs.Sub(embedded, dir)
	if err != nil {
		panic(err)
	}
	return sub
}

type agentEnvelope struct {
	Type            string    `json:"type"`
	RoomName        string    `json:"room_name,omitempty"`
	AgentID         string    `json:"agent_id,omitempty"`
	CommandID       string    `json:"command_id,omitempty"`
	Revision        int       `json:"revision,omitempty"`
	Action          string    `json:"action,omitempty"`
	TargetMeetingID string    `json:"target_meeting_id,omitempty"`
	Status          string    `json:"status,omitempty"`
	CurrentMeetingID string   `json:"current_meeting_id,omitempty"`
	Timestamp       time.Time `json:"timestamp,omitempty"`
	LastCommandID   string    `json:"last_command_id,omitempty"`
	LastCommandResult string  `json:"last_command_result,omitempty"`
	LastError       string    `json:"last_error,omitempty"`
	HeartbeatMS     int64     `json:"heartbeat_ms,omitempty"`
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
	RoomName            string    `json:"room_name"`
	AgentID             string    `json:"agent_id,omitempty"`
	Status              string    `json:"status"`
	CurrentMeetingID     string    `json:"current_meeting_id,omitempty"`
	CurrentMeetingName   string    `json:"current_meeting_name,omitempty"`
	LastCommandID       string    `json:"last_command_id,omitempty"`
	LastCommandResult   string    `json:"last_command_result,omitempty"`
	LastError           string    `json:"last_error,omitempty"`
	LastHeartbeatAt     time.Time `json:"last_heartbeat_at,omitempty"`
	HeartbeatAgeSeconds  float64   `json:"heartbeat_age_seconds,omitempty"`
	PendingCommandID    string    `json:"pending_command_id,omitempty"`
	PendingCommandAction string    `json:"pending_command_action,omitempty"`
	UpdatedAt            time.Time `json:"updated_at,omitempty"`
}

type snapshotHeartbeat struct {
	RoomName          string    `json:"room_name"`
	AgentID           string    `json:"agent_id"`
	Status            string    `json:"status"`
	CurrentMeetingID   string    `json:"current_meeting_id"`
	Timestamp         time.Time `json:"timestamp"`
	LastCommandID     string    `json:"last_command_id"`
	LastCommandResult string    `json:"last_command_result"`
	LastError         string    `json:"last_error"`
	Type              string    `json:"type"`
}
