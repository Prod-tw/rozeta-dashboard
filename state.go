package main

import (
	"encoding/json"
	"crypto/rand"
	"encoding/hex"
	"os"
	"sort"
	"strings"
	"sync"
	"time"
)

type state struct {
	mu               sync.RWMutex
	rooms            map[string]*roomState
	meetingNames     map[string]string
	agentClients     map[*agentClient]struct{}
	adminClients     map[*adminClient]struct{}
	commandRevision  map[string]int
}

type roomState struct {
	roomName            string
	agentID             string
	status              string
	currentMeetingID    string
	lastCommandID       string
	lastCommandResult   string
	lastError           string
	lastHeartbeatAt     time.Time
	updatedAt           time.Time
	pendingCommandID    string
	pendingCommandAction string
	lostNotified        bool
}

func newState() *state {
	meetingNames := map[string]string{}
	if loaded, err := loadMeetingNames("meeting-names.json"); err == nil {
		meetingNames = loaded
	}

	return &state{
		rooms:           make(map[string]*roomState),
		meetingNames:    meetingNames,
		agentClients:    make(map[*agentClient]struct{}),
		adminClients:    make(map[*adminClient]struct{}),
		commandRevision: make(map[string]int),
	}
}

func loadMeetingNames(path string) (map[string]string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var mapping map[string]string
	if err := json.Unmarshal(data, &mapping); err != nil {
		return nil, err
	}
	if mapping == nil {
		mapping = map[string]string{}
	}
	return mapping, nil
}

func (s *state) ensureRoom(roomName string) *roomState {
	roomName = strings.TrimSpace(roomName)
	if roomName == "" {
		return &roomState{}
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	room, ok := s.rooms[roomName]
	if !ok {
		room = &roomState{roomName: roomName, status: "ready", updatedAt: time.Now().UTC()}
		s.rooms[roomName] = room
	}
	return room
}

func (s *state) issueCommand(roomName, action, targetMeetingID string) (command, *roomState) {
	now := time.Now().UTC()
	roomName = strings.TrimSpace(roomName)
	action = strings.TrimSpace(action)
	targetMeetingID = strings.TrimSpace(targetMeetingID)

	s.mu.Lock()
	defer s.mu.Unlock()

	room, ok := s.rooms[roomName]
	if !ok {
		room = &roomState{roomName: roomName, status: "ready"}
		s.rooms[roomName] = room
	}

	s.commandRevision[roomName]++
	revision := s.commandRevision[roomName]
	cmdID := newID("cmd")

	room.pendingCommandID = cmdID
	room.pendingCommandAction = action
	room.lastCommandID = cmdID
	room.lastCommandResult = "pending"
	room.lastError = ""
	room.updatedAt = now

	switch action {
	case "goto", "goto_and_start":
		room.status = "switching"
	case "start":
		room.status = "in_progress"
	case "pause":
		room.status = "paused"
	default:
		room.status = "ready"
	}

	return command{
		CommandID:       cmdID,
		RoomName:        roomName,
		Action:          action,
		TargetMeetingID: targetMeetingID,
		Revision:        revision,
		IssuedAt:        now,
	}, room
}

func (s *state) applyHeartbeat(h snapshotHeartbeat) (roomView, bool) {
	now := time.Now().UTC()

	s.mu.Lock()
	defer s.mu.Unlock()

	room, ok := s.rooms[h.RoomName]
	if !ok {
		room = &roomState{roomName: h.RoomName}
		s.rooms[h.RoomName] = room
	}

	recovered := room.lostNotified
	room.roomName = h.RoomName
	room.agentID = h.AgentID
	if h.Status != "" {
		room.status = h.Status
	}
	room.currentMeetingID = h.CurrentMeetingID
	room.lastHeartbeatAt = now
	room.updatedAt = now
	room.lastCommandID = h.LastCommandID
	room.lastCommandResult = h.LastCommandResult
	room.lastError = h.LastError
	room.lostNotified = false

	if room.status == "" {
		room.status = "ready"
	}

	return room.snapshotLocked(s.meetingNames), recovered
}

func (s *state) markLostRooms(timeout time.Duration) []*roomState {
	now := time.Now().UTC()

	s.mu.Lock()
	defer s.mu.Unlock()

	var lost []*roomState
	for _, room := range s.rooms {
		if room.lastHeartbeatAt.IsZero() || room.lostNotified {
			continue
		}
		if now.Sub(room.lastHeartbeatAt) > timeout {
			room.status = "lost"
			room.lastError = "heartbeat timeout"
			room.updatedAt = now
			room.lostNotified = true
			lost = append(lost, room)
		}
	}
	return lost
}

func (s *state) snapshotRooms() []roomView {
	s.mu.RLock()
	defer s.mu.RUnlock()

	views := make([]roomView, 0, len(s.rooms))
	for _, room := range s.rooms {
		views = append(views, room.snapshotLocked(s.meetingNames))
	}
	sort.Slice(views, func(i, j int) bool {
		return views[i].RoomName < views[j].RoomName
	})
	return views
}

func (s *state) snapshotRoom(roomName string) roomView {
	s.mu.RLock()
	defer s.mu.RUnlock()

	if room, ok := s.rooms[roomName]; ok {
		return room.snapshotLocked(s.meetingNames)
	}
	return roomView{RoomName: roomName, Status: "ready"}
}

func (s *state) broadcastAgents(data []byte) {
	s.mu.RLock()
	clients := make([]*agentClient, 0, len(s.agentClients))
	for client := range s.agentClients {
		clients = append(clients, client)
	}
	s.mu.RUnlock()

	for _, client := range clients {
		client.enqueue(data)
	}
}

func (s *state) broadcastAdmins(data []byte) {
	s.mu.RLock()
	clients := make([]*adminClient, 0, len(s.adminClients))
	for client := range s.adminClients {
		clients = append(clients, client)
	}
	s.mu.RUnlock()

	for _, client := range clients {
		client.enqueue(data)
	}
}

func (s *state) registerAgent(client *agentClient) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.agentClients[client] = struct{}{}
}

func (s *state) unregisterAgent(client *agentClient) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.agentClients, client)
}

func (s *state) registerAdmin(client *adminClient) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.adminClients[client] = struct{}{}
}

func (s *state) unregisterAdmin(client *adminClient) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.adminClients, client)
}

func (r *roomState) snapshot(meetingNames map[string]string) roomView {
	return r.snapshotLocked(meetingNames)
}

func (r *roomState) snapshotLocked(meetingNames map[string]string) roomView {
	meetingName := meetingNames[r.currentMeetingID]
	age := 0.0
	if !r.lastHeartbeatAt.IsZero() {
		age = time.Since(r.lastHeartbeatAt).Seconds()
	}
	return roomView{
		RoomName:            r.roomName,
		AgentID:             r.agentID,
		Status:              r.status,
		CurrentMeetingID:    r.currentMeetingID,
		CurrentMeetingName:   meetingName,
		LastCommandID:       r.lastCommandID,
		LastCommandResult:   r.lastCommandResult,
		LastError:           r.lastError,
		LastHeartbeatAt:     r.lastHeartbeatAt,
		HeartbeatAgeSeconds: age,
		PendingCommandID:    r.pendingCommandID,
		PendingCommandAction: r.pendingCommandAction,
		UpdatedAt:            r.updatedAt,
	}
}

func newID(prefix string) string {
	var raw [8]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return prefix + "-" + time.Now().UTC().Format("20060102150405.000000000")
	}
	return prefix + "-" + hex.EncodeToString(raw[:])
}
