package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	controllerStateVersion  = 2
	reconcileInterval       = 5 * time.Second
	reconcileRequestTimeout = 12 * time.Second
	stopDrainTimeout        = 30 * time.Second
)

type reconciliationState string

const (
	reconciliationSuspended reconciliationState = "suspended"
	reconciliationStarting  reconciliationState = "starting"
	reconciliationActive    reconciliationState = "active"
	reconciliationStopping  reconciliationState = "stopping"
)

type consumedResume struct {
	Generation         uint64    `json:"generation"`
	CompletedUpdatedAt time.Time `json:"completed_updated_at"`
}

type desiredState struct {
	MeetingID      string          `json:"meeting_id"`
	Generation     uint64          `json:"generation"`
	ConsumedResume *consumedResume `json:"consumed_resume,omitempty"`
}

type desiredStateFile struct {
	Version int                     `json:"version"`
	Rooms   map[string]desiredState `json:"rooms"`
}

type desiredStateUpdate struct {
	MeetingID                string
	ExpectedEpoch            string
	ExpectedRun              uint64
	ExpectedGeneration       uint64
	ConfirmDestructiveResume bool
	Rearm                    bool
}

type reconcileCondition struct {
	Type    string `json:"type"`
	Status  string `json:"status"`
	Reason  string `json:"reason,omitempty"`
	Message string `json:"message,omitempty"`
}

type recentAction struct {
	Action     string    `json:"action"`
	MeetingID  string    `json:"meeting_id,omitempty"`
	Succeeded  bool      `json:"succeeded"`
	Dispatched time.Time `json:"dispatched_at"`
	Error      string    `json:"error,omitempty"`
}

type controllerRoom struct {
	name  string
	token string

	// dispatchGate linearizes lifecycle/spec mutations with command reservation.
	// It is never held across HTTP I/O or while waiting for the global scheduler.
	dispatchGate sync.Mutex

	desired           desiredState
	resumeAuthorized  uint64
	meetings          []roomMeetingView
	activeMeetingIDs  []string
	desiredStatus     string
	activeObservedAt  time.Time
	activeSetStale    bool
	lifecycle         reconciliationState
	reconciliationRun uint64
	revision          uint64
	updatedAt         time.Time
	summary           string
	summaryReason     string
	conditions        []reconcileCondition
	gotoCondition     reconcileCondition
	recentActions     []recentAction
	lastError         string
	advanceAlert      *reconcileCondition
	stopDeadline      time.Time

	runCtx    context.Context
	runCancel context.CancelFunc
	wake      chan struct{}
}

type requestPriority uint8

const (
	observationRequest requestPriority = iota
	controlRequest
)

type requestJob struct {
	ctx context.Context
	run func(context.Context)
}

// WHY: remote calls need a global cap without allowing pagination to starve controls.
// Previously observations and controls competed for one semaphore; now two of six
// workers are control-only and the mixed workers always prefer queued controls.
type requestScheduler struct {
	ctx     context.Context
	cancel  context.CancelFunc
	control chan requestJob
	observe chan requestJob
	wg      sync.WaitGroup
}

func newRequestScheduler(parent context.Context) *requestScheduler {
	ctx, cancel := context.WithCancel(parent)
	scheduler := &requestScheduler{
		ctx: ctx, cancel: cancel,
		control: make(chan requestJob), observe: make(chan requestJob),
	}
	for range 2 {
		scheduler.wg.Go(scheduler.runControl)
	}
	for range roomSyncConcurrency - 2 {
		scheduler.wg.Go(scheduler.runMixed)
	}
	return scheduler
}

func (s *requestScheduler) close() {
	s.cancel()
	s.wg.Wait()
}

func (s *requestScheduler) runControl() {
	for {
		select {
		case <-s.ctx.Done():
			return
		case job := <-s.control:
			job.run(job.ctx)
		}
	}
}

func (s *requestScheduler) runMixed() {
	for {
		select {
		case <-s.ctx.Done():
			return
		case job := <-s.control:
			job.run(job.ctx)
			continue
		default:
		}
		select {
		case <-s.ctx.Done():
			return
		case job := <-s.control:
			job.run(job.ctx)
		case job := <-s.observe:
			job.run(job.ctx)
		}
	}
}

func runScheduled[T any](ctx context.Context, scheduler *requestScheduler, priority requestPriority, fn func(context.Context) (T, error)) (T, error) {
	var zero T
	result := make(chan struct {
		value T
		err   error
	}, 1)
	job := requestJob{ctx: ctx, run: func(requestCtx context.Context) {
		value, err := fn(requestCtx)
		select {
		case result <- struct {
			value T
			err   error
		}{value: value, err: err}:
		case <-requestCtx.Done():
		}
	}}
	queue := scheduler.observe
	if priority == controlRequest {
		queue = scheduler.control
	}
	select {
	case <-ctx.Done():
		return zero, ctx.Err()
	case <-scheduler.ctx.Done():
		return zero, scheduler.ctx.Err()
	case queue <- job:
	}
	select {
	case <-ctx.Done():
		return zero, ctx.Err()
	case result := <-result:
		return result.value, result.err
	}
}

type controller struct {
	app            *app
	statePath      string
	schedule       meetingSchedule
	ctx            context.Context
	cancel         context.CancelFunc
	scheduler      *requestScheduler
	stopTimeout    time.Duration
	controlTimeout time.Duration

	mu      sync.RWMutex
	storeMu sync.Mutex
	rooms   map[string]*controllerRoom
	file    desiredStateFile
	wg      sync.WaitGroup
	fatal   func(error)
}

type committedStateError struct{ err error }

func (e *committedStateError) Error() string { return e.err.Error() }
func (e *committedStateError) Unwrap() error { return e.err }

func newController(parent context.Context, app *app, tokens map[string]string, schedule meetingSchedule, statePath string) (*controller, error) {
	file, err := loadDesiredStateFile(statePath)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(parent)
	controller := &controller{
		app: app, statePath: statePath, schedule: schedule, ctx: ctx, cancel: cancel,
		rooms: make(map[string]*controllerRoom, len(tokens)), file: file,
		stopTimeout: stopDrainTimeout, controlTimeout: reconcileRequestTimeout,
	}
	controller.fatal = func(err error) { log.Fatalf("controller state durability failure: %v", err) }
	controller.scheduler = newRequestScheduler(ctx)
	for name, token := range tokens {
		desired := file.Rooms[name]
		summaryReason := "ActiveSetUnknown"
		if desired.MeetingID == "" {
			summaryReason = "InitialMeetingRequired"
		}
		controller.rooms[name] = &controllerRoom{
			name: name, token: token, desired: desired,
			meetings: []roomMeetingView{}, activeMeetingIDs: []string{},
			recentActions: []recentAction{},
			lifecycle:     reconciliationSuspended, activeSetStale: true,
			summary: "Suspended", summaryReason: summaryReason,
			conditions: suspendedConditions(), updatedAt: time.Now().UTC(), wake: make(chan struct{}, 1),
		}
	}
	return controller, nil
}

