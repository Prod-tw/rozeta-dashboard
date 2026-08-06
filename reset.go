package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
)

const resetReadyAttempts = 5

var (
	errResetConfirmationRequired = errors.New("reset confirmation is required")
	errResetNotStopped           = errors.New("room must be stopped before reset")
	errResetActive               = errors.New("room still has in-progress meetings")
)

type resetReadyRequest struct {
	Epoch                     string   `json:"epoch"`
	ExpectedReconciliationRun *uint64  `json:"expected_reconciliation_run"`
	ExpectedGeneration        *uint64  `json:"expected_generation"`
	MeetingIDs                []string `json:"meeting_ids"`
	Confirmed                 bool     `json:"confirmed"`
}

type resetReadyMeeting struct {
	MeetingID string `json:"meeting_id"`
	Status    string `json:"status"`
	Action    string `json:"action"`
}

type resetReadyResult struct {
	MeetingID string `json:"meeting_id"`
	Status    string `json:"status"`
	Outcome   string `json:"outcome"`
	Attempts  int    `json:"attempts"`
	Error     string `json:"error,omitempty"`
}

type resetReadyPreflightResponse struct {
	Epoch    string              `json:"epoch"`
	Room     roomView            `json:"room"`
	Meetings []resetReadyMeeting `json:"meetings"`
}

type resetReadyResponse struct {
	Epoch   string             `json:"epoch"`
	Room    roomView           `json:"room"`
	Results []resetReadyResult `json:"results"`
}

func (a *app) handleResetReadyPreflight(c *gin.Context) {
	if a.controller == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "controller unavailable"})
		return
	}
	var request resetReadyRequest
	if err := c.ShouldBindJSON(&request); err != nil || request.ExpectedReconciliationRun == nil || request.ExpectedGeneration == nil || strings.TrimSpace(request.Epoch) == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "epoch, expected_reconciliation_run, and expected_generation are required"})
		return
	}
	roomName := strings.TrimSpace(c.Param("roomName"))
	room, meetings, err := a.controller.resetReadyPreflight(c.Request.Context(), roomName, request.Epoch, *request.ExpectedReconciliationRun, *request.ExpectedGeneration)
	if err != nil {
		a.writeResetReadyError(c, err, room)
		return
	}
	preflight := make([]resetReadyMeeting, 0, len(meetings))
	for _, meeting := range meetings {
		action := "already_ready"
		if meeting.Status == "paused" || meeting.Status == "completed" {
			action = "resume"
		}
		preflight = append(preflight, resetReadyMeeting{MeetingID: meeting.ID, Status: meeting.Status, Action: action})
	}
	c.JSON(http.StatusOK, resetReadyPreflightResponse{Epoch: a.epoch, Room: room, Meetings: preflight})
}

func (a *app) handleResetReady(c *gin.Context) {
	if a.controller == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "controller unavailable"})
		return
	}
	var request resetReadyRequest
	if err := c.ShouldBindJSON(&request); err != nil || request.ExpectedReconciliationRun == nil || request.ExpectedGeneration == nil || strings.TrimSpace(request.Epoch) == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "epoch, expected_reconciliation_run, and expected_generation are required"})
		return
	}
	if !request.Confirmed {
		c.JSON(http.StatusUnprocessableEntity, gin.H{"error": errResetConfirmationRequired.Error()})
		return
	}
	roomName := strings.TrimSpace(c.Param("roomName"))
	room, results, err := a.controller.resetReadyRoom(c.Request.Context(), roomName, request.Epoch, *request.ExpectedReconciliationRun, *request.ExpectedGeneration, request.MeetingIDs)
	if err != nil {
		a.writeResetReadyError(c, err, room)
		return
	}
	c.JSON(http.StatusAccepted, resetReadyResponse{Epoch: a.epoch, Room: room, Results: results})
}

