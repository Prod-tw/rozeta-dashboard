export function isCurrentVersion(incoming, current) {
	if (!current) return true
	const incomingGeneration = Number(incoming?.generation || 0)
	const currentGeneration = Number(current.generation || 0)
	if (incomingGeneration !== currentGeneration) return incomingGeneration > currentGeneration
	return Number(incoming?.revision || 0) >= Number(current.revision || 0)
}

export function reconcileAuthoritativeRooms(currentRooms, currentMeetings, incomingRooms) {
	const rooms = new Map()
	const meetings = new Map()
	for (const incoming of incomingRooms) {
		if (!incoming?.room_name) continue
		const current = currentRooms.get(incoming.room_name)
		const accepted = isCurrentVersion(incoming, current)
		const room = accepted ? incoming : current
		rooms.set(room.room_name, room)
		if (accepted && Array.isArray(incoming.meetings)) {
			meetings.set(room.room_name, incoming.meetings)
		} else if (currentMeetings.has(room.room_name)) {
			meetings.set(room.room_name, currentMeetings.get(room.room_name))
		}
	}
	return { rooms, meetings }
}

export function shouldAcceptSnapshot(currentEpoch, incomingEpoch, source, websocketSnapshotReceived) {
	if (!incomingEpoch) return false
	// Delayed HTTP and conflict responses from a previous process must not roll
	// back an epoch already established by WebSocket. A WebSocket snapshot may
	// still install a new process epoch.
	return !(
		source !== 'websocket' &&
		source !== 'authoritative-conflict' &&
		websocketSnapshotReceived &&
		currentEpoch &&
		incomingEpoch !== currentEpoch
	)
}

export function shouldAcceptConflictSnapshot(currentEpoch, responseEpoch, requestEpoch) {
	if (!responseEpoch || !requestEpoch) return false
	// A different response epoch proves that the request reached a replacement
	// process. A matching response can be delayed old-process data and is accepted
	// only while the browser has not already advanced to another epoch.
	if (!currentEpoch || currentEpoch === responseEpoch) return true
	return currentEpoch === requestEpoch && responseEpoch !== requestEpoch
}

export function bufferRoomSnapshot(buffer, message) {
	const roomName = message?.room?.room_name
	if (!roomName) return
	const current = buffer.get(roomName)
	if (!current || isCurrentVersion(message.room, current.room)) buffer.set(roomName, message)
}

export function takeBufferedRoomSnapshots(buffer, epoch) {
	const snapshots = Array.from(buffer.values()).filter(message => message.epoch === epoch)
	buffer.clear()
	return snapshots
}

export function reconciliationActionFor(room) {
	// WHY: the previous UI mapped stopped/running transport-era states. The controller now owns an explicit four-state
	// lifecycle, including immediate Force-stop availability while a normal Stop is draining.
	switch (room?.lifecycle) {
		case 'suspended':
			return 'start'
		case 'starting':
		case 'active':
			return 'stop'
		case 'stopping':
			return 'force-stop'
		default:
			return ''
	}
}

export function reconciliationTargets(rooms, action) {
	// WHY: run alone previously allowed a frozen bulk confirmation to cross a desired update. Each target now carries
	// both controller fences, so epoch plus the complete browser snapshot is validated atomically by the server.
	return Array.from(rooms)
		.filter(room => reconciliationActionFor(room) === action)
		.map(room => ({
			room_name: room.room_name,
			expected_reconciliation_run: Number(room.reconciliation_run || 0),
			expected_generation: Number(room.generation || 0),
		}))
		.sort((left, right) => left.room_name.localeCompare(right.room_name))
}

export function confirmationTargets(targets, results, action) {
	// WHY: confirmed requests previously carried only optimistic fences, allowing backend observations to change after
	// the dialog was shown. Freeze the risk-bearing fact for each observable room so changed facts force a new preflight.
	const resultsByRoom = new Map(results.map(result => [result?.room_name, result]))
	return targets.flatMap(target => {
		const result = resultsByRoom.get(target.room_name)
		if (!result?.observable) return []
		if (action === 'start' && typeof result.destructive_resume === 'boolean') {
			return [{ ...target, preflight: { destructive_resume: result.destructive_resume } }]
		}
		if (action === 'stop' && Array.isArray(result.active_meeting_ids)) {
			return [{ ...target, preflight: { active_meeting_ids: [...result.active_meeting_ids] } }]
		}
		return []
	})
}

export function cloneReconciliationTargets(targets) {
	// WHY: a shallow copy left Stop's active ID array shared with the preflight response. Deep-copying the small facts
	// object keeps the confirmed payload identical to what was rendered even if another owner mutates its source array.
	return targets.map(target => ({
		...target,
		...(target.preflight
			? {
					preflight: {
						...target.preflight,
						...(Array.isArray(target.preflight.active_meeting_ids)
							? { active_meeting_ids: [...target.preflight.active_meeting_ids] }
							: {}),
					},
				}
			: {}),
	}))
}