// WHY: v1 included the removed running bit but its meeting/generation remain authoritative.
// Previously any non-current version aborted startup; now v1 is decoded, validated, and
// atomically replaced by v2 without starting actors or issuing remote requests.
func loadDesiredStateFile(path string) (desiredStateFile, error) {
	empty := desiredStateFile{Version: controllerStateVersion, Rooms: map[string]desiredState{}}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return empty, nil
	}
	if err != nil {
		return desiredStateFile{}, fmt.Errorf("read controller state: %w", err)
	}
	var header struct {
		Version int `json:"version"`
	}
	if err := json.Unmarshal(data, &header); err != nil {
		return desiredStateFile{}, fmt.Errorf("decode controller state: %w", err)
	}
	var file desiredStateFile
	switch header.Version {
	case controllerStateVersion:
		if err := json.Unmarshal(data, &file); err != nil {
			return desiredStateFile{}, fmt.Errorf("decode controller state: %w", err)
		}
	case 1:
		var legacy struct {
			Version int `json:"version"`
			Rooms   map[string]struct {
				MeetingID  string `json:"meeting_id"`
				Generation uint64 `json:"generation"`
			} `json:"rooms"`
		}
		if err := json.Unmarshal(data, &legacy); err != nil {
			return desiredStateFile{}, fmt.Errorf("decode legacy controller state: %w", err)
		}
		file = empty
		for room, desired := range legacy.Rooms {
			file.Rooms[room] = desiredState{MeetingID: desired.MeetingID, Generation: desired.Generation}
		}
	default:
		return desiredStateFile{}, errors.New("controller state has an unsupported version")
	}
	if err := validateDesiredStateFile(file); err != nil {
		return desiredStateFile{}, err
	}
	if header.Version == 1 {
		if err := writeDesiredState(path, file); err != nil {
			return desiredStateFile{}, fmt.Errorf("migrate controller state to version 2: %w", err)
		}
	}
	return file, nil
}

func validateDesiredStateFile(file desiredStateFile) error {
	if file.Version != controllerStateVersion || file.Rooms == nil {
		return errors.New("controller state has an invalid version or rooms map")
	}
	for room, desired := range file.Rooms {
		if strings.TrimSpace(room) == "" || strings.TrimSpace(desired.MeetingID) == "" || desired.Generation == 0 {
			return errors.New("controller state contains an invalid desired room")
		}
		if desired.ConsumedResume != nil {
			if desired.ConsumedResume.Generation != desired.Generation || desired.ConsumedResume.CompletedUpdatedAt.IsZero() {
				return errors.New("controller state contains an invalid consumed resume marker")
			}
		}
	}
	return nil
}