func (a *app) writeResetReadyError(c *gin.Context, err error, room roomView) {
	status := http.StatusConflict
	var apiErr *rozetaAPIError
	switch {
	case errors.Is(err, errUnknownRoom):
		status = http.StatusNotFound
	case errors.Is(err, errResetConfirmationRequired):
		status = http.StatusUnprocessableEntity
	case errors.Is(err, context.DeadlineExceeded), errors.As(err, &apiErr):
		status = http.StatusServiceUnavailable
	}
	response := gin.H{"error": err.Error(), "epoch": a.epoch}
	if room.RoomName != "" {
		response["room"] = room
	}
	c.JSON(status, response)
}

func (c *controller) resetReadyPreflight(ctx context.Context, roomName, epoch string, expectedRun, expectedGeneration uint64) (roomView, []roomMeetingView, error) {
	// Reset used to have no controller operation. This fresh active-set check makes
	// the new destructive action available only after the stopped-room invariant is
	// observed from Rozeta, instead of trusting a stale browser lifecycle snapshot.
	c.mu.RLock()
	room, found := c.rooms[roomName]
	if !found {
		c.mu.RUnlock()
		return roomView{}, nil, errUnknownRoom
	}
	if epoch != c.app.epoch || room.reconciliationRun != expectedRun || room.desired.Generation != expectedGeneration {
		view := c.snapshotLocked(room)
		c.mu.RUnlock()
		return view, nil, errReconciliationConflict
	}
	if room.lifecycle != reconciliationSuspended || room.resetting {
		view := c.snapshotLocked(room)
		c.mu.RUnlock()
		return view, nil, errResetNotStopped
	}
	c.mu.RUnlock()

	active, err := c.listActiveMeetings(ctx, room)
	if err != nil {
		return c.snapshotRoom(room), nil, err
	}
	if len(active) != 0 {
		c.recordResetObservation(room, active, false)
		return c.snapshotRoom(room), nil, errResetActive
	}
	meetings, err := c.listMeetings(ctx, room)
	if err != nil {
		return c.snapshotRoom(room), nil, err
	}
	view := c.recordResetObservation(room, active, true)
	return view, meetings, nil
}

func (c *controller) resetReadyRoom(ctx context.Context, roomName, epoch string, expectedRun, expectedGeneration uint64, expectedMeetingIDs []string) (resultView roomView, results []resetReadyResult, resultErr error) {
	c.mu.RLock()
	room, found := c.rooms[roomName]
	if !found {
		c.mu.RUnlock()
		return roomView{}, nil, errUnknownRoom
	}
	c.mu.RUnlock()

	room.resetMu.Lock()
	defer room.resetMu.Unlock()

	c.mu.Lock()
	if epoch != c.app.epoch || room.reconciliationRun != expectedRun || room.desired.Generation != expectedGeneration {
		view := c.snapshotLocked(room)
		c.mu.Unlock()
		return view, nil, errReconciliationConflict
	}
	if room.lifecycle != reconciliationSuspended || room.resetting {
		view := c.snapshotLocked(room)
		c.mu.Unlock()
		return view, nil, errResetNotStopped
	}
	room.resetting = true
	room.revision++
	view := c.snapshotLocked(room)
	c.mu.Unlock()
	c.publish(view)

	defer func() {
		c.mu.Lock()
		room.resetting = false
		room.revision++
		resultView = c.snapshotLocked(room)
		c.mu.Unlock()
		c.publish(resultView)
	}()

	active, err := c.listActiveMeetings(ctx, room)
	if err != nil {
		return c.snapshotRoom(room), nil, err
	}
	if len(active) != 0 {
		c.recordResetObservation(room, active, false)
		return c.snapshotRoom(room), nil, errResetActive
	}
	meetings, err := c.listMeetings(ctx, room)
	if err != nil {
		return c.snapshotRoom(room), nil, err
	}
	ids := meetingIDs(meetings)
	wanted := append([]string{}, expectedMeetingIDs...)
	sort.Strings(wanted)
	if !slices.Equal(ids, wanted) {
		return c.snapshotRoom(room), nil, errReconciliationConflict
	}
	c.recordResetObservation(room, active, true)

	results = resetReadyMeetings(ctx, c, room, meetings, expectedRun, expectedGeneration)
	if refreshed, refreshErr := c.listMeetings(ctx, room); refreshErr == nil {
		c.cacheResetMeetings(room, expectedRun, expectedGeneration, refreshed)
	}
	return c.snapshotRoom(room), results, nil
}

