import assert from 'node:assert/strict'
import test from 'node:test'

import {
	availableMeetingDates,
	bufferRoomSnapshot,
	canEditDesired,
	canObserve,
	cloneReconciliationTargets,
	confirmationTargets,
	createClientClock,
	defaultAlertThresholdMinutes,
	evaluateScheduleAlert,
	isCurrentVersion,
	meetingDateKey,
	meetingsForDate,
	isPreflightFactsChanged,
	reconciliationActionFor,
	reconciliationRequestBody,
	reconciliationTargets,
	reconcileAuthoritativeRooms,
	shouldAcceptConflictSnapshot,
	shouldAcceptSnapshot,
	takeBufferedRoomSnapshots,
	roomNameIncludes,
	visibleRooms,
} from './state.js'
import {
	intersectsTimelineWindow,
	shiftedMeetingTimes,
	timelinePositionPercent,
	timelineQuarterHourTicks,
	timelineWindow,
} from './timeline-model.js'

test('a restarted process can replace a higher old revision', () => {
	const oldRoom = { room_name: 'room-a', generation: 3, revision: 40 }
	const restartedRoom = { room_name: 'room-a', generation: 3, revision: 1 }
	assert.equal(isCurrentVersion(restartedRoom, oldRoom), false)
	const result = reconcileAuthoritativeRooms(new Map(), new Map(), [restartedRoom])
	assert.deepEqual(result.rooms.get('room-a'), restartedRoom)
})

test('an authoritative snapshot removes absent rooms and meeting caches', () => {
	const currentRooms = new Map([
		['room-a', { room_name: 'room-a', generation: 1, revision: 2 }],
		['room-b', { room_name: 'room-b', generation: 1, revision: 2 }],
	])
	const currentMeetings = new Map([
		['room-a', [{ id: 'meeting-a' }]],
		['room-b', [{ id: 'meeting-b' }]],
	])
	const result = reconcileAuthoritativeRooms(currentRooms, currentMeetings, [
		{ room_name: 'room-a', generation: 1, revision: 3, meetings: [] },
	])
	assert.deepEqual([...result.rooms.keys()], ['room-a'])
	assert.deepEqual(result.meetings.get('room-a'), [])
	assert.equal(result.meetings.has('room-b'), false)
})

test('a same-process stale snapshot cannot replace a newer room', () => {
	const current = { room_name: 'room-a', generation: 2, revision: 9 }
	const stale = { room_name: 'room-a', generation: 2, revision: 8 }
	const result = reconcileAuthoritativeRooms(new Map([['room-a', current]]), new Map(), [stale])
	assert.deepEqual(result.rooms.get('room-a'), current)
})

test('a delayed HTTP response cannot reverse a WebSocket epoch', () => {
	assert.equal(shouldAcceptSnapshot('new-process', 'old-process', 'http', true), false)
	assert.equal(shouldAcceptSnapshot('old-process', 'new-process', 'websocket', true), true)
	assert.equal(shouldAcceptSnapshot('new-process', 'old-process', 'conflict', true), false)
	assert.equal(shouldAcceptSnapshot('old-process', 'new-process', 'conflict', false), true)
})

test('conflict epochs distinguish replacement servers from delayed old responses', () => {
	assert.equal(shouldAcceptConflictSnapshot('old-process', 'new-process', 'old-process'), true)
	assert.equal(shouldAcceptConflictSnapshot('new-process', 'old-process', 'old-process'), false)
	assert.equal(shouldAcceptConflictSnapshot('new-process', 'new-process', 'old-process'), true)
	assert.equal(shouldAcceptConflictSnapshot('newest-process', 'middle-process', 'old-process'), false)
})

test('room updates received before a full snapshot replay the newest revision', () => {
	const buffer = new Map()
	bufferRoomSnapshot(buffer, {
		epoch: 'new-process',
		room: { room_name: 'room-a', generation: 2, revision: 11 },
	})
	bufferRoomSnapshot(buffer, {
		epoch: 'new-process',
		room: { room_name: 'room-a', generation: 2, revision: 10 },
	})
	bufferRoomSnapshot(buffer, {
		epoch: 'old-process',
		room: { room_name: 'room-b', generation: 1, revision: 99 },
	})
	const snapshots = takeBufferedRoomSnapshots(buffer, 'new-process')
	assert.equal(snapshots.length, 1)
	assert.equal(snapshots[0].room.revision, 11)
	assert.equal(buffer.size, 0)
})

