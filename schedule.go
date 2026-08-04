package main

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"
)

const opassScheduleURL = "https://coscup.org/2026/api/opass.json"

type meetingSchedule struct {
	enabled   bool
	starts    map[string]time.Time
	snapshots map[string][]roomMeetingView
}

var (
	errScheduleUnavailable       = errors.New("meeting schedule is unavailable")
	errCurrentMeetingUnscheduled = errors.New("current desired meeting is not scheduled")
	errNextMeetingNotFound       = errors.New("next scheduled meeting was not found")
)

type scheduleWarning struct {
	Line      int
	MeetingID string
	SessionID string
	Reason    string
}

type sessionMapping struct {
	line      int
	meetingID string
	sessionID string
}

type opassSchedule struct {
	Sessions []struct {
		ID    string `json:"id"`
		Start string `json:"start"`
	} `json:"sessions"`
}

type scheduleLoadOptions struct {
	url         string
	client      *http.Client
	retryDelays []time.Duration
}

type scheduleRemoteError struct{ err error }

func (e *scheduleRemoteError) Error() string { return e.err.Error() }
func (e *scheduleRemoteError) Unwrap() error { return e.err }

func loadMeetingSchedule(ctx context.Context, path string) (meetingSchedule, []scheduleWarning, error) {
	return loadMeetingScheduleWithOptions(ctx, path, scheduleLoadOptions{
		url:         opassScheduleURL,
		client:      &http.Client{Timeout: 10 * time.Second},
		retryDelays: []time.Duration{time.Second, 2 * time.Second},
	})
}

func loadMeetingScheduleWithOptions(
	ctx context.Context,
	path string,
	options scheduleLoadOptions,
) (meetingSchedule, []scheduleWarning, error) {
	opass, err := fetchOPASSSchedule(ctx, options)
	if err != nil {
		return meetingSchedule{}, nil, &scheduleRemoteError{err: err}
	}
	sessionStarts, err := indexOPASSSessions(opass)
	if err != nil {
		return meetingSchedule{}, nil, &scheduleRemoteError{err: err}
	}
	mappings, warnings, err := loadSessionMappings(path)
	if err != nil {
		return meetingSchedule{}, nil, err
	}
	if len(warnings) > 0 {
		// Previously malformed session rows were warnings and the UI could silently
		// fall back to API order. Strict startup validation now rejects them before
		// any meeting list can be served without a complete schedule.
		return meetingSchedule{}, nil, fmt.Errorf("session CSV contains invalid rows: %s", warnings[0].Reason)
	}

	starts := make(map[string]time.Time, len(mappings))
	mappedSessionIDs := make(map[string]struct{}, len(mappings))
	for _, mapping := range mappings {
		startValue, found := sessionStarts[mapping.sessionID]
		if !found {
			log.Printf("ignoring unmatched session mapping: csv_line=%d meeting_id=%q session_id=%q reason=session_not_in_opass", mapping.line, mapping.meetingID, mapping.sessionID)
			continue
		}
		start, err := time.Parse(time.RFC3339, startValue)
		if err != nil {
			return meetingSchedule{}, nil, fmt.Errorf("session CSV line %d meeting %q has invalid opass start time %q", mapping.line, mapping.meetingID, startValue)
		}
		starts[mapping.meetingID] = start
		mappedSessionIDs[mapping.sessionID] = struct{}{}
	}
	for sessionID := range sessionStarts {
		if _, found := mappedSessionIDs[sessionID]; !found {
			log.Printf("ignoring unmatched opass session: session_id=%q reason=session_not_in_session_csv", sessionID)
		}
	}

	return meetingSchedule{enabled: true, starts: starts, snapshots: make(map[string][]roomMeetingView)}, warnings, nil
}

func loadSessionMappings(path string) ([]sessionMapping, []scheduleWarning, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, nil, err
	}
	defer file.Close()

	records, err := csv.NewReader(file).ReadAll()
	if err != nil {
		return nil, nil, err
	}
	if len(records) == 0 {
		return nil, nil, errors.New("session CSV has no header")
	}

	meetingColumn := headerColumn(records[0], "議程 ID")
	sessionColumn := headerColumn(records[0], "Session ID")
	if meetingColumn < 0 || sessionColumn < 0 {
		return nil, nil, errors.New("session CSV must contain 議程 ID and Session ID headers")
	}

	mappings := make([]sessionMapping, 0, len(records)-1)
	warnings := make([]scheduleWarning, 0)
	meetingLines := make(map[string]int)
	sessionLines := make(map[string]int)
	for index, record := range records[1:] {
		line := index + 2
		meetingID := strings.TrimSpace(record[meetingColumn])
		sessionID := strings.TrimSpace(record[sessionColumn])
		if previousLine, duplicate := meetingLines[meetingID]; meetingID != "" && duplicate {
			return nil, nil, fmt.Errorf(
				"session CSV line %d duplicates meeting ID %q from line %d",
				line,
				meetingID,
				previousLine,
			)
		}
		if previousLine, duplicate := sessionLines[sessionID]; sessionID != "" && duplicate {
			return nil, nil, fmt.Errorf(
				"session CSV line %d duplicates session ID %q from line %d",
				line,
				sessionID,
				previousLine,
			)
		}
		// Empty rows previously skipped duplicate tracking for their non-empty ID. Track
		// each usable ID first so malformed rows cannot hide an ambiguous mapping.
		if meetingID != "" {
			meetingLines[meetingID] = line
		}
		if sessionID != "" {
			sessionLines[sessionID] = line
		}
		if meetingID == "" || sessionID == "" {
			warnings = append(warnings, scheduleWarning{
				Line:      line,
				MeetingID: meetingID,
				SessionID: sessionID,
				Reason:    "empty meeting ID or session ID",
			})
			continue
		}
		mappings = append(mappings, sessionMapping{line: line, meetingID: meetingID, sessionID: sessionID})
	}
	if len(mappings) == 0 {
		warnings = append(warnings, scheduleWarning{Reason: "session CSV contains no valid mappings"})
	}
	return mappings, warnings, nil
}

