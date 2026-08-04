import assert from 'node:assert/strict'
import test from 'node:test'

import {
	bufferRoomSnapshot,
	canEditDesired,
	canObserve,
	cloneReconciliationTargets,
	confirmationTargets,
	isCurrentVersion,
	isPreflightFactsChanged,
	reconciliationActionFor,
	reconciliationRequestBody,
	reconciliationTargets,
	reconcileAuthoritativeRooms,
	shouldAcceptConflictSnapshot,
	shouldAcceptSnapshot,
	takeBufferedRoomSnapshots,
} from './state.js'

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