test('reconciliation lifecycle exposes the valid next action', () => {
	assert.equal(reconciliationActionFor({ lifecycle: 'suspended' }), 'start')
	assert.equal(reconciliationActionFor({ lifecycle: 'starting' }), 'stop')
	assert.equal(reconciliationActionFor({ lifecycle: 'active' }), 'stop')
	assert.equal(reconciliationActionFor({ lifecycle: 'stopping' }), 'force-stop')
	assert.equal(reconciliationActionFor({ lifecycle: 'unknown' }), '')
})

test('bulk reconciliation freezes eligible rooms with run and generation fences', () => {
	const rooms = [
		{ room_name: 'room-b', lifecycle: 'suspended', reconciliation_run: 4, generation: 7 },
		{ room_name: 'room-c', lifecycle: 'active', reconciliation_run: 8, generation: 3 },
		{ room_name: 'room-a', lifecycle: 'suspended', reconciliation_run: 2, generation: 5 },
	]
	const targets = reconciliationTargets(rooms, 'start')
	rooms[0].reconciliation_run = 5
	assert.deepEqual(targets, [
		{ room_name: 'room-a', expected_reconciliation_run: 2, expected_generation: 5 },
		{ room_name: 'room-b', expected_reconciliation_run: 4, expected_generation: 7 },
	])
})

test('Start confirmation freezes the destructive Resume fact for observable rooms', () => {
	assert.deepEqual(
		confirmationTargets(
			[
				{ room_name: 'room-a', expected_reconciliation_run: 1, expected_generation: 2 },
				{ room_name: 'room-b', expected_reconciliation_run: 3, expected_generation: 4 },
				{ room_name: 'room-c', expected_reconciliation_run: 5, expected_generation: 6 },
			],
			[
				{ room_name: 'room-a', observable: false, error: 'timeout' },
				{ room_name: 'room-b', observable: true, destructive_resume: false },
				{ room_name: 'room-c', observable: true, destructive_resume: true },
			],
			'start',
		),
		[
			{
				room_name: 'room-b',
				expected_reconciliation_run: 3,
				expected_generation: 4,
				preflight: { destructive_resume: false },
			},
			{
				room_name: 'room-c',
				expected_reconciliation_run: 5,
				expected_generation: 6,
				preflight: { destructive_resume: true },
			},
		],
	)
})

test('Stop confirmation freezes active meeting IDs without sharing the source array', () => {
	const activeMeetingIDs = ['meeting-b', 'meeting-a']
	const confirmed = confirmationTargets(
		[{ room_name: 'room-a', expected_reconciliation_run: 7, expected_generation: 8 }],
		[{ room_name: 'room-a', observable: true, active_meeting_ids: activeMeetingIDs }],
		'stop',
	)
	activeMeetingIDs.push('meeting-c')
	assert.deepEqual(confirmed[0].preflight, { active_meeting_ids: ['meeting-b', 'meeting-a'] })
})

test('malformed preflight facts are never sent as confirmed targets', () => {
	const target = [{ room_name: 'room-a', expected_reconciliation_run: 1, expected_generation: 2 }]
	assert.deepEqual(confirmationTargets(target, [{ room_name: 'room-a', observable: true }], 'start'), [])
	assert.deepEqual(confirmationTargets(target, [{ room_name: 'room-a', observable: true }], 'stop'), [])
})

test('frozen target cloning deep-copies Stop facts and preserves Start false', () => {
	const targets = [
		{ room_name: 'room-a', preflight: { active_meeting_ids: ['meeting-a'] } },
		{ room_name: 'room-b', preflight: { destructive_resume: false } },
	]
	const cloned = cloneReconciliationTargets(targets)
	targets[0].preflight.active_meeting_ids.push('meeting-b')
	assert.deepEqual(cloned, [
		{ room_name: 'room-a', preflight: { active_meeting_ids: ['meeting-a'] } },
		{ room_name: 'room-b', preflight: { destructive_resume: false } },
	])
})

