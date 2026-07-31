package main

import "testing"

func TestMeetingSyncResolvesOnlyUniqueActiveMeeting(t *testing.T) {
	tests := []struct {
		name       string
		meetings   []roomMeetingView
		wantID     string
		wantStatus string
		wantError  bool
	}{
		{
			name:       "unique in progress wins",
			meetings:   []roomMeetingView{{ID: "running", Status: "in_progress"}, {ID: "paused", Status: "paused"}},
			wantID:     "running",
			wantStatus: "in_progress",
		},
		{
			name:       "unique paused fallback",
			meetings:   []roomMeetingView{{ID: "paused", Status: "paused"}, {ID: "ready", Status: "ready"}},
			wantID:     "paused",
			wantStatus: "paused",
		},
		{
			name:       "ready meetings remain unresolved",
			meetings:   []roomMeetingView{{ID: "one", Status: "ready"}, {ID: "two", Status: "ready"}},
			wantStatus: "ready",
		},
		{
			name:       "multiple active meetings are ambiguous",
			meetings:   []roomMeetingView{{ID: "one", Status: "in_progress"}, {ID: "two", Status: "in_progress"}},
			wantStatus: "unknown",
			wantError:  true,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			state := newState()
			state.seedRooms([]string{"room-a"})
			room, _ := state.applyMeetingSync("room-a", test.meetings)
			if room.CurrentMeetingID != test.wantID || room.Status != test.wantStatus {
				t.Fatalf("room = %#v, want meeting %q status %q", room, test.wantID, test.wantStatus)
			}
			if (room.LastError != "") != test.wantError {
				t.Fatalf("last error = %q, wantError %v", room.LastError, test.wantError)
			}
		})
	}
}

func TestSuccessfulGotoRemainsCurrentWhileReady(t *testing.T) {
	state := newState()
	state.seedRooms([]string{"room-a"})
	command, _, err := state.beginCommand("room-a", "goto", "target", "")
	if err != nil {
		t.Fatalf("beginCommand() error = %v", err)
	}
	if _, ok := state.finishCommand("room-a", command.CommandID, "confirmed", "", ""); !ok {
		t.Fatal("finishCommand() did not update room")
	}
	room, _ := state.applyMeetingSync("room-a", []roomMeetingView{{ID: "target", Status: "ready"}, {ID: "other", Status: "paused"}})
	if room.CurrentMeetingID != "target" || room.Status != "ready" {
		t.Fatalf("room = %#v, want ready target", room)
	}
}

func TestInferredMeetingIsReevaluatedOnEverySync(t *testing.T) {
	state := newState()
	state.seedRooms([]string{"room-a"})
	state.applyMeetingSync("room-a", []roomMeetingView{{ID: "first", Status: "in_progress"}})
	room, _ := state.applyMeetingSync("room-a", []roomMeetingView{
		{ID: "first", Status: "completed"},
		{ID: "second", Status: "in_progress"},
	})
	if room.CurrentMeetingID != "second" || room.Status != "in_progress" {
		t.Fatalf("room = %#v, want newly active second meeting", room)
	}
}

func TestTimedOutCommandCanConfirmLate(t *testing.T) {
	state := newState()
	state.seedRooms([]string{"room-a"})
	state.applyMeetingSync("room-a", []roomMeetingView{{ID: "meeting-a", Status: "paused"}})
	command, _, err := state.beginCommand("room-a", "start", "", "in_progress")
	if err != nil {
		t.Fatalf("beginCommand() error = %v", err)
	}
	state.finishCommand("room-a", command.CommandID, "confirmation_timeout", "", "timed out")
	room, _ := state.applyMeetingSync("room-a", []roomMeetingView{{ID: "meeting-a", Status: "in_progress"}})
	if room.LastCommandResult != "confirmed_late" {
		t.Fatalf("result = %q, want confirmed_late", room.LastCommandResult)
	}
}

func TestAuthenticationErrorDisablesEveryCommand(t *testing.T) {
	state := newState()
	state.seedRooms([]string{"room-a"})
	state.markSyncError("room-a", "authentication_error", "expired token")
	for _, action := range []string{"goto", "start", "pause", "resume"} {
		if _, _, err := state.beginCommand("room-a", action, "meeting-a", ""); err != errRoomNotReady {
			t.Fatalf("beginCommand(%s) error = %v, want %v", action, err, errRoomNotReady)
		}
	}
	room, _ := state.applyMeetingSync("room-a", []roomMeetingView{{ID: "meeting-a", Status: "paused"}})
	if room.APIStatus != "synced" || room.CurrentMeetingID != "meeting-a" {
		t.Fatalf("room did not recover after successful sync: %#v", room)
	}
}

func TestGotoTargetRemainsAuthoritativeWhenListOmitsIt(t *testing.T) {
	state := newState()
	state.seedRooms([]string{"room-a"})
	command, _, err := state.beginCommand("room-a", "goto", "target", "")
	if err != nil {
		t.Fatalf("beginCommand() error = %v", err)
	}
	state.finishCommand("room-a", command.CommandID, "confirmed", "", "")
	room, _ := state.applyMeetingSync("room-a", []roomMeetingView{{ID: "other", Status: "in_progress"}})
	if room.CurrentMeetingID != "target" || room.APIStatus != "stale" {
		t.Fatalf("room = %#v, want stale authoritative target", room)
	}
}

func TestSuccessfulSyncPreservesCommandError(t *testing.T) {
	state := newState()
	state.seedRooms([]string{"room-a"})
	state.applyMeetingSync("room-a", []roomMeetingView{{ID: "meeting-a", Status: "paused"}})
	command, _, err := state.beginCommand("room-a", "start", "", "in_progress")
	if err != nil {
		t.Fatalf("beginCommand() error = %v", err)
	}
	state.finishCommand("room-a", command.CommandID, "confirmation_timeout", "", "command timed out")
	room, _ := state.applyMeetingSync("room-a", []roomMeetingView{{ID: "meeting-a", Status: "paused"}})
	if room.LastError != "command timed out" {
		t.Fatalf("last error = %q, want command diagnostic", room.LastError)
	}
}

func TestCommandCompletionDoesNotOverwriteNewCurrentMeeting(t *testing.T) {
	state := newState()
	state.seedRooms([]string{"room-a"})
	state.applyMeetingSync("room-a", []roomMeetingView{{ID: "first", Status: "in_progress"}})
	command, _, err := state.beginCommand("room-a", "pause", "", "paused")
	if err != nil {
		t.Fatalf("beginCommand() error = %v", err)
	}
	state.applyMeetingSync("room-a", []roomMeetingView{
		{ID: "first", Status: "completed"},
		{ID: "second", Status: "in_progress"},
	})
	state.finishCommand("room-a", command.CommandID, "confirmed", "paused", "")
	room, _ := state.snapshotRoom("room-a")
	if room.CurrentMeetingID != "second" || room.Status != "in_progress" {
		t.Fatalf("room = %#v, want unchanged second meeting", room)
	}
}
