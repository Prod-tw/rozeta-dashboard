const state = {
	rooms: new Map(),
	roomMeetings: new Map(),
	selectedRoom: '',
	meetingsLoadingFor: '',
	alerts: [],
	alertTimers: new Map(),
	nextAlertId: 0,
}

const roomsBody = document.getElementById('rooms-body')
const selectedRoomInput = document.getElementById('selected-room')
const selectedRoomLabel = document.getElementById('selected-room-label')
const targetMeetingInput = document.getElementById('target-meeting')
const roomDetails = document.getElementById('room-details')
const roomMeetings = document.getElementById('room-meetings')
const meetingsStatus = document.getElementById('meetings-status')
const alertsNode = document.getElementById('alerts')
const wsStatusNode = document.getElementById('ws-status')

document.getElementById('refresh-btn').addEventListener('click', () => loadRooms())
document.querySelectorAll('[data-action]').forEach(button => {
	button.addEventListener('click', () => sendCommand(button.dataset.action))
})

selectedRoomInput.addEventListener('input', () => {
	state.selectedRoom = selectedRoomInput.value.trim()
	render()
})

targetMeetingInput.addEventListener('input', () => render())

async function loadRooms() {
	const response = await fetch('/api/rooms')
	const rooms = await response.json()
	state.rooms = new Map(rooms.map(room => [room.room_name, room]))
	if (!state.selectedRoom && rooms[0]) {
		selectRoom(rooms[0].room_name, true)
		return
	}
	render()
}

function selectRoom(roomName, loadMeetings = false) {
	state.selectedRoom = roomName.trim()
	selectedRoomInput.value = state.selectedRoom
	if (loadMeetings && state.selectedRoom) {
		state.meetingsLoadingFor = state.selectedRoom
	}
	render()
	if (loadMeetings && state.selectedRoom) {
		void loadRoomMeetings(state.selectedRoom)
	}
}

function normalizeRoomName(roomName) {
	return String(roomName || '').trim()
}

function resolveCurrentMeetingName(room) {
	// The admin snapshot only knows the meeting ID unless the backend has an explicit
	// name mapping. We now resolve the human-readable title from the loaded meeting
	// list first, then fall back to the backend mapping, and finally to the ID.
	const meetingId = String(room?.current_meeting_id || '').trim()
	const mappedMeetingName = String(room?.current_meeting_name || '').trim()
	if (mappedMeetingName) {
		return mappedMeetingName
	}
	if (!meetingId) {
		return '—'
	}

	const meetings = state.roomMeetings.get(room?.room_name || '') || []
	const meeting = meetings.find(item => String(item?.id || '').trim() === meetingId)
	const meetingTitle = String(meeting?.title || '').trim()
	return meetingTitle || meetingId
}

function clearAlertTimer(alertId) {
	const timer = state.alertTimers.get(alertId)
	if (timer) {
		window.clearTimeout(timer)
		state.alertTimers.delete(alertId)
	}
}

function removeAlert(alertId, rerender = true) {
	const index = state.alerts.findIndex(alert => alert.id === alertId)
	if (index < 0) {
		return false
	}
	clearAlertTimer(alertId)
	state.alerts.splice(index, 1)
	if (rerender) {
		renderAlerts()
	}
	return true
}

function removeErrorAlertsForRoom(roomName) {
	const normalizedRoomName = normalizeRoomName(roomName)
	if (!normalizedRoomName) {
		return false
	}

	const errorAlerts = state.alerts.filter(alert => alert.level === 'error' && alert.room_name === normalizedRoomName)
	if (!errorAlerts.length) {
		return false
	}

	for (const alert of errorAlerts) {
		removeAlert(alert.id, false)
	}
	return true
}

function scheduleInfoAlertDismiss(alert) {
	// Info alerts used to pile up in the shared stack, which made the admin page noisy.
	// The old behavior kept them until another render displaced them; the new behavior
	// expires these messages automatically after a short delay.
	const timer = window.setTimeout(() => {
		removeAlert(alert.id)
	}, 5000)
	state.alertTimers.set(alert.id, timer)
}

// The room list used to stop at the local agent snapshot. The admin panel now
// asks the backend for Rozeta meetings after selection so the goto picker stays
// in sync with the live server-side token lookup.
async function loadRoomMeetings(roomName) {
	const trimmedRoomName = roomName.trim()
	if (!trimmedRoomName) {
		return
	}

	state.meetingsLoadingFor = trimmedRoomName
	renderMeetingList()

	try {
		const response = await fetch(`/api/rooms/${encodeURIComponent(trimmedRoomName)}/meetings`)
		const body = await response.json().catch(() => null)
		if (!response.ok) {
			throw new Error(body?.error || 'meeting lookup failed')
		}
		state.roomMeetings.set(trimmedRoomName, body.meetings || [])
	} catch (error) {
		pushAlert('error', error instanceof Error ? error.message : String(error), { room_name: trimmedRoomName })
	} finally {
		if (state.meetingsLoadingFor === trimmedRoomName) {
			state.meetingsLoadingFor = ''
		}
		renderMeetingList()
	}
}