test('preflight fact conflicts are distinguished from optimistic conflicts', () => {
	assert.equal(isPreflightFactsChanged('preflight facts changed; confirmation must be repeated'), true)
	assert.equal(isPreflightFactsChanged('reconciliation state conflict'), false)
})

test('single and bulk confirmed payloads carry exactly the frozen preflight facts', () => {
	const startTarget = {
		room_name: 'room-a',
		expected_reconciliation_run: 2,
		expected_generation: 3,
		preflight: { destructive_resume: false },
	}
	const stopTarget = {
		room_name: 'room-b',
		expected_reconciliation_run: 4,
		expected_generation: 5,
		preflight: { active_meeting_ids: ['meeting-a'] },
	}
	assert.deepEqual(reconciliationRequestBody({ epoch: 'process-a', bulk: false, confirmedTargets: [startTarget] }), {
		epoch: 'process-a',
		expected_reconciliation_run: 2,
		expected_generation: 3,
		preflight: { destructive_resume: false },
		confirmed: true,
	})
	assert.deepEqual(
		reconciliationRequestBody({ epoch: 'process-a', bulk: true, confirmedTargets: [startTarget, stopTarget] }),
		{ epoch: 'process-a', rooms: [startTarget, stopTarget], confirmed: true },
	)
})

test('Force-stop confirmed payload has no preflight facts', () => {
	assert.deepEqual(
		reconciliationRequestBody({
			epoch: 'process-a',
			bulk: false,
			confirmedTargets: [{ room_name: 'room-a', expected_reconciliation_run: 9, expected_generation: 10 }],
		}),
		{
			epoch: 'process-a',
			expected_reconciliation_run: 9,
			expected_generation: 10,
			confirmed: true,
		},
	)
})

test('desired editing and manual observation follow lifecycle constraints', () => {
	assert.equal(canEditDesired({ lifecycle: 'suspended' }), true)
	assert.equal(canEditDesired({ lifecycle: 'starting' }), true)
	assert.equal(canEditDesired({ lifecycle: 'active' }), true)
	assert.equal(canEditDesired({ lifecycle: 'stopping' }), false)
	assert.equal(canObserve({ lifecycle: 'active' }), true)
	assert.equal(canObserve({ lifecycle: 'starting' }), false)
})

test('room visibility filters only hidden rooms and preserves new rooms by default', () => {
	const rooms = [{ room_name: 'Room-A' }, { room_name: 'Room-B' }, { room_name: 'Room-C' }]
	assert.deepEqual(visibleRooms(rooms, new Set(['Room-B'])), [{ room_name: 'Room-A' }, { room_name: 'Room-C' }])
	assert.deepEqual(visibleRooms(rooms, new Set(['Room-B', 'Removed-Room'])), [
		{ room_name: 'Room-A' },
		{ room_name: 'Room-C' },
	])
})

test('room picker search uses case-insensitive substring matching only', () => {
	assert.equal(roomNameIncludes('Room-A', 'oom-'), true)
	assert.equal(roomNameIncludes('Room-A', 'ROOM'), true)
	assert.equal(roomNameIncludes('Room-A', '*'), false)
	assert.equal(roomNameIncludes('Room-A', '?'), false)
})

test('meeting dates use the browser local calendar date and sort chronologically', () => {
	assert.equal(meetingDateKey({ scheduled_start: '2026-08-08T10:30:00+08:00' }), '2026-08-08')
	assert.deepEqual(
		availableMeetingDates(
			new Map([
				['room-a', [{ scheduled_start: '2026-08-09T10:00:00+08:00' }]],
				['room-b', [{ scheduled_start: '2026-08-08T10:00:00+08:00' }]],
			]),
		),
		['2026-08-08', '2026-08-09'],
	)
})