func writeDesiredState(path string, file desiredStateFile) error {
	data, err := json.MarshalIndent(file, "", "\t")
	if err != nil {
		return err
	}
	directory := filepath.Dir(path)
	if err := ensureStateDirectory(directory); err != nil {
		return fmt.Errorf("create controller state directory: %w", err)
	}
	temporary, err := os.CreateTemp(directory, ".controller-state-*")
	if err != nil {
		return fmt.Errorf("create controller state temp file: %w", err)
	}
	temporaryName := temporary.Name()
	defer os.Remove(temporaryName)
	if _, err := temporary.Write(data); err != nil {
		temporary.Close()
		return fmt.Errorf("write controller state: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		temporary.Close()
		return fmt.Errorf("sync controller state: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("close controller state: %w", err)
	}
	if err := os.Rename(temporaryName, path); err != nil {
		return fmt.Errorf("replace controller state: %w", err)
	}
	directoryHandle, err := os.Open(directory)
	if err != nil {
		return &committedStateError{err: fmt.Errorf("open committed controller state directory: %w", err)}
	}
	defer directoryHandle.Close()
	if err := directoryHandle.Sync(); err != nil {
		return &committedStateError{err: fmt.Errorf("sync committed controller state directory: %w", err)}
	}
	return nil
}

func ensureStateDirectory(directory string) error {
	missing := make([]string, 0)
	for current := directory; ; current = filepath.Dir(current) {
		_, err := os.Stat(current)
		if err == nil {
			break
		}
		if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		missing = append(missing, current)
		parent := filepath.Dir(current)
		if parent == current {
			break
		}
	}
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return err
	}
	for index := len(missing) - 1; index >= 0; index-- {
		parent, err := os.Open(filepath.Dir(missing[index]))
		if err != nil {
			return err
		}
		syncErr := parent.Sync()
		closeErr := parent.Close()
		if syncErr != nil {
			return syncErr
		}
		if closeErr != nil {
			return closeErr
		}
	}
	return nil
}

func (c *controller) close() {
	c.cancel()
	c.scheduler.close()
	c.wg.Wait()
}

type reconciliationTarget struct {
	RoomName                  string          `json:"room_name"`
	ExpectedReconciliationRun uint64          `json:"expected_reconciliation_run"`
	ExpectedGeneration        uint64          `json:"expected_generation"`
	Preflight                 *preflightFacts `json:"preflight,omitempty"`
}

type preflightFacts struct {
	DestructiveResume *bool     `json:"destructive_resume,omitempty"`
	ActiveMeetingIDs  *[]string `json:"active_meeting_ids,omitempty"`
}

type reconciliationResult struct {
	RoomName string `json:"room_name"`
	Applied  bool   `json:"applied"`
	State    string `json:"lifecycle"`
	Error    string `json:"error,omitempty"`
}

type preflightResult struct {
	RoomName          string   `json:"room_name"`
	Observable        bool     `json:"observable"`
	DesiredMeetingID  string   `json:"desired_meeting_id,omitempty"`
	DesiredStatus     string   `json:"desired_status,omitempty"`
	ActiveMeetingIDs  []string `json:"active_meeting_ids"`
	DestructiveResume bool     `json:"destructive_resume"`
	Error             string   `json:"error,omitempty"`
}

var (
	errReconciliationConflict  = errors.New("reconciliation state conflict")
	errInvalidReconciliation   = errors.New("invalid reconciliation request")
	errConfirmationRequired    = errors.New("explicit confirmation is required")
	errGenerationConflict      = errors.New("desired state generation conflict")
	errUnknownRoom             = errors.New("unknown room")
	errMeetingIDRequired       = errors.New("meeting_id is required")
	errGenerationExhausted     = errors.New("desired state generation exhausted")
	errReconciliationNotActive = errors.New("room reconciliation is not active")
	errDestructiveConfirmation = errors.New("completed desired meeting requires destructive resume confirmation")
	errPreflightChanged        = errors.New("preflight facts changed; confirmation must be repeated")
	errRoomStopping            = errors.New("room is stopping")
	errStaleControllerState    = errors.New("controller state is stale")
	errCurrentMeetingUnset     = errors.New("current desired meeting is unset")
)

type advancePreflight struct {
	meetings []roomMeetingView
	active   []roomMeetingView
	desired  roomMeetingView
}

type advanceResult struct {
	Room       roomView
	MeetingID  string
	Generation uint64
}

func (c *controller) advanceAndStart(ctx context.Context, roomName string) (advanceResult, error) {
	// WHY: this operation combines remote preflight, persisted desired state, and lifecycle start.
	// Previously callers had to perform those steps independently, so a concurrent update could
	// select the wrong next meeting or start a run from stale remote evidence. The complete
	// operation now fences its preflight and commit with the room run and desired generation.
	c.mu.RLock()
	room, found := c.rooms[roomName]
	if !found {
		c.mu.RUnlock()
		return advanceResult{}, errUnknownRoom
	}
	run, generation, currentID := room.reconciliationRun, room.desired.Generation, room.desired.MeetingID
	lifecycle := room.lifecycle
	c.mu.RUnlock()
	if currentID == "" {
		return advanceResult{}, errCurrentMeetingUnset
	}
	if lifecycle == reconciliationStopping {
		return advanceResult{}, errRoomStopping
	}

	preflight, next, err := c.advancePreflightWithRetry(ctx, room, run, generation, currentID)
	if err != nil {
		return advanceResult{}, err
	}

	room.dispatchGate.Lock()
	defer room.dispatchGate.Unlock()
	c.storeMu.Lock()
	defer c.storeMu.Unlock()
	c.mu.Lock()
	if room.reconciliationRun != run || room.desired.Generation != generation ||
		room.desired.MeetingID != currentID || room.lifecycle == reconciliationStopping {
		c.mu.Unlock()
		return advanceResult{}, errStaleControllerState
	}
	if generation == ^uint64(0) {
		c.mu.Unlock()
		return advanceResult{}, errGenerationExhausted
	}
	nextDesired := desiredState{MeetingID: next.ID, Generation: generation + 1}
	candidate := cloneStateFile(c.file)
	candidate.Rooms[roomName] = nextDesired
	c.mu.Unlock()
	if err := writeDesiredState(c.statePath, candidate); err != nil {
		var committed *committedStateError
		if !errors.As(err, &committed) {
			return advanceResult{}, err
		}
		c.fatal(err)
	}

	c.mu.Lock()
	if room.reconciliationRun != run || room.desired.Generation != generation || room.desired.MeetingID != currentID || room.lifecycle == reconciliationStopping {
		c.mu.Unlock()
		return advanceResult{}, errStaleControllerState
	}
	room.desired = nextDesired
	room.resumeAuthorized = 0
	if next.Status == "completed" {
		room.resumeAuthorized = nextDesired.Generation
	}
	room.meetings = append([]roomMeetingView{}, preflight.meetings...)
	room.activeMeetingIDs = meetingIDs(preflight.active)
	room.activeSetStale = false
	room.desiredStatus = next.Status
	room.advanceAlert = nil
	room.lastError = ""
	room.updatedAt = time.Now().UTC()
	room.revision++
	c.file = candidate
	var runCtx context.Context
	if room.lifecycle == reconciliationSuspended {
		if room.reconciliationRun == ^uint64(0) {
			c.mu.Unlock()
			return advanceResult{}, errGenerationExhausted
		}
		room.reconciliationRun++
		room.lifecycle = reconciliationStarting
		room.stopDeadline = time.Time{}
		room.activeSetStale = true
		room.summary, room.summaryReason = "Reconciling", "StartingDesiredMeeting"
		room.conditions = activeConditions(true, false)
		room.runCtx, room.runCancel = context.WithCancel(c.ctx)
		run = room.reconciliationRun
		runCtx = room.runCtx
		c.wg.Go(func() { c.runRoom(room, run, runCtx) })
	} else {
		room.summary, room.summaryReason = summaryForLifecycle(room.lifecycle, next.ID)
		room.conditions = activeConditions(room.activeSetStale, false)
	}
	view := c.snapshotLocked(room)
	result := advanceResult{Room: view, MeetingID: next.ID, Generation: nextDesired.Generation}
	active := room.lifecycle != reconciliationSuspended
	c.mu.Unlock()
	if active {
		c.notify(room)
	}
	c.publish(view)
	return result, nil
}

func (c *controller) advancePreflightWithRetry(ctx context.Context, room *controllerRoom, run, generation uint64, currentID string) (advancePreflight, roomMeetingView, error) {
	var lastErr error
	for attempt := 1; attempt <= 3; attempt++ {
		preflight, next, err := c.advancePreflight(ctx, room, run, generation, currentID)
		if err == nil {
			return preflight, next, nil
		}
		lastErr = err
		if !retryablePreflightError(err) || attempt == 3 {
			c.setAdvanceAlert(room, run, generation, "AdvancePreflightFailed", err.Error())
			return advancePreflight{}, roomMeetingView{}, err
		}
		c.setAdvanceAlert(room, run, generation, "Retrying", fmt.Sprintf("preflight attempt %d/3 failed: %v", attempt, err))
		timer := time.NewTimer(time.Duration(1<<(attempt-1)) * time.Second)
		select {
		case <-ctx.Done():
			timer.Stop()
			return advancePreflight{}, roomMeetingView{}, ctx.Err()
		case <-timer.C:
		}
	}
	return advancePreflight{}, roomMeetingView{}, lastErr
}

func (c *controller) advancePreflight(ctx context.Context, room *controllerRoom, run, generation uint64, currentID string) (advancePreflight, roomMeetingView, error) {
	c.mu.RLock()
	if room.reconciliationRun != run || room.desired.Generation != generation || room.desired.MeetingID != currentID || room.lifecycle == reconciliationStopping {
		c.mu.RUnlock()
		return advancePreflight{}, roomMeetingView{}, errStaleControllerState
	}
	c.mu.RUnlock()
	meetings, err := c.listMeetings(ctx, room)
	if err != nil {
		return advancePreflight{}, roomMeetingView{}, err
	}
	next, err := c.schedule.nextMeeting(meetings, currentID)
	if err != nil {
		return advancePreflight{}, roomMeetingView{}, err
	}
	active, err := c.listActiveMeetings(ctx, room)
	if err != nil {
		return advancePreflight{}, roomMeetingView{}, err
	}
	desired, err := c.getMeeting(ctx, room, next.ID)
	if err != nil {
		return advancePreflight{}, roomMeetingView{}, err
	}
	return advancePreflight{meetings: meetings, active: active, desired: desired}, desired, nil
}

func retryablePreflightError(err error) bool {
	if errors.Is(err, context.Canceled) {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var apiErr *rozetaAPIError
	if errors.As(err, &apiErr) {
		return apiErr.StatusCode >= http.StatusInternalServerError
	}
	var networkErr net.Error
	return errors.As(err, &networkErr)
}

func (c *controller) setAdvanceAlert(room *controllerRoom, run, generation uint64, reason, message string) {
	c.mu.Lock()
	if room.reconciliationRun == run && room.desired.Generation == generation && room.lifecycle != reconciliationStopping {
		room.advanceAlert = &reconcileCondition{Type: "AdvanceAndStartReady", Status: "False", Reason: reason, Message: message}
		room.lastError = message
		room.revision++
		view := c.snapshotLocked(room)
		c.mu.Unlock()
		c.publish(view)
		return
	}
	c.mu.Unlock()
}

func (c *controller) validateTargetsLocked(epoch string, targets []reconciliationTarget) ([]*controllerRoom, error) {
	if epoch != c.app.epoch || len(targets) == 0 {
		return nil, errReconciliationConflict
	}
	seen := make(map[string]struct{}, len(targets))
	rooms := make([]*controllerRoom, len(targets))
	for index, target := range targets {
		room, found := c.rooms[target.RoomName]
		_, duplicate := seen[target.RoomName]
		if !found || duplicate || strings.TrimSpace(target.RoomName) != target.RoomName ||
			room.reconciliationRun != target.ExpectedReconciliationRun || room.desired.Generation != target.ExpectedGeneration {
			return nil, errReconciliationConflict
		}
		seen[target.RoomName] = struct{}{}
		rooms[index] = room
	}
	return rooms, nil
}

// WHY: confirmation must show current remote risk for exactly the browser-frozen targets.
// Previously lifecycle calls transitioned immediately; preflight now validates all local
// fences atomically, then reports each remote observation independently so bulk Start can
// proceed for observable rooms while failed rooms remain suspended.
func (c *controller) lifecyclePreflight(ctx context.Context, epoch, action string, targets []reconciliationTarget) ([]roomView, []preflightResult, error) {
	if action != "start" && action != "stop" {
		return c.snapshotRooms(), nil, errInvalidReconciliation
	}
	c.mu.RLock()
	rooms, err := c.validateTargetsLocked(epoch, targets)
	views := c.snapshotRoomsLocked()
	inputs := make([]controllerRoom, len(rooms))
	for index, room := range rooms {
		// Preflight performs remote I/O after releasing the controller lock. Copying
		// the immutable request inputs prevents a concurrent desired update from
		// racing the confirmation payload that the administrator is reviewing.
		inputs[index] = controllerRoom{
			name: room.name, token: room.token, desired: room.desired, lifecycle: room.lifecycle,
		}
	}
	c.mu.RUnlock()
	if err != nil {
		return views, nil, err
	}
	results := make([]preflightResult, len(inputs))
	for index := range inputs {
		room := &inputs[index]
		result := preflightResult{RoomName: room.name, DesiredMeetingID: room.desired.MeetingID, ActiveMeetingIDs: []string{}}
		if action == "start" && room.lifecycle != reconciliationSuspended {
			result.Error = fmt.Sprintf("start is not applicable while lifecycle is %s", room.lifecycle)
			results[index] = result
			continue
		}
		if action == "stop" && room.lifecycle != reconciliationStarting && room.lifecycle != reconciliationActive {
			result.Error = fmt.Sprintf("stop is not applicable while lifecycle is %s", room.lifecycle)
			results[index] = result
			continue
		}
		if action == "start" && room.desired.MeetingID == "" {
			result.Error = "a desired meeting must be selected before Start"
			results[index] = result
			continue
		}
		active, activeErr := c.listActiveMeetings(ctx, room)
		if activeErr != nil {
			result.Error = activeErr.Error()
			results[index] = result
			continue
		}
		result.ActiveMeetingIDs = meetingIDs(active)
		result.Observable = true
		if action == "start" && room.desired.MeetingID != "" {
			desired, desiredErr := c.getMeeting(ctx, room, room.desired.MeetingID)
			if desiredErr != nil {
				result.Observable = false
				result.Error = desiredErr.Error()
			} else {
				result.DesiredStatus = desired.Status
				result.DestructiveResume = desired.Status == "completed"
			}
		}
		results[index] = result
	}
	return views, results, nil
}

func (c *controller) reconcileLifecycle(epoch, action string, targets []reconciliationTarget, confirmed bool) ([]roomView, []reconciliationResult, error) {
	if !confirmed {
		return c.snapshotRooms(), nil, errConfirmationRequired
	}
	c.mu.RLock()
	gateRooms, err := c.validateTargetsLocked(epoch, targets)
	c.mu.RUnlock()
	if err != nil {
		return c.snapshotRooms(), nil, err
	}
	unlockGates := lockRoomDispatchGates(gateRooms)
	defer unlockGates()
	// Desired-state persistence drops c.mu during fsync. Per-room gates and storeMu
	// order Stop either before a spec write/command reservation or after it; no
	// global controller lock is held while remote I/O runs.
	c.storeMu.Lock()
	defer c.storeMu.Unlock()
	c.mu.Lock()
	rooms, err := c.validateTargetsLocked(epoch, targets)
	if err != nil {
		views := c.snapshotRoomsLocked()
		c.mu.Unlock()
		return views, nil, err
	}
	results := make([]reconciliationResult, 0, len(rooms))
	changed := make([]roomView, 0, len(rooms))
	for _, room := range rooms {
		applied := c.applyLifecycleLocked(room, action)
		result := reconciliationResult{RoomName: room.name, Applied: applied, State: string(room.lifecycle)}
		if !applied {
			result.Error = fmt.Sprintf("%s is not applicable while lifecycle is %s", action, room.lifecycle)
		} else {
			changed = append(changed, c.snapshotLocked(room))
		}
		results = append(results, result)
	}
	views := c.snapshotRoomsLocked()
	c.mu.Unlock()
	for _, view := range changed {
		c.publish(view)
	}
	return views, results, nil
}

func (c *controller) confirmedLifecycle(ctx context.Context, epoch, action string, targets []reconciliationTarget, confirmed bool) ([]roomView, []reconciliationResult, error) {
	if !confirmed {
		return c.snapshotRooms(), nil, errConfirmationRequired
	}
	if action == "force-stop" {
		// Force-stop has no safe remote preflight: its purpose is to abandon an
		// unconfirmable stopping run. It still requires confirmation and all fences.
		return c.reconcileLifecycle(epoch, action, targets, true)
	}
	views, preflight, err := c.lifecyclePreflight(ctx, epoch, action, targets)
	if err != nil {
		return views, nil, err
	}
	eligible := make([]reconciliationTarget, 0, len(targets))
	results := make([]reconciliationResult, 0, len(targets))
	for index, result := range preflight {
		if result.Observable {
			facts := targets[index].Preflight
			// Confirmed requests previously performed a new preflight but ignored what
			// the administrator had actually seen. Compare the risk-bearing facts so a
			// completed transition or changed Stop set requires a new confirmation.
			if facts == nil || action == "start" && (facts.DestructiveResume == nil || *facts.DestructiveResume != result.DestructiveResume) ||
				action == "stop" && (facts.ActiveMeetingIDs == nil || !slicesEqualSorted(*facts.ActiveMeetingIDs, result.ActiveMeetingIDs)) {
				return views, nil, errPreflightChanged
			}
			eligible = append(eligible, targets[index])
			continue
		}
		results = append(results, reconciliationResult{
			RoomName: result.RoomName, State: string(c.roomLifecycleByName(result.RoomName)), Error: result.Error,
		})
	}
	if len(eligible) == 0 {
		return c.snapshotRooms(), results, nil
	}
	views, applied, err := c.reconcileLifecycle(epoch, action, eligible, true)
	results = append(results, applied...)
	return views, results, err
}

func (c *controller) roomLifecycleByName(roomName string) reconciliationState {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if room, found := c.rooms[roomName]; found {
		return room.lifecycle
	}
	return reconciliationSuspended
}

func (c *controller) applyLifecycleLocked(room *controllerRoom, action string) bool {
	switch action {
	case "start":
		if room.lifecycle != reconciliationSuspended || room.reconciliationRun == ^uint64(0) {
			return false
		}
		room.reconciliationRun++
		room.lifecycle = reconciliationStarting
		room.resumeAuthorized = room.desired.Generation
		room.stopDeadline = time.Time{}
		room.activeSetStale = true
		room.summary, room.summaryReason = "Reconciling", "StartingDesiredMeeting"
		room.conditions = activeConditions(true, false)
		room.runCtx, room.runCancel = context.WithCancel(c.ctx)
		room.revision++
		run, runCtx := room.reconciliationRun, room.runCtx
		c.wg.Go(func() { c.runRoom(room, run, runCtx) })
		return true
	case "stop":
		if room.lifecycle != reconciliationStarting && room.lifecycle != reconciliationActive {
			return false
		}
		room.lifecycle = reconciliationStopping
		room.resumeAuthorized = 0
		// The previous timer began only when the actor next noticed stopping, so an
		// in-flight request could extend force-stop beyond 30 seconds. Anchor it to
		// the instant Stop is accepted instead.
		room.stopDeadline = time.Now().Add(c.stopTimeout)
		room.summary, room.summaryReason = "Reconciling", "PausingAllMeetings"
		room.revision++
		run, deadline := room.reconciliationRun, room.stopDeadline
		c.wg.Go(func() { c.runForceStopWatchdog(room, run, deadline) })
		c.notify(room)
		return true
	case "force-stop":
		if room.lifecycle != reconciliationStopping {
			return false
		}
		c.forceStopLocked(room)
		return true
	default:
		return false
	}
}

func (c *controller) runRoom(room *controllerRoom, run uint64, runCtx context.Context) {
	c.mu.Lock()
	// WHY: desired updates are allowed while starting and must be handled by this
	// actor. Previously the initial generation was treated as an actor fence, so
	// an update before this lock was acquired made the actor exit and strand the
	// room in starting. The reconciliation run remains the actor fence; each
	// reconcile round separately reads and validates the current generation.
	if room.reconciliationRun != run ||
		(room.lifecycle != reconciliationStarting && room.lifecycle != reconciliationStopping) {
		c.mu.Unlock()
		return
	}
	// Stop may win immediately after Start publishes "starting". The actor must
	// still start and own the stopping loop instead of exiting and stranding it.
	if room.lifecycle == reconciliationStarting {
		room.lifecycle = reconciliationActive
		room.revision++
	}
	view := c.snapshotLocked(room)
	c.mu.Unlock()
	c.publish(view)

	ticker := time.NewTicker(reconcileInterval)
	defer ticker.Stop()
	for {
		c.reconcileRound(room, run)
		c.mu.RLock()
		current := room.reconciliationRun == run
		c.mu.RUnlock()
		if !current {
			return
		}
		select {
		case <-runCtx.Done():
			return
		case <-room.wake:
		case <-ticker.C:
		}
	}
}

func (c *controller) runForceStopWatchdog(room *controllerRoom, run uint64, deadline time.Time) {
	timer := time.NewTimer(time.Until(deadline))
	defer timer.Stop()
	select {
	case <-c.ctx.Done():
		return
	case <-timer.C:
	}
	room.dispatchGate.Lock()
	c.mu.Lock()
	if room.reconciliationRun != run || room.lifecycle != reconciliationStopping || !room.stopDeadline.Equal(deadline) {
		c.mu.Unlock()
		room.dispatchGate.Unlock()
		return
	}
	// The watchdog is independent of the actor, so an uncooperative HTTP request
	// cannot delay the 30-second local force-stop transition.
	c.forceStopLocked(room)
	view := c.snapshotLocked(room)
	c.mu.Unlock()
	room.dispatchGate.Unlock()
	c.publish(view)
}

// WHY: independent observe/navigation/running workers previously raced and could act on
// different evidence. The actor now owns one Observe-Diff-Act-Requeue sequence; each
// round dispatches each command at most once and returns to a fresh active-set read.
func (c *controller) reconcileRound(room *controllerRoom, run uint64) {
	c.mu.RLock()
	generation := room.desired.Generation
	lifecycle := room.lifecycle
	roundCtx := room.runCtx
	c.mu.RUnlock()
	if lifecycle != reconciliationActive && lifecycle != reconciliationStopping {
		return
	}
	ctx, cancel := context.WithTimeout(roundCtx, reconcileRequestTimeout)
	defer cancel()
	active, err := c.listActiveMeetings(ctx, room)
	if err != nil {
		c.recordObservationFailure(room, run, generation, err, true)
		return
	}
	if !c.applyActiveObservation(room, run, generation, active) {
		return
	}
	if lifecycle == reconciliationStopping || c.roomLifecycle(room, run) == reconciliationStopping {
		c.reconcileStop(room, run, generation, active)
		return
	}
	desired := c.desiredFor(room, run, generation)
	if desired.MeetingID == "" {
		c.setSummary(room, run, generation, reconciliationActive, "Blocked", "InitialMeetingRequired", "an administrator must select a desired meeting")
		return
	}
	if containsMeeting(active, desired.MeetingID) {
		// The filtered active set is authoritative. Previously a failed or lagging
		// unfiltered list blocked convergence and cleanup even after desired was
		// proven in_progress.
		for _, meeting := range active {
			if meeting.ID != desired.MeetingID {
				c.dispatchControl(room, run, generation, reconciliationActive, "pause_meeting", meeting.ID)
				c.setSummary(room, run, generation, reconciliationActive, "Degraded", "MultipleInProgress", "desired is active; cleaning up another active meeting")
				c.notify(room)
				return
			}
		}
		c.setSummary(room, run, generation, reconciliationActive, "Converged", "DesiredMeetingSoleInProgress", "")
		return
	}
	meetings, err := c.listMeetings(ctx, room)
	if err != nil {
		c.recordObservationFailure(room, run, generation, err, false)
		return
	}
	if !c.applyMeetings(room, run, generation, meetings) {
		return
	}
	desiredMeeting, found := meetingByID(meetings, desired.MeetingID)
	if !found {
		// WHY: pausing old meetings when the desired target is inaccessible causes outage.
		// Previously desired-running logic could clean up independently; now missing desired
		// preserves every existing active meeting and reports an explicit block.
		c.setSummary(room, run, generation, reconciliationActive, "Blocked", "DesiredMeetingMissing", "the desired meeting was not returned by Rozeta")
		return
	}

	// Availability-first switching requires Goto before attempting desired activation,
	// but a failed/unknown Goto must not block Start or Resume in this same round.
	c.dispatchGotoPair(room, run, generation, desired.MeetingID)
	switch desiredMeeting.Status {
	case "ready", "paused":
		c.dispatchControl(room, run, generation, reconciliationActive, "start_meeting", desired.MeetingID)
		c.setSummary(room, run, generation, reconciliationActive, "Reconciling", "StartingDesiredMeeting", "")
	case "completed":
		c.reconcileCompleted(room, run, desired, desiredMeeting)
	case "in_progress":
		// The filtered active-set response is authoritative. A detail/list mismatch is
		// observed again instead of treating stale detail data as convergence.
		c.setSummary(room, run, generation, reconciliationActive, "Reconciling", "StartingDesiredMeeting", "active-set observation has not yet included desired")
	}
	c.notify(room)
}

func (c *controller) reconcileStop(room *controllerRoom, run, generation uint64, active []roomMeetingView) {
	if len(active) == 0 {
		c.mu.Lock()
		if room.reconciliationRun != run || room.lifecycle != reconciliationStopping {
			c.mu.Unlock()
			return
		}
		room.lifecycle = reconciliationSuspended
		room.stopDeadline = time.Time{}
		room.activeSetStale = true
		room.summary, room.summaryReason = "Suspended", "LastStopConfirmedEmpty"
		room.conditions = suspendedConditions()
		room.revision++
		if room.runCancel != nil {
			room.runCancel()
		}
		view := c.snapshotLocked(room)
		c.mu.Unlock()
		c.publish(view)
		return
	}
	c.dispatchControl(room, run, generation, reconciliationStopping, "pause_meeting", active[0].ID)
	c.setSummary(room, run, generation, reconciliationStopping, "Reconciling", "PausingAllMeetings", "")
	c.notify(room)
}

func (c *controller) reconcileCompleted(room *controllerRoom, run uint64, desired desiredState, meeting roomMeetingView) {
	if meeting.UpdatedAt.IsZero() {
		c.setSummary(room, run, desired.Generation, reconciliationActive, "Blocked", "ResumeLimitReached", "completed meeting omitted updated_at")
		return
	}
	if desired.ConsumedResume != nil && desired.ConsumedResume.Generation == desired.Generation {
		c.setSummary(room, run, desired.Generation, reconciliationActive, "Blocked", "ResumeLimitReached", "automatic Resume was already consumed for this generation")
		return
	}
	c.mu.RLock()
	authorized := room.reconciliationRun == run && room.resumeAuthorized == desired.Generation
	c.mu.RUnlock()
	if !authorized {
		c.setSummary(room, run, desired.Generation, reconciliationActive, "Blocked", "ResumeAuthorizationRequired", "Start or an active desired change must authorize automatic Resume")
		return
	}
	marker := &consumedResume{Generation: desired.Generation, CompletedUpdatedAt: meeting.UpdatedAt}
	if err := c.consumeResumeBeforeDispatch(room, run, desired, marker); err != nil {
		if !errors.Is(err, errGenerationConflict) {
			c.setSummary(room, run, desired.Generation, reconciliationActive, "Blocked", "StateWriteFailed", err.Error())
		}
		return
	}
	// WHY: Resume destroys transcript data and transport failure is ambiguous. Previously
	// the marker was memory-only and could be retried after a crash; v2 persists it before
	// dispatch and this path never retries Resume without a new generation.
	c.dispatchControl(room, run, desired.Generation, reconciliationActive, "resume_meeting", desired.MeetingID)
	c.setSummary(room, run, desired.Generation, reconciliationActive, "Reconciling", "StartingDesiredMeeting", "automatic Resume was consumed; awaiting fresh observation")
}

func (c *controller) consumeResumeBeforeDispatch(room *controllerRoom, run uint64, desired desiredState, marker *consumedResume) error {
	room.dispatchGate.Lock()
	defer room.dispatchGate.Unlock()
	c.storeMu.Lock()
	defer c.storeMu.Unlock()
	c.mu.RLock()
	if !c.currentLocked(room, run, desired.Generation, reconciliationActive) || room.desired.ConsumedResume != nil {
		c.mu.RUnlock()
		return errGenerationConflict
	}
	candidate := cloneStateFile(c.file)
	next := desired
	next.ConsumedResume = marker
	candidate.Rooms[room.name] = next
	c.mu.RUnlock()
	if err := writeDesiredState(c.statePath, candidate); err != nil {
		var committed *committedStateError
		if !errors.As(err, &committed) {
			return err
		}
		c.fatal(err)
	}
	c.mu.Lock()
	if !c.currentLocked(room, run, desired.Generation, reconciliationActive) || room.desired.ConsumedResume != nil {
		c.mu.Unlock()
		return errGenerationConflict
	}
	room.desired = next
	c.file = candidate
	room.revision++
	c.mu.Unlock()
	return nil
}

func (c *controller) forceStopLocked(room *controllerRoom) {
	// WHY: an accepted remote command cannot be revoked. Previously force-stop reused a
	// generic error; now cancellation fences all old-run local results and explicitly
	// reports that the remote active set is unknown while allowing immediate restart.
	if room.runCancel != nil {
		room.runCancel()
	}
	room.lifecycle = reconciliationSuspended
	room.resumeAuthorized = 0
	room.stopDeadline = time.Time{}
	room.activeSetStale = true
	room.summary, room.summaryReason = "Suspended", "RemoteOutcomeUnknown"
	room.conditions = suspendedConditions()
	room.lastError = "remote outcome of cancelled work is unknown"
	room.revision++
}

func (c *controller) updateDesired(roomName string, update desiredStateUpdate) (roomView, error) {
	meetingID := strings.TrimSpace(update.MeetingID)
	if meetingID == "" {
		return roomView{}, errMeetingIDRequired
	}
	c.mu.RLock()
	room, found := c.rooms[roomName]
	if !found {
		c.mu.RUnlock()
		return roomView{}, errUnknownRoom
	}
	if update.ExpectedEpoch != c.app.epoch || room.reconciliationRun != update.ExpectedRun || room.desired.Generation != update.ExpectedGeneration {
		view := c.snapshotLocked(room)
		c.mu.RUnlock()
		return view, errGenerationConflict
	}
	current := room.desired
	lifecycle := room.lifecycle
	c.mu.RUnlock()

	if lifecycle == reconciliationStopping {
		return roomView{}, errReconciliationNotActive
	}
	if current.MeetingID == meetingID && !update.Rearm {
		c.mu.RLock()
		view := c.snapshotLocked(room)
		c.mu.RUnlock()
		return view, nil
	}
	if current.Generation == ^uint64(0) {
		return roomView{}, errGenerationExhausted
	}
	if update.Rearm && current.MeetingID != meetingID {
		return roomView{}, errInvalidReconciliation
	}
	// Active changes are verified remotely before acceptance. Suspended changes remain
	// command-free, but when Rozeta can prove the target completed they still require
	// the same explicit destructive confirmation contract.
	// A confirmed Start authorizes its generation in applyLifecycleLocked. An active
	// ready/paused switch is safe only at the instant of selection and must not grant
	// future destructive Resume; only a confirmed completed switch or explicit re-arm
	// grants the new generation an allowance.
	reconciling := lifecycle == reconciliationActive || lifecycle == reconciliationStarting
	authorizeResume := update.Rearm
	if reconciling {
		ctx, cancel := context.WithTimeout(c.ctx, reconcileRequestTimeout)
		meeting, err := c.getMeeting(ctx, room, meetingID)
		cancel()
		if err != nil {
			return roomView{}, fmt.Errorf("verify desired meeting: %w", err)
		}
		if meeting.Status == "completed" {
			if !update.ConfirmDestructiveResume {
				return roomView{}, errDestructiveConfirmation
			}
			authorizeResume = true
		}
	}
	if update.Rearm && !update.ConfirmDestructiveResume {
		return roomView{}, errDestructiveConfirmation
	}

	next := desiredState{MeetingID: meetingID, Generation: current.Generation + 1}
	room.dispatchGate.Lock()
	defer room.dispatchGate.Unlock()
	c.storeMu.Lock()
	defer c.storeMu.Unlock()
	c.mu.RLock()
	if room.reconciliationRun != update.ExpectedRun || room.desired.Generation != current.Generation || room.lifecycle == reconciliationStopping {
		view := c.snapshotLocked(room)
		c.mu.RUnlock()
		return view, errGenerationConflict
	}
	candidate := cloneStateFile(c.file)
	candidate.Rooms[roomName] = next
	c.mu.RUnlock()
	if err := writeDesiredState(c.statePath, candidate); err != nil {
		var committed *committedStateError
		if !errors.As(err, &committed) {
			return roomView{}, err
		}
		c.fatal(err)
	}
	c.mu.Lock()
	if room.reconciliationRun != update.ExpectedRun || room.desired.Generation != current.Generation || room.lifecycle == reconciliationStopping {
		c.mu.Unlock()
		return roomView{}, errGenerationConflict
	}
	room.desired = next
	c.file = candidate
	if authorizeResume {
		room.resumeAuthorized = next.Generation
	} else {
		room.resumeAuthorized = 0
	}
	room.desiredStatus = ""
	if room.lifecycle == reconciliationSuspended {
		room.conditions = suspendedConditions()
	} else {
		room.conditions = activeConditions(room.activeSetStale, false)
	}
	room.summary, room.summaryReason = summaryForLifecycle(room.lifecycle, next.MeetingID)
	room.updatedAt = time.Now().UTC()
	room.revision++
	view := c.snapshotLocked(room)
	c.mu.Unlock()
	if reconciling {
		c.notify(room)
	}
	c.publish(view)
	return view, nil
}

func cloneStateFile(file desiredStateFile) desiredStateFile {
	clone := desiredStateFile{Version: file.Version, Rooms: make(map[string]desiredState, len(file.Rooms))}
	for room, desired := range file.Rooms {
		clone.Rooms[room] = desired
	}
	return clone
}

func (c *controller) requestObservation(epoch, roomName string, expectedRun, expectedGeneration uint64) error {
	c.mu.RLock()
	room, found := c.rooms[roomName]
	if !found {
		c.mu.RUnlock()
		return errUnknownRoom
	}
	valid := epoch == c.app.epoch && room.reconciliationRun == expectedRun && room.desired.Generation == expectedGeneration && room.lifecycle == reconciliationActive
	c.mu.RUnlock()
	if !valid {
		return errReconciliationNotActive
	}
	c.notify(room)
	return nil
}

func (c *controller) notify(room *controllerRoom) {
	select {
	case room.wake <- struct{}{}:
	default:
	}
}

func (c *controller) listMeetings(ctx context.Context, room *controllerRoom) ([]roomMeetingView, error) {
	meetings, err := c.fetchMeetings(ctx, room)
	if err != nil {
		return nil, err
	}
	if !c.schedule.enabled {
		return meetings, nil
	}
	// Previously every full Rozeta read could replace the list and its order. The
	// startup snapshot now owns identity and order; cross-source additions and
	// removals are filtered as nonexistent while malformed retained data remains fatal.
	validated, err := c.schedule.validateMeetings(room.name, meetings)
	if err != nil {
		c.app.setMajorError("live Rozeta meeting validation failed", err)
		return nil, err
	}
	return validated, nil
}

func (c *controller) fetchMeetings(ctx context.Context, room *controllerRoom) ([]roomMeetingView, error) {
	meetings, err := runScheduled(ctx, c.scheduler, observationRequest, func(requestCtx context.Context) ([]roomMeetingView, error) {
		return c.app.fetchRozetaMeetings(requestCtx, room.token, "")
	})
	if err != nil && !errors.Is(err, context.Canceled) {
		c.app.setMajorError("Rozeta meeting list request failed", err)
	}
	return meetings, err
}

func (c *controller) validateStartupMeetings(ctx context.Context) error {
	c.mu.RLock()
	rooms := make([]*controllerRoom, 0, len(c.rooms))
	for _, room := range c.rooms {
		rooms = append(rooms, room)
	}
	c.mu.RUnlock()
	sort.Slice(rooms, func(left, right int) bool { return rooms[left].name < rooms[right].name })
	seenMeetingIDs := make(map[string]string, len(c.schedule.starts))

	for _, room := range rooms {
		meetings, err := c.fetchMeetings(ctx, room)
		if err != nil {
			return fmt.Errorf("room %q: fetch Rozeta meetings: %w", room.name, err)
		}
		prepared, err := c.schedule.validateStartupMeetings(room.name, meetings)
		if err != nil {
			return err
		}
		for _, meeting := range prepared {
			if previousRoom, duplicate := seenMeetingIDs[meeting.ID]; duplicate {
				return fmt.Errorf("meeting %q appeared in rooms %q and %q", meeting.ID, previousRoom, room.name)
			}
			seenMeetingIDs[meeting.ID] = room.name
		}
		c.mu.Lock()
		room.meetings = cloneMeetings(prepared)
		room.desiredStatus = observedStatus(prepared, room.desired.MeetingID)
		room.updatedAt = time.Now().UTC()
		room.revision++
		c.mu.Unlock()
	}
	for meetingID := range c.schedule.starts {
		if _, found := seenMeetingIDs[meetingID]; !found {
			log.Printf("ignoring unmatched session mapping: meeting_id=%q reason=meeting_not_in_rozeta", meetingID)
			delete(c.schedule.starts, meetingID)
		}
	}
	return nil
}

func (c *controller) listActiveMeetings(ctx context.Context, room *controllerRoom) ([]roomMeetingView, error) {
	return runScheduled(ctx, c.scheduler, observationRequest, func(requestCtx context.Context) ([]roomMeetingView, error) {
		return c.app.fetchRozetaMeetings(requestCtx, room.token, "in_progress")
	})
}

func (c *controller) currentMeeting(ctx context.Context, roomName string) (*currentMeetingResponse, error) {
	c.mu.RLock()
	room, found := c.rooms[roomName]
	if !found {
		c.mu.RUnlock()
		return nil, errUnknownRoom
	}
	desiredMeetingID := room.desired.MeetingID
	c.mu.RUnlock()

	active, err := c.listActiveMeetings(ctx, room)
	if err != nil {
		return nil, err
	}
	if snapshot, found := c.schedule.snapshots[roomName]; found {
		known := make(map[string]struct{}, len(snapshot))
		for _, meeting := range snapshot {
			known[meeting.ID] = struct{}{}
		}
		filtered := active[:0]
		for _, meeting := range active {
			if _, exists := known[meeting.ID]; exists {
				filtered = append(filtered, meeting)
			}
		}
		active = filtered
	}
	return selectCurrentMeeting(active, desiredMeetingID, c.schedule.opassIDs), nil
}

func selectCurrentMeeting(active []roomMeetingView, desiredMeetingID string, opassIDs map[string]string) *currentMeetingResponse {
	if len(active) == 0 {
		return nil
	}

	selected := roomMeetingView{}
	if len(active) == 1 {
		selected = active[0]
	} else {
		// Multiple remote meetings are a degraded state. Only expose the controller's
		// desired meeting when it is among them; guessing from response order could
		// report an old meeting during a transition.
		for _, meeting := range active {
			if meeting.ID == desiredMeetingID {
				selected = meeting
				break
			}
		}
		if selected.ID == "" {
			return nil
		}
	}

	opassID := strings.TrimSpace(opassIDs[selected.ID])
	if opassID == "" {
		return nil
	}
	return &currentMeetingResponse{Name: selected.Title, OPASSID: opassID}
}

func (c *controller) getMeeting(ctx context.Context, room *controllerRoom, meetingID string) (roomMeetingView, error) {
	return runScheduled(ctx, c.scheduler, observationRequest, func(requestCtx context.Context) (roomMeetingView, error) {
		return c.app.fetchRozetaMeeting(requestCtx, room.token, meetingID)
	})
}

func (c *controller) dispatchGotoPair(room *controllerRoom, run, generation uint64, meetingID string) {
	firstErr := c.controlOnce(room, run, generation, reconciliationActive, "goto_meeting", meetingID)
	secondErr := error(nil)
	if c.current(room, run, generation, reconciliationActive) {
		secondErr = c.controlOnce(room, run, generation, reconciliationActive, "goto_meeting_embed", meetingID)
	}
	err := errors.Join(firstErr, secondErr)
	condition := reconcileCondition{Type: "LatestGotoDispatch", Status: "True", Reason: "Dispatched"}
	if err != nil {
		condition.Status, condition.Reason, condition.Message = "False", "DispatchFailed", err.Error()
	}
	c.mu.Lock()
	if c.currentLocked(room, run, generation, reconciliationActive) {
		room.gotoCondition = condition
		c.appendActionLocked(room, recentAction{Action: "goto_pair", MeetingID: meetingID, Succeeded: err == nil, Dispatched: time.Now().UTC(), Error: errorString(err)})
		room.revision++
	}
	c.mu.Unlock()
}

func (c *controller) dispatchControl(room *controllerRoom, run, generation uint64, lifecycle reconciliationState, action, meetingID string) error {
	err := c.controlOnce(room, run, generation, lifecycle, action, meetingID)
	c.mu.Lock()
	if c.currentLocked(room, run, generation, lifecycle) {
		c.appendActionLocked(room, recentAction{Action: action, MeetingID: meetingID, Succeeded: err == nil, Dispatched: time.Now().UTC(), Error: errorString(err)})
		room.lastError = errorString(err)
		room.revision++
	}
	c.mu.Unlock()
	return err
}

func (c *controller) controlOnce(room *controllerRoom, run, generation uint64, lifecycle reconciliationState, action, meetingID string) error {
	c.mu.RLock()
	baseCtx := room.runCtx
	c.mu.RUnlock()
	// The old round context could be exhausted by the first Goto timeout, silently
	// skipping Embed and Start. Every control gets an independent bounded context.
	ctx, cancel := context.WithTimeout(baseCtx, c.controlTimeout)
	defer cancel()
	_, err := runScheduled(ctx, c.scheduler, controlRequest, func(requestCtx context.Context) (struct{}, error) {
		room.dispatchGate.Lock()
		if !c.current(room, run, generation, lifecycle) {
			room.dispatchGate.Unlock()
			return struct{}{}, context.Canceled
		}
		// Passing this gate is the dispatch linearization point. Stop cannot be
		// accepted until this reservation completes; after it completes, the request
		// is treated as already dispatched and may finish or time out.
		room.dispatchGate.Unlock()
		switch action {
		case "goto_meeting_embed":
			return struct{}{}, c.app.sendRozetaEmbedCommand(requestCtx, room.token, meetingID)
		case "resume_meeting":
			return struct{}{}, c.app.resumeRozetaMeeting(requestCtx, room.token, meetingID)
		default:
			return struct{}{}, c.app.sendRozetaCommand(requestCtx, room.token, action, meetingID)
		}
	})
	return err
}

func lockRoomDispatchGates(rooms []*controllerRoom) func() {
	ordered := append([]*controllerRoom{}, rooms...)
	sort.Slice(ordered, func(left, right int) bool { return ordered[left].name < ordered[right].name })
	for _, room := range ordered {
		room.dispatchGate.Lock()
	}
	return func() {
		for index := len(ordered) - 1; index >= 0; index-- {
			ordered[index].dispatchGate.Unlock()
		}
	}
}

func slicesEqualSorted(left, right []string) bool {
	left = append([]string{}, left...)
	right = append([]string{}, right...)
	sort.Strings(left)
	sort.Strings(right)
	return slices.Equal(left, right)
}

func (c *controller) appendActionLocked(room *controllerRoom, action recentAction) {
	room.recentActions = append(room.recentActions, action)
	if len(room.recentActions) > 10 {
		room.recentActions = append([]recentAction{}, room.recentActions[len(room.recentActions)-10:]...)
	}
}

func (c *controller) current(room *controllerRoom, run, generation uint64, lifecycle reconciliationState) bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.currentLocked(room, run, generation, lifecycle)
}

func (c *controller) currentLocked(room *controllerRoom, run, generation uint64, lifecycle reconciliationState) bool {
	return room.reconciliationRun == run && room.desired.Generation == generation && room.lifecycle == lifecycle
}

func (c *controller) currentAnyLifecycle(room *controllerRoom, run, generation uint64) bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.currentLockedAnyLifecycle(room, run, generation)
}