function connectAdminSocket() {
	const ws = new WebSocket(`${location.origin.replace(/^http/, 'ws')}/ws/admin`)
	wsStatusNode.textContent = 'connecting'
	ws.addEventListener('open', () => {
		wsStatusNode.textContent = 'connected'
	})
	ws.addEventListener('close', () => {
		wsStatusNode.textContent = 'disconnected'
		setTimeout(connectAdminSocket, 2000)
	})
	ws.addEventListener('message', event => {
		let message
		try {
			message = JSON.parse(event.data)
		} catch {
			return
		}
		handleMessage(message)
	})
	ws.addEventListener('error', () => {
		wsStatusNode.textContent = 'error'
	})
	window.adminSocket = ws
}

function handleMessage(message) {
	switch (message.type) {
		case 'snapshot':
			state.rooms = new Map((message.rooms || []).map(room => [room.room_name, room]))
			// Room-scoped errors used to linger even after the room had recovered, because
			// the old snapshot flow only refreshed the table. We now clear those stale error
			// alerts before rendering the recovered room state.
			for (const room of message.rooms || []) {
				if (room?.room_name && room.status !== 'lost') {
					removeErrorAlertsForRoom(room.room_name)
				}
			}
			render()
			break
		case 'room_snapshot':
			if (message.room?.room_name) {
				state.rooms.set(message.room.room_name, message.room)
				if (message.room.status !== 'lost') {
					removeErrorAlertsForRoom(message.room.room_name)
				}
				render()
			}
			break
		case 'alert':
			pushAlert(message.level || 'error', message.message || 'alert', message.room)
			if (message.room?.room_name) {
				state.rooms.set(message.room.room_name, message.room)
				render()
			}
			break
	}
}

function pushAlert(level, message, room) {
	const normalizedLevel = level === 'info' ? 'info' : 'error'
	const roomName = normalizeRoomName(room?.room_name)
	if (normalizedLevel === 'error' && roomName) {
		removeErrorAlertsForRoom(roomName)
	}

	const alert = {
		id: ++state.nextAlertId,
		level: normalizedLevel,
		message,
		room_name: roomName,
	}
	state.alerts.unshift(alert)
	if (normalizedLevel === 'info') {
		scheduleInfoAlertDismiss(alert)
	}
	renderAlerts()
}

function renderAlerts() {
	if (!state.alerts.length) {
		alertsNode.innerHTML = '<div class="alert-empty">No active alerts.</div>'
		return
	}

	const alerts = [
		...state.alerts.filter(alert => alert.level === 'error'),
		...state.alerts.filter(alert => alert.level === 'info'),
	]

	alertsNode.innerHTML = alerts.map(alert => {
		const roomLabel = alert.room_name ? `<span class="alert-room">${escapeHtml(alert.room_name)}</span>` : ''
		const dismissButton = alert.level === 'error'
			? `<button type="button" class="alert-dismiss" data-alert-dismiss="${alert.id}">Dismiss</button>`
			: ''
		const clickableAttrs = alert.level === 'error' ? ` role="button" tabindex="0" data-alert-id="${alert.id}"` : ''
		return `
			<article class="alert ${escapeHtml(alert.level)}"${clickableAttrs}>
				<div class="alert-copy">
					<span class="alert-level">${escapeHtml(alert.level)}</span>
					${roomLabel}
					<p>${escapeHtml(alert.message)}</p>
				</div>
				${dismissButton}
			</article>
		`
	}).join('')

	alertsNode.querySelectorAll('[data-alert-id]').forEach(alertNode => {
		alertNode.addEventListener('click', () => dismissAlert(Number(alertNode.dataset.alertId)))
		alertNode.addEventListener('keydown', event => {
			if (event.key === 'Enter' || event.key === ' ') {
				event.preventDefault()
				dismissAlert(Number(alertNode.dataset.alertId))
			}
		})
	})

	alertsNode.querySelectorAll('[data-alert-dismiss]').forEach(alertNode => {
		alertNode.addEventListener('click', event => {
			event.stopPropagation()
			dismissAlert(Number(alertNode.dataset.alertDismiss))
		})
	})
}

function dismissAlert(alertId) {
	if (!Number.isInteger(alertId)) {
		return
	}
	removeAlert(alertId)
}

function render() {
	renderRooms()
	renderDetails()
	renderMeetingList()
	renderAlerts()
}