func headerColumn(header []string, name string) int {
	for index, value := range header {
		if strings.TrimSpace(value) == name {
			return index
		}
	}
	return -1
}

func fetchOPASSSchedule(ctx context.Context, options scheduleLoadOptions) (opassSchedule, error) {
	client := options.client
	if client == nil {
		client = http.DefaultClient
	}
	requestURL := strings.TrimSpace(options.url)
	if requestURL == "" {
		requestURL = opassScheduleURL
	}

	var lastErr error
	for attempt := 0; attempt <= len(options.retryDelays); attempt++ {
		if attempt > 0 {
			delay := options.retryDelays[attempt-1]
			timer := time.NewTimer(delay)
			select {
			case <-ctx.Done():
				timer.Stop()
				return opassSchedule{}, ctx.Err()
			case <-timer.C:
			}
		}
		opass, err := fetchOPASSScheduleOnce(ctx, client, requestURL)
		if err == nil {
			return opass, nil
		}
		lastErr = err
	}
	return opassSchedule{}, fmt.Errorf("load opass schedule after %d attempts: %w", len(options.retryDelays)+1, lastErr)
}

func fetchOPASSScheduleOnce(ctx context.Context, client *http.Client, requestURL string) (opassSchedule, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, requestURL, nil)
	if err != nil {
		return opassSchedule{}, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return opassSchedule{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return opassSchedule{}, fmt.Errorf("opass API returned %s", resp.Status)
	}

	var opass opassSchedule
	if err := json.NewDecoder(resp.Body).Decode(&opass); err != nil {
		return opassSchedule{}, fmt.Errorf("decode opass schedule: %w", err)
	}
	return opass, nil
}

func indexOPASSSessions(opass opassSchedule) (map[string]string, error) {
	starts := make(map[string]string, len(opass.Sessions))
	for _, session := range opass.Sessions {
		id := strings.TrimSpace(session.ID)
		if id == "" {
			return nil, errors.New("opass contains a session with an empty ID")
		}
		start := strings.TrimSpace(session.Start)
		if start == "" {
			return nil, fmt.Errorf("opass session %q has an empty start time", id)
		}
		if _, err := time.Parse(time.RFC3339, start); err != nil {
			return nil, fmt.Errorf("opass session %q has invalid start time %q", id, start)
		}
		if _, duplicate := starts[id]; duplicate {
			return nil, fmt.Errorf("opass duplicates session ID %q", id)
		}
		starts[id] = start
	}
	return starts, nil
}

func (schedule *meetingSchedule) validateStartupMeetings(roomName string, meetings []roomMeetingView) ([]roomMeetingView, error) {
	prepared, err := schedule.validateMeetings(roomName, meetings)
	if err != nil {
		return nil, err
	}
	if schedule.snapshots == nil {
		schedule.snapshots = make(map[string][]roomMeetingView)
	}
	schedule.snapshots[roomName] = cloneMeetings(prepared)
	return prepared, nil
}