test('meeting list filtering keeps only meetings for the selected date', () => {
	const meetings = [
		{ id: 'preparation', virtual: true, title: '準備' },
		{ id: 'day-two', scheduled_start: '2026-08-09T09:00:00+08:00' },
		{ id: 'day-one', scheduled_start: '2026-08-08T09:00:00+08:00' },
	]
	assert.deepEqual(meetingsForDate(meetings, '2026-08-08'), [meetings[0], meetings[2]])
	assert.deepEqual(meetingsForDate(meetings, '2026-08-10'), [meetings[0]])
})

test('schedule alert uses the next meeting, room offset, and inclusive threshold', () => {
	const room = {
		room_name: 'room-a',
		lifecycle: 'active',
		desired_status: 'in_progress',
		desired_meeting_id: 'current',
		active_meeting_ids: ['current'],
		schedule_offset_minutes: 10,
	}
	const meetings = [
		{ id: 'next', title: 'Next', scheduled_start: '2026-08-06T12:00:00Z' },
		{ id: 'current', title: 'Current', scheduled_start: '2026-08-06T11:00:00Z' },
	]
	const alert = evaluateScheduleAlert(room, meetings, new Date('2026-08-06T12:20:00Z'), 10)
	assert.equal(alert.meetingID, 'next')
	assert.equal(alert.adjustedStart.toISOString(), '2026-08-06T12:10:00.000Z')
	assert.equal(alert.overdueMinutes, 0)
})

test('schedule alert applies negative offset and default threshold', () => {
	const room = {
		room_name: 'room-a',
		lifecycle: 'active',
		desired_status: 'in_progress',
		desired_meeting_id: 'current',
		active_meeting_ids: ['current'],
		schedule_offset_minutes: -5,
	}
	const meetings = [
		{ id: 'current', scheduled_start: '2026-08-06T11:00:00Z' },
		{ id: 'next', scheduled_start: '2026-08-06T12:00:00Z' },
	]
	assert.equal(defaultAlertThresholdMinutes, 5)
	assert.equal(evaluateScheduleAlert(room, meetings, new Date('2026-08-06T11:59:00Z')), null)
	assert.equal(evaluateScheduleAlert(room, meetings, new Date('2026-08-06T12:00:00Z'))?.meetingID, 'next')
})

test('schedule alert is disabled unless desired is the sole active meeting', () => {
	const room = {
		room_name: 'room-a',
		lifecycle: 'active',
		desired_status: 'in_progress',
		desired_meeting_id: 'current',
		active_meeting_ids: ['current', 'other'],
	}
	assert.equal(
		evaluateScheduleAlert(
			room,
			[
				{ id: 'current', scheduled_start: '2026-08-06T11:00:00Z' },
				{ id: 'next', scheduled_start: '2026-08-06T12:00:00Z' },
			],
			new Date('2026-08-06T13:00:00Z'),
		),
		null,
	)
})

test('schedule alert detects an active desired meeting that started too early', () => {
	const room = {
		room_name: 'room-a',
		lifecycle: 'active',
		desired_status: 'in_progress',
		desired_meeting_id: 'current',
		active_meeting_ids: ['current'],
		schedule_offset_minutes: 0,
	}
	const meetings = [
		{ id: 'previous', title: 'Previous', scheduled_start: '2026-08-06T11:00:00Z' },
		{ id: 'current', title: 'Current', scheduled_start: '2026-08-06T12:00:00Z' },
		{ id: 'next', title: 'Next', scheduled_start: '2026-08-06T13:00:00Z' },
	]
	const alert = evaluateScheduleAlert(room, meetings, new Date('2026-08-06T11:30:00Z'), 10)
	assert.equal(alert.kind, 'early')
	assert.equal(alert.meetingID, 'current')
	assert.equal(alert.previousMeetingID, 'previous')
	assert.equal(alert.alertAt.toISOString(), '2026-08-06T11:50:00.000Z')
	assert.equal(evaluateScheduleAlert(room, meetings, new Date('2026-08-06T11:50:00Z'), 10), null)
})