func resetReadyMeetings(ctx context.Context, c *controller, room *controllerRoom, meetings []roomMeetingView, run, generation uint64) []resetReadyResult {
	results := make([]resetReadyResult, len(meetings))
	if len(meetings) == 0 {
		return results
	}
	jobs := make(chan int)
	var wg sync.WaitGroup
	workerCount := min(2, len(meetings))
	for range workerCount {
		wg.Go(func() {
			for index := range jobs {
				results[index] = c.resetReadyMeeting(ctx, room, meetings[index].ID, run, generation)
			}
		})
	}
	for index := range meetings {
		jobs <- index
	}
	close(jobs)
	wg.Wait()
	return results
}

func (c *controller) resetReadyMeeting(ctx context.Context, room *controllerRoom, meetingID string, run, generation uint64) resetReadyResult {
	result := resetReadyResult{MeetingID: meetingID}
	for attempt := 1; attempt <= resetReadyAttempts; attempt++ {
		result.Attempts = attempt
		meeting, err := c.getMeeting(ctx, room, meetingID)
		if err != nil {
			result.Outcome, result.Error = "failed", err.Error()
			return result
		}
		result.Status = meeting.Status
		switch meeting.Status {
		case "ready":
			result.Outcome = "already_ready"
			return result
		case "in_progress":
			result.Outcome = "skipped_in_progress"
			return result
		case "paused", "completed":
		default:
			result.Outcome = "failed"
			result.Error = fmt.Sprintf("unsupported meeting status %q", meeting.Status)
			return result
		}

		select {
		case c.resetSlots <- struct{}{}:
		case <-ctx.Done():
			result.Outcome, result.Error = "failed", ctx.Err().Error()
			return result
		}
		err = c.resetResume(ctx, room, meetingID, run, generation)
		<-c.resetSlots
		if err == nil {
			result.Status, result.Outcome = "ready", "reset"
			return result
		}
		result.Error = err.Error()
		// The next loop re-reads the meeting. A ready response means the previous
		// Resume succeeded even if its HTTP acknowledgement was lost; only a fresh
		// paused/completed observation permits another destructive attempt.
	}
	result.Outcome = "failed"
	return result
}

func (c *controller) resetResume(ctx context.Context, room *controllerRoom, meetingID string, run, generation uint64) error {
	_, err := runScheduled(ctx, c.scheduler, resetRequest, func(requestCtx context.Context) (struct{}, error) {
		c.mu.RLock()
		current := room.reconciliationRun == run && room.desired.Generation == generation && room.lifecycle == reconciliationSuspended && room.resetting
		c.mu.RUnlock()
		if !current {
			return struct{}{}, errReconciliationConflict
		}
		return struct{}{}, c.app.resumeRozetaMeeting(requestCtx, room.token, meetingID)
	})
	return err
}

func (c *controller) recordResetObservation(room *controllerRoom, active []roomMeetingView, empty bool) roomView {
	c.mu.Lock()
	room.activeMeetingIDs = meetingIDs(active)
	room.activeObservedAt = time.Now().UTC()
	room.activeSetStale = false
	room.activeSetConfirmedEmpty = empty
	room.revision++
	view := c.snapshotLocked(room)
	c.mu.Unlock()
	c.publish(view)
	return view
}

func (c *controller) cacheResetMeetings(room *controllerRoom, run, generation uint64, meetings []roomMeetingView) {
	c.mu.Lock()
	if room.reconciliationRun == run && room.desired.Generation == generation && room.lifecycle == reconciliationSuspended {
		room.meetings = append([]roomMeetingView{}, meetings...)
		room.desiredStatus = observedStatus(meetings, room.desired.MeetingID)
		room.updatedAt = time.Now().UTC()
		room.revision++
		view := c.snapshotLocked(room)
		c.mu.Unlock()
		c.publish(view)
		return
	}
	c.mu.Unlock()
}

func (c *controller) snapshotRoom(room *controllerRoom) roomView {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.snapshotLocked(room)
}
