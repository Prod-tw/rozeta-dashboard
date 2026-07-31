package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"sort"
	"strings"
	"sync"
	"time"
)

var (
	errCommandPending = errors.New("room already has a pending command")
	errRoomNotReady   = errors.New("room meeting state is not ready")
	errCurrentUnknown = errors.New("current meeting is unknown; send goto first")
)

type state struct {
	mu           sync.RWMutex
	rooms        map[string]*roomState
	meetingNames map[string]string
	adminClients map[*adminClient]struct{}
	revisions    map[string]int
}

type roomState struct {
	roomName           string
	status             string
	currentMeetingID   string
	currentFromGoto    bool
	apiStatus          string
	lastSyncedAt       time.Time
	syncError          string
	commandError       string
	lastCommandID      string
	lastCommandAction  string
	lastCommandResult  string
	lastCommandTarget  string
	lastExpectedStatus string
	updatedAt          time.Time
	pending            *pendingCommand
}

type pendingCommand struct {
	CommandID      string
	Action         string
	TargetID       string
	ExpectedStatus string
	IssuedAt       time.Time
}

func newState() *state {
	meetingNames := map[string]string{}
	if loaded, err := loadMeetingNames("meeting-names.json"); err == nil {
		meetingNames = loaded
	}

	return &state{
		rooms:        make(map[string]*roomState),
		meetingNames: meetingNames,
		adminClients: make(map[*adminClient]struct{}),
		revisions:    make(map[string]int),
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

// Rooms previously appeared only after a userscript heartbeat. Direct API control
// has no agent, so every configured token now creates a syncing room at startup.
func (s *state) seedRooms(roomNames []string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	for _, roomName := range roomNames {
		roomName = strings.TrimSpace(roomName)
		if roomName == "" {
			continue
		}
		if _, exists := s.rooms[roomName]; exists {
			continue
		}
		s.rooms[roomName] = &roomState{
			roomName:  roomName,
			status:    "unknown",
			apiStatus: "syncing",
			updatedAt: time.Now().UTC(),
		}
	}
}

func (s *state) beginCommand(roomName, action, targetID, expectedStatus string) (command, roomView, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	room, ok := s.rooms[roomName]
	if !ok {
		return command{}, roomView{}, errors.New("unknown room")
	}
	if room.pending != nil {
		return command{}, roomView{}, errCommandPending
	}
	if room.apiStatus == "authentication_error" {
		return command{}, roomView{}, errRoomNotReady
	}
	if (action == "start" || action == "pause") && room.apiStatus != "synced" {
		return command{}, roomView{}, errRoomNotReady
	}
	if (action == "start" || action == "pause") && room.currentMeetingID == "" {
		return command{}, roomView{}, errCurrentUnknown
	}

	if action == "start" || action == "pause" {
		targetID = room.currentMeetingID
	}

	now := time.Now().UTC()
	s.revisions[roomName]++
	cmd := command{
		CommandID:       newID("cmd"),
		RoomName:        roomName,
		Action:          action,
		TargetMeetingID: targetID,
		Revision:        s.revisions[roomName],
		IssuedAt:        now,
	}
	room.pending = &pendingCommand{
		CommandID:      cmd.CommandID,
		Action:         action,
		TargetID:       targetID,
		ExpectedStatus: expectedStatus,
		IssuedAt:       now,
	}
	room.lastCommandID = cmd.CommandID
	room.lastCommandAction = action
	room.lastCommandResult = "pending"
	room.lastCommandTarget = targetID
	room.lastExpectedStatus = expectedStatus
	room.commandError = ""
	room.updatedAt = now

	return cmd, room.snapshotLocked(s.meetingNames), nil
}

func (s *state) finishCommand(roomName, commandID, result, status, message string) (roomView, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	room, ok := s.rooms[roomName]
	if !ok || room.pending == nil || room.pending.CommandID != commandID {
		return roomView{}, false
	}
	pending := room.pending
	room.pending = nil
	room.lastCommandResult = result
	room.commandError = message
	room.updatedAt = time.Now().UTC()
	if status != "" && room.currentMeetingID == pending.TargetID {
		room.status = status
	}
	// A successful goto is the only API-only signal that identifies a ready meeting.
	// Previously the userscript URL heartbeat supplied this value; now the server keeps
	// the explicit target until Rozeta meeting status provides a newer observation.
	if pending.Action == "goto" && result == "confirmed" {
		room.currentMeetingID = pending.TargetID
		room.currentFromGoto = true
	}

	return room.snapshotLocked(s.meetingNames), true
}

func (s *state) applyMeetingSync(roomName string, meetings []roomMeetingView) (roomView, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	room, ok := s.rooms[roomName]
	if !ok {
		return roomView{}, false
	}
	before := room.snapshotLocked(s.meetingNames)
	now := time.Now().UTC()
	room.apiStatus = "synced"
	room.lastSyncedAt = now
	room.syncError = ""
	room.updatedAt = now

	meetingByID := make(map[string]roomMeetingView, len(meetings))
	inProgress := make([]roomMeetingView, 0, 1)
	paused := make([]roomMeetingView, 0, 1)
	for _, meeting := range meetings {
		meetingByID[meeting.ID] = meeting
		switch meeting.Status {
		case "in_progress":
			inProgress = append(inProgress, meeting)
		case "paused":
			paused = append(paused, meeting)
		}
	}

	current, currentFound := meetingByID[room.currentMeetingID]
	if room.currentFromGoto {
		if currentFound {
			room.status = current.Status
		} else {
			// A successful Goto remains authoritative for this process. Previously one
			// incomplete list response silently retargeted the room to another meeting;
			// now controls stay disabled until the explicit target is observable again.
			room.apiStatus = "stale"
			room.status = "unknown"
			room.syncError = "current goto meeting was not found in Rozeta"
		}
	} else {
		switch {
		case len(inProgress) == 1:
			room.currentMeetingID = inProgress[0].ID
			room.status = inProgress[0].Status
		case len(inProgress) > 1:
			room.currentMeetingID = ""
			room.status = "unknown"
			room.syncError = "multiple in-progress meetings; send goto first"
		case len(paused) == 1:
			room.currentMeetingID = paused[0].ID
			room.status = paused[0].Status
		case len(paused) > 1:
			room.currentMeetingID = ""
			room.status = "unknown"
			room.syncError = "multiple paused meetings; send goto first"
		default:
			room.currentMeetingID = ""
			room.status = "ready"
		}
	}

	// A timed-out command can complete after its 15-second confirmation window. The
	// old state stayed failed forever; periodic sync now upgrades that exact result.
	lateTarget, lateTargetFound := meetingByID[room.lastCommandTarget]
	lateConfirmed := room.pending == nil && room.lastCommandResult == "confirmation_timeout" && lateTargetFound &&
		lateTarget.Status == room.lastExpectedStatus
	if lateConfirmed {
		room.lastCommandResult = "confirmed_late"
		room.commandError = ""
	}

	after := room.snapshotLocked(s.meetingNames)
	return after, lateConfirmed || before.APIStatus != after.APIStatus || before.Status != after.Status ||
		before.CurrentMeetingID != after.CurrentMeetingID || before.LastCommandResult != after.LastCommandResult ||
		before.LastError != after.LastError
}

func (s *state) markSyncError(roomName, apiStatus, message string) (roomView, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	room, ok := s.rooms[roomName]
	if !ok {
		return roomView{}, false
	}
	if room.apiStatus == "authentication_error" && apiStatus == "stale" {
		return room.snapshotLocked(s.meetingNames), false
	}
	changed := room.apiStatus != apiStatus || room.syncError != message
	room.apiStatus = apiStatus
	room.syncError = message
	room.updatedAt = time.Now().UTC()
	return room.snapshotLocked(s.meetingNames), changed
}

func (s *state) snapshotRoom(roomName string) (roomView, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	room, ok := s.rooms[roomName]
	if !ok {
		return roomView{}, false
	}
	return room.snapshotLocked(s.meetingNames), true
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

func (r *roomState) snapshotLocked(meetingNames map[string]string) roomView {
	pendingID := ""
	pendingAction := ""
	pendingTarget := ""
	if r.pending != nil {
		pendingID = r.pending.CommandID
		pendingAction = r.pending.Action
		pendingTarget = r.pending.TargetID
	}
	lastError := r.commandError
	if lastError == "" {
		lastError = r.syncError
	}
	return roomView{
		RoomName:             r.roomName,
		Status:               r.status,
		APIStatus:            r.apiStatus,
		CurrentMeetingID:     r.currentMeetingID,
		CurrentMeetingName:   meetingNames[r.currentMeetingID],
		LastSyncedAt:         r.lastSyncedAt,
		LastCommandID:        r.lastCommandID,
		LastCommandAction:    r.lastCommandAction,
		LastCommandResult:    r.lastCommandResult,
		LastError:            lastError,
		PendingCommandID:     pendingID,
		PendingCommandAction: pendingAction,
		PendingCommandTarget: pendingTarget,
		UpdatedAt:            r.updatedAt,
	}
}

func newID(prefix string) string {
	var raw [8]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return prefix + "-fallback"
	}
	return prefix + "-" + hex.EncodeToString(raw[:])
}