func (c *controller) currentLockedAnyLifecycle(room *controllerRoom, run, generation uint64) bool {
	return room.reconciliationRun == run && room.desired.Generation == generation &&
		(room.lifecycle == reconciliationActive || room.lifecycle == reconciliationStopping)
}

func (c *controller) applyActiveObservation(room *controllerRoom, run, generation uint64, meetings []roomMeetingView) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.currentLockedAnyLifecycle(room, run, generation) {
		return false
	}
	room.activeMeetingIDs = meetingIDs(meetings)
	room.activeObservedAt = time.Now().UTC()
	room.activeSetStale = false
	if room.desired.MeetingID != "" && containsMeeting(meetings, room.desired.MeetingID) {
		room.desiredStatus = "in_progress"
	} else if room.desiredStatus == "in_progress" {
		// A fresh filtered active-set exclusion invalidates the previous in_progress
		// status even when the following full meeting read fails.
		room.desiredStatus = "unknown"
	}
	room.updatedAt = room.activeObservedAt
	room.revision++
	return true
}

func (c *controller) applyMeetings(room *controllerRoom, run, generation uint64, meetings []roomMeetingView) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.currentLocked(room, run, generation, reconciliationActive) {
		return false
	}
	room.meetings = append([]roomMeetingView{}, meetings...)
	room.desiredStatus = observedStatus(meetings, room.desired.MeetingID)
	room.updatedAt = time.Now().UTC()
	room.revision++
	return true
}

