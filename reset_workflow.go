package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	defaultResetJobMaxAge    = 8
	resetWorkflowWorkerCount = 4
	resetDateLayout          = "2006/1/2"
)

type resetSelection struct {
	date     *time.Time
	roomName string
}

type resetTarget struct {
	roomName  string
	meetingID string
	title     string
}

type resetJobKind string

const (
	resetObserveJob resetJobKind = "observe"
	resetStopJob    resetJobKind = "stop"
	resetMeetingJob resetJobKind = "reset"
)

type resetJob struct {
	kind   resetJobKind
	target resetTarget
	age    int
}

type resetJobResult struct {
	job      resetJob
	next     *resetJob
	terminal bool
	success  bool
	message  string
}

type resetTargetReport struct {
	target  resetTarget
	success bool
	message string
}

func parseResetSelection(value string) (resetSelection, error) {
	if strings.Count(value, ",") != 1 {
		return resetSelection{}, errors.New("-reset must contain exactly one comma")
	}
	parts := strings.SplitN(value, ",", 2)
	dateValue := strings.TrimSpace(parts[0])
	roomName := strings.TrimSpace(parts[1])
	if dateValue == "" || roomName == "" {
		return resetSelection{}, errors.New("-reset requires non-empty date and room values")
	}

	selection := resetSelection{roomName: roomName}
	if dateValue == "all" {
		return selection, nil
	}
	date, err := parseResetDate(dateValue)
	if err != nil {
		return resetSelection{}, err
	}
	selection.date = &date
	return selection, nil
}

func parseResetDate(value string) (time.Time, error) {
	parts := strings.Split(value, "/")
	if len(parts) != 3 || len(parts[0]) != 4 || len(parts[1]) < 1 || len(parts[1]) > 2 || len(parts[2]) < 1 || len(parts[2]) > 2 {
		return time.Time{}, fmt.Errorf("invalid reset date %q; expected YYYY/M/D", value)
	}
	year, yearErr := strconv.Atoi(parts[0])
	month, monthErr := strconv.Atoi(parts[1])
	day, dayErr := strconv.Atoi(parts[2])
	if yearErr != nil || monthErr != nil || dayErr != nil {
		return time.Time{}, fmt.Errorf("invalid reset date %q; expected YYYY/M/D", value)
	}
	location, err := time.LoadLocation("Asia/Taipei")
	if err != nil {
		return time.Time{}, fmt.Errorf("load Asia/Taipei timezone: %w", err)
	}
	date := time.Date(year, time.Month(month), day, 0, 0, 0, 0, location)
	if date.Year() != year || date.Month() != time.Month(month) || date.Day() != day {
		return time.Time{}, fmt.Errorf("invalid reset date %q", value)
	}
	return date, nil
}

func (c *controller) resetTargets(selection resetSelection) ([]resetTarget, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()

	if selection.roomName != "all" {
		if _, found := c.rooms[selection.roomName]; !found {
			return nil, fmt.Errorf("unknown room %q", selection.roomName)
		}
	}

	roomNames := make([]string, 0, len(c.rooms))
	for roomName := range c.rooms {
		if selection.roomName == "all" || selection.roomName == roomName {
			roomNames = append(roomNames, roomName)
		}
	}
	sort.Strings(roomNames)

	targets := make([]resetTarget, 0)
	for _, roomName := range roomNames {
		room := c.rooms[roomName]
		for _, meeting := range room.meetings {
			if meeting.Virtual || meeting.ID == preparationMeetingID || !resetDateMatches(selection.date, meeting.ScheduledStart) {
				continue
			}
			targets = append(targets, resetTarget{roomName: roomName, meetingID: meeting.ID, title: meeting.Title})
		}
	}
	return targets, nil
}

func resetDateMatches(want *time.Time, scheduledStart *time.Time) bool {
	if want == nil {
		return true
	}
	if scheduledStart == nil {
		return false
	}
	local := scheduledStart.In(want.Location())
	return local.Year() == want.Year() && local.Month() == want.Month() && local.Day() == want.Day()
}