test('schedule alert does not classify the first meeting as early', () => {
	const room = {
		room_name: 'room-a',
		lifecycle: 'active',
		desired_status: 'in_progress',
		desired_meeting_id: 'first',
		active_meeting_ids: ['first'],
	}
	assert.equal(
		evaluateScheduleAlert(
			room,
			[
				{ id: 'first', scheduled_start: '2026-08-06T12:00:00Z' },
				{ id: 'next', scheduled_start: '2026-08-06T13:00:00Z' },
			],
			new Date('2026-08-06T11:00:00Z'),
			10,
		),
		null,
	)
})

test('client test clock starts at the query time and continues at real-time speed', () => {
	const clock = createClientClock('?alert_test_at=2026-08-06T12:00:00Z', 1_000)
	assert.equal(clock.enabled, true)
	assert.equal(clock.now(1_000).toISOString(), '2026-08-06T12:00:00.000Z')
	assert.equal(clock.now(61_000).toISOString(), '2026-08-06T12:01:00.000Z')
})

test('invalid or missing test clock uses the real browser time', () => {
	const missing = createClientClock('', 1_000)
	const invalid = createClientClock('?alert_test_at=invalid', 1_000)
	assert.equal(missing.enabled, false)
	assert.equal(invalid.enabled, false)
	assert.equal(missing.now(61_000).getTime(), 61_000)
})

test('timeline window stays centered on now and uses quarter-hour ticks', () => {
	const now = new Date('2026-08-08T10:15:00+08:00')
	const window = timelineWindow(now)
	assert.equal(window.start.toISOString(), '2026-08-08T01:15:00.000Z')
	assert.equal(window.end.toISOString(), '2026-08-08T03:15:00.000Z')
	assert.deepEqual(
		timelineQuarterHourTicks(window.start, window.end).map(value => value.toISOString()),
		[
			'2026-08-08T01:15:00.000Z',
			'2026-08-08T01:30:00.000Z',
			'2026-08-08T01:45:00.000Z',
			'2026-08-08T02:00:00.000Z',
			'2026-08-08T02:15:00.000Z',
			'2026-08-08T02:30:00.000Z',
			'2026-08-08T02:45:00.000Z',
			'2026-08-08T03:00:00.000Z',
			'2026-08-08T03:15:00.000Z',
		],
	)
})

test('timeline offset preserves the original window and shifts the adjusted window', () => {
	const shifted = shiftedMeetingTimes(
		{ scheduled_start: '2026-08-08T10:00:00+08:00', scheduled_end: '2026-08-08T10:40:00+08:00' },
		5,
	)
	assert.equal(shifted.originalStart.toISOString(), '2026-08-08T02:00:00.000Z')
	assert.equal(shifted.originalEnd.toISOString(), '2026-08-08T02:40:00.000Z')
	assert.equal(shifted.adjustedStart.toISOString(), '2026-08-08T02:05:00.000Z')
	assert.equal(shifted.adjustedEnd.toISOString(), '2026-08-08T02:45:00.000Z')
})

test('timeline renders meetings that cross either 60-minute boundary', () => {
	const window = timelineWindow(new Date('2026-08-08T10:15:00+08:00'))
	assert.equal(
		intersectsTimelineWindow(new Date('2026-08-08T08:50:00+08:00'), new Date('2026-08-08T09:30:00+08:00'), window),
		true,
	)
	assert.equal(
		intersectsTimelineWindow(new Date('2026-08-08T11:00:00+08:00'), new Date('2026-08-08T11:40:00+08:00'), window),
		true,
	)
	assert.equal(
		intersectsTimelineWindow(new Date('2026-08-08T07:00:00+08:00'), new Date('2026-08-08T08:00:00+08:00'), window),
		false,
	)
})

test('timeline positions are finite percentages across the full time window', () => {
	const window = timelineWindow(new Date('2026-08-08T10:15:00+08:00'))
	assert.equal(timelinePositionPercent(window.start, window), 0)
	assert.equal(timelinePositionPercent(new Date('2026-08-08T10:15:00+08:00'), window), 50)
	assert.equal(timelinePositionPercent(window.end, window), 100)
	assert.equal(timelinePositionPercent(new Date('invalid'), window), null)
})