func (schedule meetingSchedule) validateMeetings(roomName string, meetings []roomMeetingView) ([]roomMeetingView, error) {
	if !schedule.enabled {
		return nil, errScheduleUnavailable
	}
	seenStarts := make(map[int64]string, len(meetings))
	prepared := make([]roomMeetingView, 0, len(meetings))
	for _, meeting := range meetings {
		start, found := schedule.starts[meeting.ID]
		if !found {
			log.Printf("ignoring unmatched Rozeta meeting: room=%q meeting_id=%q reason=meeting_not_in_opass_session_csv", roomName, meeting.ID)
			continue
		}
		startInstant := start.UnixNano()
		if previous, duplicate := seenStarts[startInstant]; duplicate {
			return nil, fmt.Errorf("room %q meetings %q and %q have the same opass start time %s", roomName, previous, meeting.ID, start.Format(time.RFC3339))
		}
		seenStarts[startInstant] = meeting.ID
		copy := meeting
		copy.ScheduledStart = &start
		prepared = append(prepared, copy)
	}
	if snapshot, found := schedule.snapshots[roomName]; found {
		filtered := prepared[:0]
		for _, meeting := range prepared {
			if _, exists := meetingByID(snapshot, meeting.ID); !exists {
				log.Printf("ignoring unmatched Rozeta meeting: room=%q meeting_id=%q reason=meeting_not_in_startup_snapshot", roomName, meeting.ID)
				continue
			}
			filtered = append(filtered, meeting)
		}
		prepared = filtered
		if err := validateMeetingIdentity(snapshot, prepared, roomName); err != nil {
			return nil, err
		}
		current := make(map[string]roomMeetingView, len(prepared))
		for _, meeting := range prepared {
			current[meeting.ID] = meeting
		}
		ordered := make([]roomMeetingView, 0, len(snapshot))
		for _, original := range snapshot {
			if updated, found := current[original.ID]; found {
				ordered = append(ordered, updated)
			}
		}
		return ordered, nil
	}
	sort.Slice(prepared, func(left, right int) bool {
		return prepared[left].ScheduledStart.Before(*prepared[right].ScheduledStart)
	})
	return prepared, nil
}

func validateMeetingIdentity(snapshot, current []roomMeetingView, roomName string) error {
	currentIDs := make(map[string]struct{}, len(current))
	for _, meeting := range current {
		currentIDs[meeting.ID] = struct{}{}
	}
	for _, original := range snapshot {
		if _, found := currentIDs[original.ID]; !found {
			log.Printf("ignoring missing Rozeta meeting: room=%q meeting_id=%q reason=meeting_not_in_current_rozeta_result", roomName, original.ID)
		}
	}
	for _, meeting := range current {
		original, found := meetingByID(snapshot, meeting.ID)
		if !found {
			continue
		}
		if meeting.Title != original.Title || meeting.Source != original.Source || meeting.Target != original.Target {
			return fmt.Errorf("room %q meeting %q immutable metadata changed", roomName, original.ID)
		}
	}
	return nil
}

func cloneMeetings(meetings []roomMeetingView) []roomMeetingView {
	cloned := append([]roomMeetingView{}, meetings...)
	for index := range cloned {
		if cloned[index].ScheduledStart != nil {
			start := *cloned[index].ScheduledStart
			cloned[index].ScheduledStart = &start
		}
	}
	return cloned
}

func (schedule meetingSchedule) prepareMeetings(meetings []roomMeetingView) []roomMeetingView {
	prepared := append([]roomMeetingView{}, meetings...)
	if !schedule.enabled {
		sort.Slice(prepared, func(left, right int) bool {
			return meetingTitleBefore(prepared[left], prepared[right])
		})
		return prepared
	}
	for index := range prepared {
		if start, found := schedule.starts[prepared[index].ID]; found {
			prepared[index].ScheduledStart = &start
		}
	}
	if schedule.enabled {
		sort.Slice(prepared, func(left, right int) bool {
			leftStart, rightStart := prepared[left].ScheduledStart, prepared[right].ScheduledStart
			if leftStart == nil || rightStart == nil {
				return leftStart != nil
			}
			if !leftStart.Equal(*rightStart) {
				return leftStart.Before(*rightStart)
			}
			return meetingTitleBefore(prepared[left], prepared[right])
		})
	}
	return prepared
}

func (schedule meetingSchedule) nextMeeting(meetings []roomMeetingView, currentID string) (roomMeetingView, error) {
	// WHY: Rozeta's response order is not a schedule. Previously the UI could only display
	// that order; advance now sorts the known scheduled meetings deterministically and rejects
	// an unscheduled current target instead of guessing which meeting comes next.
	if !schedule.enabled || len(schedule.starts) == 0 {
		return roomMeetingView{}, errScheduleUnavailable
	}
	if _, scheduled := schedule.starts[currentID]; !scheduled {
		return roomMeetingView{}, errCurrentMeetingUnscheduled
	}
	ordered := make([]roomMeetingView, 0, len(meetings))
	for _, meeting := range meetings {
		start, found := schedule.starts[meeting.ID]
		if !found {
			continue
		}
		copy := meeting
		copy.ScheduledStart = &start
		ordered = append(ordered, copy)
	}
	sort.Slice(ordered, func(left, right int) bool {
		leftStart := ordered[left].ScheduledStart
		rightStart := ordered[right].ScheduledStart
		if !leftStart.Equal(*rightStart) {
			return leftStart.Before(*rightStart)
		}
		return meetingTitleBefore(ordered[left], ordered[right])
	})
	currentIndex := -1
	for index, meeting := range ordered {
		if meeting.ID == currentID {
			currentIndex = index
			break
		}
	}
	if currentIndex < 0 || currentIndex+1 >= len(ordered) {
		return roomMeetingView{}, errNextMeetingNotFound
	}
	return ordered[currentIndex+1], nil
}

func meetingTitleBefore(left, right roomMeetingView) bool {
	if left.Title != right.Title {
		return left.Title < right.Title
	}
	return left.ID < right.ID
}