func runResetWorkflow(ctx context.Context, c *controller, selectionValue string, maxAge int, output io.Writer) error {
	selection, err := parseResetSelection(selectionValue)
	if err != nil {
		return err
	}
	targets, err := c.resetTargets(selection)
	if err != nil {
		return err
	}
	if len(targets) == 0 {
		_, _ = fmt.Fprintln(output, "reset: no matching meetings")
		return nil
	}

	reports := runResetJobs(ctx, c, targets, maxAge)
	if err := ctx.Err(); err != nil {
		return err
	}
	failed := 0
	for _, report := range reports {
		status := "ok"
		if !report.success {
			status = "failed"
			failed++
		}
		_, _ = fmt.Fprintf(output, "reset: room=%s meeting=%s status=%s message=%s\n", report.target.roomName, report.target.meetingID, status, report.message)
	}
	if failed != 0 {
		return fmt.Errorf("reset completed with %d failed meeting(s)", failed)
	}
	return nil
}

func runResetJobs(ctx context.Context, c *controller, targets []resetTarget, maxAge int) []resetTargetReport {
	// The coordinator owns pending/running counts; workers only execute jobs and return
	// successors, which lets the queue grow safely without closing it while a worker adds work.
	jobs := make(chan resetJob)
	results := make(chan resetJobResult, resetWorkflowWorkerCount)
	var workers sync.WaitGroup
	for range resetWorkflowWorkerCount {
		workers.Go(func() {
			for {
				select {
				case <-ctx.Done():
					return
				case job, ok := <-jobs:
					if !ok {
						return
					}
					result := processResetJob(ctx, c, job)
					select {
					case results <- result:
					case <-ctx.Done():
						return
					}
				}
			}
		})
	}

	pending := make([]resetJob, 0, len(targets))
	for _, target := range targets {
		pending = append(pending, resetJob{kind: resetObserveJob, target: target})
	}
	reports := make(map[string]resetTargetReport, len(targets))
	running := 0
	for len(pending) != 0 || running != 0 {
		var send chan resetJob
		var next resetJob
		if len(pending) != 0 && running < resetWorkflowWorkerCount {
			send = jobs
			next = pending[0]
		}

		select {
		case send <- next:
			pending = pending[1:]
			running++
		case result := <-results:
			running--
			if result.terminal {
				reports[resetTargetKey(result.job.target)] = resetTargetReport{
					target:  result.job.target,
					success: result.success,
					message: result.message,
				}
			}
			if result.next != nil {
				nextAge := result.next.age
				if nextAge > maxAge {
					reports[resetTargetKey(result.next.target)] = resetTargetReport{
						target:  result.next.target,
						success: false,
						message: fmt.Sprintf("job age %d exceeded maximum %d after %s", nextAge, maxAge, result.message),
					}
				} else {
					pending = append(pending, *result.next)
				}
			}
		case <-ctx.Done():
			close(jobs)
			workers.Wait()
			return sortedResetReports(reports)
		}
	}
	close(jobs)
	workers.Wait()
	return sortedResetReports(reports)
}

func processResetJob(ctx context.Context, c *controller, job resetJob) resetJobResult {
	switch job.kind {
	case resetObserveJob:
		return processResetObserve(ctx, c, job)
	case resetStopJob:
		return processResetStop(ctx, c, job)
	case resetMeetingJob:
		return processResetMeeting(ctx, c, job)
	default:
		return resetJobResult{job: job, terminal: true, message: fmt.Sprintf("unknown reset job kind %q", job.kind)}
	}
}

func processResetObserve(ctx context.Context, c *controller, job resetJob) resetJobResult {
	room, err := c.controllerRoomByName(job.target.roomName)
	if err != nil {
		return failedResetJob(job, err)
	}
	active, err := c.listActiveMeetings(ctx, room)
	if err != nil {
		return retryOrFailResetJob(job, err)
	}
	c.recordResetObservation(room, active, len(active) == 0)
	meeting, err := c.getMeeting(ctx, room, job.target.meetingID)
	if err != nil {
		return retryOrFailResetJob(job, err)
	}

	lifecycle := c.currentRoomLifecycle(room)
	switch meeting.Status {
	case "ready":
		return resetJobResult{job: job, terminal: true, success: true, message: "already_ready"}
	case "paused", "completed":
		if lifecycle == reconciliationStopping {
			return retryResetJob(job, errors.New("room is stopping"))
		}
		if len(active) != 0 || lifecycle == reconciliationActive || lifecycle == reconciliationStarting {
			if lifecycle == reconciliationSuspended {
				return failedResetJob(job, errors.New("room is suspended while remote active set is not empty"))
			}
			return nextResetJob(job, resetStopJob, "room must be stopped before reset")
		}
		return nextResetJob(job, resetMeetingJob, "room is stopped")
	case "in_progress":
		if lifecycle == reconciliationActive || lifecycle == reconciliationStarting {
			return nextResetJob(job, resetStopJob, "selected meeting is active")
		}
		return retryResetJob(job, errors.New("meeting detail is active but active set is not"))
	default:
		return failedResetJob(job, fmt.Errorf("unsupported meeting status %q", meeting.Status))
	}
}