function renderRooms() {
	const rooms = Array.from(state.rooms.values()).sort((a, b) => a.room_name.localeCompare(b.room_name))
	if (!rooms.length) {
		roomsBody.innerHTML = '<tr><td colspan="4">No rooms yet. Wait for agents to connect.</td></tr>'
		return
	}
	roomsBody.innerHTML = rooms.map(room => {
		const selected = room.room_name === state.selectedRoom ? 'selected' : ''
		const meetingReference = formatMeetingReference(room)
		return `
			<tr class="${selected}" data-room="${escapeAttr(room.room_name)}">
				<td>${escapeHtml(room.room_name)}</td>
				<td><span class="status ${escapeHtml(room.status || 'ready')}">${escapeHtml(room.status || 'ready')}</span></td>
				<td>${escapeHtml(meetingReference)}</td>
				<td>${formatHeartbeat(room.heartbeat_age_seconds)}</td>
			</tr>
		`
	}).join('')

	roomsBody.querySelectorAll('tr[data-room]').forEach(row => {
		row.addEventListener('click', () => {
			selectRoom(row.dataset.room, true)
		})
	})
}

function renderDetails() {
	const room = state.rooms.get(state.selectedRoom)
	selectedRoomLabel.textContent = room ? `Selected: ${room.room_name}` : 'No room selected'
	if (!room) {
		roomDetails.textContent = 'Select a room to see details.'
		return
	}
	roomDetails.textContent = [
		`room: ${room.room_name}`,
		`status: ${room.status || 'ready'}`,
		`meeting id: ${room.current_meeting_id || '—'}`,
		`meeting name: ${resolveCurrentMeetingName(room)}`,
		`heartbeat age: ${formatHeartbeat(room.heartbeat_age_seconds)}`,
		`last error: ${room.last_error || '—'}`,
	].join('\n')
}

function renderMeetingList() {
	const roomName = state.selectedRoom
	const room = state.rooms.get(roomName)
	if (!roomName || !room) {
		meetingsStatus.textContent = 'Select a room'
		roomMeetings.innerHTML = '<div class="meeting-empty">Select a room to load meetings.</div>'
		return
	}

	const meetings = state.roomMeetings.get(roomName) || []
	if (state.meetingsLoadingFor === roomName) {
		meetingsStatus.textContent = 'Loading meetings...'
		roomMeetings.innerHTML = '<div class="meeting-empty">Loading meetings from Rozeta.</div>'
		return
	}

	meetingsStatus.textContent = `${meetings.length} meetings`
	if (!meetings.length) {
		roomMeetings.innerHTML = '<div class="meeting-empty">No meetings found for this room.</div>'
		return
	}

	const targetMeetingId = targetMeetingInput.value.trim()
	roomMeetings.innerHTML = meetings.map(meeting => {
		const selected = meeting.id === targetMeetingId ? 'selected' : ''
		const meta = [meeting.id, meeting.status, meeting.source_language || '—', meeting.target_language || '—']
			.filter(Boolean)
			.join(' · ')
		return `
			<button type="button" class="meeting-item ${selected}" data-meeting-id="${escapeAttr(meeting.id)}">
				<span class="meeting-title">${escapeHtml(meeting.title || meeting.id)}</span>
				<span class="meeting-meta">${escapeHtml(meta)}</span>
			</button>
		`
	}).join('')

	roomMeetings.querySelectorAll('[data-meeting-id]').forEach(button => {
		button.addEventListener('click', () => {
			targetMeetingInput.value = button.dataset.meetingId
			render()
			// Meeting selection is quick feedback, so it now goes through the transient
			// info channel instead of the persistent error stack used for room failures.
			pushAlert('info', `meeting selected: ${button.dataset.meetingId}`, { room_name: roomName })
		})
	})
}

async function sendCommand(action) {
	const roomName = selectedRoomInput.value.trim() || state.selectedRoom
	const targetMeetingId = targetMeetingInput.value.trim()
	if (!roomName) {
		pushAlert('error', 'select a room first')
		return
	}
	const response = await fetch(`/api/rooms/${encodeURIComponent(roomName)}/commands`, {
		method: 'POST',
		headers: { 'content-type': 'application/json' },
		body: JSON.stringify({ action, target_meeting_id: targetMeetingId }),
	})
	if (!response.ok) {
		pushAlert('error', `command failed for ${roomName}`)
		return
	}
	pushAlert('info', `${action} sent to ${roomName}`)
	await loadRooms()
}

function formatHeartbeat(ageSeconds) {
	if (!Number.isFinite(ageSeconds) || ageSeconds <= 0) {
		return '—'
	}
	if (ageSeconds < 10) {
		return `${ageSeconds.toFixed(1)}s`
	}
	return `${Math.round(ageSeconds)}s`
}

function escapeHtml(value) {
	return String(value)
		.replaceAll('&', '&amp;')
		.replaceAll('<', '&lt;')
		.replaceAll('>', '&gt;')
		.replaceAll('"', '&quot;')
		.replaceAll("'", '&#39;')
}

function escapeAttr(value) {
	return escapeHtml(value)
}

loadRooms().catch(error => pushAlert('error', error.message))
connectAdminSocket()