func (c *controller) recordObservationFailure(room *controllerRoom, run, generation uint64, err error, activeSetFailed bool) {
	if errors.Is(err, context.Canceled) {
		return
	}
	c.mu.Lock()
	if c.currentLockedAnyLifecycle(room, run, generation) {
		if activeSetFailed {
			room.activeSetStale = true
		}
		room.lastError = err.Error()
		room.summary = "Blocked"
		room.summaryReason = "DesiredMeetingObservationFailed"
		if activeSetFailed {
			room.summaryReason = "ActiveSetObservationFailed"
		}
		room.conditions = activeConditions(room.activeSetStale, false)
		room.revision++
		view := c.snapshotLocked(room)
		c.mu.Unlock()
		c.publish(view)
		return
	}
	c.mu.Unlock()
}

func (c *controller) setSummary(room *controllerRoom, run, generation uint64, lifecycle reconciliationState, summary, reason, message string) {
	c.mu.Lock()
	if !c.currentLocked(room, run, generation, lifecycle) {
		c.mu.Unlock()
		return
	}
	room.summary, room.summaryReason, room.lastError = summary, reason, message
	sole := summary == "Converged" && reason == "DesiredMeetingSoleInProgress"
	room.conditions = activeConditions(room.activeSetStale, sole)
	room.revision++
	view := c.snapshotLocked(room)
	c.mu.Unlock()
	c.publish(view)
}