export function reconciliationRequestBody(intent) {
	// WHY: lifecycle payloads previously diverged between single and bulk paths, and single requests omitted the facts
	// shown in the dialog. Build both forms from the same frozen targets while leaving Force-stop fact-free.
	const targets = cloneReconciliationTargets(intent.confirmedTargets)
	if (intent.bulk) return { epoch: intent.epoch, rooms: targets, confirmed: true }
	const target = targets[0]
	return {
		epoch: intent.epoch,
		expected_reconciliation_run: target.expected_reconciliation_run,
		expected_generation: target.expected_generation,
		...(target.preflight ? { preflight: target.preflight } : {}),
		confirmed: true,
	}
}

export function isPreflightFactsChanged(error) {
	return String(error || '').includes('preflight facts changed')
}

export function canEditDesired(room) {
	return ['suspended', 'starting', 'active'].includes(room?.lifecycle)
}

export function canObserve(room) {
	return room?.lifecycle === 'active'
}

export function visibleRooms(rooms, hiddenRooms) {
	return Array.from(rooms).filter(room => !hiddenRooms.has(room.room_name))
}

export function roomNameIncludes(roomName, pattern) {
	return String(roomName).toLocaleLowerCase().includes(String(pattern).toLocaleLowerCase())
}

export function meetingDateKey(meeting) {
	const date = new Date(meeting?.scheduled_start || '')
	if (Number.isNaN(date.getTime())) return ''
	const pad = value => String(value).padStart(2, '0')
	return `${date.getFullYear()}-${pad(date.getMonth() + 1)}-${pad(date.getDate())}`
}

export function availableMeetingDates(meetingsByRoom) {
	const dates = new Set()
	for (const meetings of meetingsByRoom.values()) {
		for (const meeting of meetings || []) {
			const date = meetingDateKey(meeting)
			if (date) dates.add(date)
		}
	}
	return Array.from(dates).sort()
}

export function meetingsForDate(meetings, dateKey) {
	return (meetings || []).filter(meeting => meetingDateKey(meeting) === dateKey)
}

export const defaultAlertThresholdMinutes = 5
export const testClockParameter = 'alert_test_at'

// WHY: browser alert tests need a deterministic event date even when the host clock is
// outside the event day. The query value anchors a client-only clock, which then advances
// with real elapsed time instead of freezing the countdown at one instant.
export function parseTestClockStart(search) {
	const value = new URLSearchParams(search || '').get(testClockParameter)
	if (!value) return null
	const timestamp = new Date(value)
	return Number.isNaN(timestamp.getTime()) ? null : timestamp
}

export function createClientClock(search, realStartedAt = Date.now()) {
	const simulatedStart = parseTestClockStart(search)
	return {
		enabled: simulatedStart !== null,
		simulatedStart,
		now(realNow = Date.now()) {
			return simulatedStart ? new Date(simulatedStart.getTime() + realNow - realStartedAt) : new Date(realNow)
		},
	}
}

export function nextScheduledMeeting(meetings, currentMeetingID) {
	const ordered = (meetings || [])
		.filter(meeting => meeting?.scheduled_start)
		.slice()
		.sort((left, right) => {
			const startDifference = new Date(left.scheduled_start).getTime() - new Date(right.scheduled_start).getTime()
			if (startDifference !== 0) return startDifference
			const titleDifference = String(left.title || '').localeCompare(String(right.title || ''))
			return titleDifference || String(left.id || '').localeCompare(String(right.id || ''))
		})
	const currentIndex = ordered.findIndex(meeting => meeting.id === currentMeetingID)
	return currentIndex >= 0 ? ordered[currentIndex + 1] || null : null
}

export function evaluateScheduleAlert(
	room,
	meetings,
	now = new Date(),
	thresholdMinutes = defaultAlertThresholdMinutes,
) {
	if (
		room?.lifecycle !== 'active' ||
		room?.desired_status !== 'in_progress' ||
		!room?.desired_meeting_id ||
		!Array.isArray(room.active_meeting_ids) ||
		room.active_meeting_ids.length !== 1 ||
		room.active_meeting_ids[0] !== room.desired_meeting_id
	) {
		return null
	}
	const next = nextScheduledMeeting(meetings, room.desired_meeting_id)
	const start = new Date(next?.scheduled_start || '')
	if (!next || Number.isNaN(start.getTime())) return null
	const offsetMinutes = Number.isFinite(Number(room.schedule_offset_minutes))
		? Number(room.schedule_offset_minutes)
		: 0
	const threshold = Number.isFinite(Number(thresholdMinutes))
		? Number(thresholdMinutes)
		: defaultAlertThresholdMinutes
	const alertAt = new Date(start.getTime() + (offsetMinutes + threshold) * 60_000)
	if (now.getTime() < alertAt.getTime()) return null
	return {
		key: `${room.room_name}:${next.id}`,
		roomName: room.room_name,
		meetingID: next.id,
		meetingTitle: next.title || next.id,
		scheduledStart: start,
		adjustedStart: new Date(start.getTime() + offsetMinutes * 60_000),
		alertAt,
		overdueMinutes: Math.max(0, Math.floor((now.getTime() - alertAt.getTime()) / 60_000)),
	}
}