func processResetStop(ctx context.Context, c *controller, job resetJob) resetJobResult {
	room, err := c.controllerRoomByName(job.target.roomName)
	if err != nil {
		return failedResetJob(job, err)
	}
	lifecycle := c.currentRoomLifecycle(room)
	if lifecycle == reconciliationStopping {
		return nextResetJob(job, resetObserveJob, "stop already accepted")
	}
	if lifecycle != reconciliationActive && lifecycle != reconciliationStarting {
		return retryResetJob(job, fmt.Errorf("stop is not applicable while lifecycle is %s", lifecycle))
	}

	active, err := c.listActiveMeetings(ctx, room)
	if err != nil {
		return retryOrFailResetJob(job, err)
	}
	activeIDs := meetingIDs(active)
	target := reconciliationTarget{
		RoomName:                  room.name,
		ExpectedReconciliationRun: c.roomRun(room),
		ExpectedGeneration:        c.roomGeneration(room),
		Preflight:                 &preflightFacts{ActiveMeetingIDs: &activeIDs},
	}
	_, _, err = c.confirmedLifecycle(ctx, c.app.epoch, "stop", []reconciliationTarget{target}, true)
	if err != nil {
		if c.currentRoomLifecycle(room) == reconciliationStopping {
			return nextResetJob(job, resetObserveJob, "stop accepted concurrently")
		}
		return retryOrFailResetJob(job, err)
	}
	return nextResetJob(job, resetObserveJob, "stop accepted")
}

func processResetMeeting(ctx context.Context, c *controller, job resetJob) resetJobResult {
	result, err := c.resetSelectedMeeting(ctx, job.target.roomName, job.target.meetingID)
	if err == nil {
		return resetJobResult{job: job, terminal: true, success: true, message: result.Outcome}
	}
	if errors.Is(err, errResetActive) || errors.Is(err, errResetNotStopped) || errors.Is(err, errReconciliationConflict) {
		return retryResetJob(job, err)
	}
	return retryOrFailResetJob(job, err)
}

func nextResetJob(job resetJob, kind resetJobKind, message string) resetJobResult {
	next := job
	next.kind = kind
	next.age++
	return resetJobResult{job: job, next: &next, message: message}
}

func retryResetJob(job resetJob, err error) resetJobResult {
	return nextResetJob(job, resetObserveJob, err.Error())
}

func retryOrFailResetJob(job resetJob, err error) resetJobResult {
	if retryableResetWorkflowError(err) {
		return retryResetJob(job, err)
	}
	return failedResetJob(job, err)
}

func failedResetJob(job resetJob, err error) resetJobResult {
	return resetJobResult{job: job, terminal: true, message: err.Error()}
}

func retryableResetWorkflowError(err error) bool {
	if errors.Is(err, errReconciliationConflict) || errors.Is(err, errPreflightChanged) || errors.Is(err, errRequestQueueFull) {
		return true
	}
	return retryablePreflightError(err)
}

func (c *controller) controllerRoomByName(roomName string) (*controllerRoom, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	room, found := c.rooms[roomName]
	if !found {
		return nil, errUnknownRoom
	}
	return room, nil
}

func (c *controller) currentRoomLifecycle(room *controllerRoom) reconciliationState {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return room.lifecycle
}

func (c *controller) roomRun(room *controllerRoom) uint64 {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return room.reconciliationRun
}

func (c *controller) roomGeneration(room *controllerRoom) uint64 {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return room.desired.Generation
}

func resetTargetKey(target resetTarget) string {
	return target.roomName + "\x00" + target.meetingID
}

func sortedResetReports(reports map[string]resetTargetReport) []resetTargetReport {
	result := make([]resetTargetReport, 0, len(reports))
	for _, report := range reports {
		result = append(result, report)
	}
	sort.Slice(result, func(left, right int) bool {
		if result[left].target.roomName != result[right].target.roomName {
			return result[left].target.roomName < result[right].target.roomName
		}
		return result[left].target.meetingID < result[right].target.meetingID
	})
	return result
}