func activeConditions(stale, sole bool) []reconcileCondition {
	observed := "True"
	if stale {
		observed = "False"
	}
	soleStatus := "False"
	if sole {
		soleStatus = "True"
	}
	return []reconcileCondition{
		{Type: "ReconciliationActive", Status: "True"},
		{Type: "ActiveSetObserved", Status: observed},
		{Type: "DesiredMeetingSoleInProgress", Status: soleStatus},
	}
}

func suspendedConditions() []reconcileCondition {
	return []reconcileCondition{
		{Type: "ReconciliationActive", Status: "False"},
		{Type: "ActiveSetObserved", Status: "Unknown", Reason: "Suspended"},
		{Type: "DesiredMeetingSoleInProgress", Status: "Unknown", Reason: "Suspended"},
	}
}

func summaryForLifecycle(lifecycle reconciliationState, meetingID string) (string, string) {
	if lifecycle == reconciliationSuspended {
		if meetingID == "" {
			return "Suspended", "InitialMeetingRequired"
		}
		return "Suspended", "ActiveSetUnknown"
	}
	return "Reconciling", "StartingDesiredMeeting"
}

func (c *controller) roomLifecycle(room *controllerRoom, run uint64) reconciliationState {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if room.reconciliationRun != run {
		return reconciliationSuspended
	}
	return room.lifecycle
}

func (c *controller) desiredFor(room *controllerRoom, run, generation uint64) desiredState {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if room.reconciliationRun != run || room.desired.Generation != generation {
		return desiredState{}
	}
	return room.desired
}

func meetingIDs(meetings []roomMeetingView) []string {
	ids := make([]string, 0, len(meetings))
	for _, meeting := range meetings {
		ids = append(ids, meeting.ID)
	}
	sort.Strings(ids)
	return ids
}

func containsMeeting(meetings []roomMeetingView, meetingID string) bool {
	_, found := meetingByID(meetings, meetingID)
	return found
}

func meetingByID(meetings []roomMeetingView, meetingID string) (roomMeetingView, bool) {
	for _, meeting := range meetings {
		if meeting.ID == meetingID {
			return meeting, true
		}
	}
	return roomMeetingView{}, false
}

func observedStatus(meetings []roomMeetingView, meetingID string) string {
	meeting, found := meetingByID(meetings, meetingID)
	if !found {
		return "unknown"
	}
	return meeting.Status
}

func errorString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func (c *controller) snapshotRooms() []roomView {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.snapshotRoomsLocked()
}

func (c *controller) snapshotRoomsLocked() []roomView {
	views := make([]roomView, 0, len(c.rooms))
	for _, room := range c.rooms {
		views = append(views, c.snapshotLocked(room))
	}
	sort.Slice(views, func(left, right int) bool { return views[left].RoomName < views[right].RoomName })
	return views
}

func (c *controller) refreshRoomMeetings(ctx context.Context, roomName string) (roomView, []roomMeetingView, error) {
	c.mu.RLock()
	room, found := c.rooms[roomName]
	if !found {
		c.mu.RUnlock()
		return roomView{}, nil, errUnknownRoom
	}
	run, generation := room.reconciliationRun, room.desired.Generation
	c.mu.RUnlock()

	meetings, err := c.listMeetings(ctx, room)
	if err != nil {
		return roomView{}, nil, err
	}
	c.mu.Lock()
	// The meetings endpoint used to return only the actor cache. Since every room
	// now starts suspended, that made initial desired selection impossible. A fresh
	// full list is cached only if its optimistic fences still match, so a slow admin
	// read cannot overwrite observations for a newer run or desired generation.
	if room.reconciliationRun != run || room.desired.Generation != generation {
		view := c.snapshotLocked(room)
		c.mu.Unlock()
		return view, nil, errReconciliationConflict
	}
	room.meetings = append([]roomMeetingView{}, meetings...)
	room.desiredStatus = observedStatus(meetings, room.desired.MeetingID)
	room.updatedAt = time.Now().UTC()
	room.revision++
	view := c.snapshotLocked(room)
	c.mu.Unlock()
	c.publish(view)
	return view, meetings, nil
}

func (c *controller) snapshotLocked(room *controllerRoom) roomView {
	conditions := append([]reconcileCondition{}, room.conditions...)
	if room.gotoCondition.Type != "" {
		conditions = append(conditions, room.gotoCondition)
	}
	if room.advanceAlert != nil {
		conditions = append(conditions, *room.advanceAlert)
	}
	return roomView{
		Epoch: c.app.epoch, RoomName: room.name,
		DesiredMeetingID: room.desired.MeetingID, Generation: room.desired.Generation,
		ResumeConsumed: room.desired.ConsumedResume != nil,
		Revision:       room.revision, DesiredStatus: room.desiredStatus,
		Lifecycle: string(room.lifecycle), ReconciliationRun: room.reconciliationRun,
		ActiveMeetingIDs: append([]string{}, room.activeMeetingIDs...),
		ActiveObservedAt: room.activeObservedAt, ActiveSetStale: room.activeSetStale || room.lifecycle == reconciliationSuspended,
		Summary: room.summary, SummaryReason: room.summaryReason,
		Conditions: conditions, RecentActions: append([]recentAction{}, room.recentActions...),
		LastError: room.lastError, Meetings: append([]roomMeetingView{}, room.meetings...), UpdatedAt: room.updatedAt,
	}
}

func (c *controller) publish(view roomView) { c.app.broadcastRoom(view) }
