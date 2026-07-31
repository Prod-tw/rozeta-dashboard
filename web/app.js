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
const resumeDialog = document.getElementById('resume-dialog')
const resumeMeetingName = document.getElementById('resume-meeting-name')

document.getElementById('refresh-btn').addEventListener('click', () => {
	void loadRooms()
	if (state.selectedRoom) {
		void loadRoomMeetings(state.selectedRoom)
	}
})
document.getElementById('logout-btn').addEventListener('click', async () => {
	await fetch('/api/logout', { method: 'POST' })
	window.location.assign('/login')
})
document.querySelectorAll('[data-action]').forEach(button => {
	button.addEventListener('click', () => {
		if (button.dataset.action === 'resume') {
			openResumeConfirmation()
			return
		}
		void sendCommand(button.dataset.action)
	})
})
document.getElementById('resume-confirm').addEventListener('click', () => {
	resumeDialog.close()
	void sendCommand('resume')
})

targetMeetingInput.addEventListener('input', renderActions)

async function apiFetch(url, options) {
	const response = await fetch(url, options)
	if (response.status === 401) {
		window.location.assign('/login')
		throw new Error('authentication required')
	}
	return response
}

async function loadRooms() {
	const response = await apiFetch('/api/rooms')
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
	targetMeetingInput.value = ''
	render()
	if (loadMeetings && state.selectedRoom) {
		void loadRoomMeetings(state.selectedRoom)
	}
}

async function loadRoomMeetings(roomName) {
	const normalizedRoom = roomName.trim()
	if (!normalizedRoom) return
	state.meetingsLoadingFor = normalizedRoom
	renderMeetingList()
	try {
		const response = await apiFetch(`/api/rooms/${encodeURIComponent(normalizedRoom)}/meetings`)
		const body = await response.json().catch(() => null)
		if (!response.ok) {
			throw new Error(body?.error || 'meeting lookup failed')
		}
		state.roomMeetings.set(normalizedRoom, body.meetings || [])
	} catch (error) {
		pushAlert('error', error instanceof Error ? error.message : String(error), { room_name: normalizedRoom })
	} finally {
		if (state.meetingsLoadingFor === normalizedRoom) {
			state.meetingsLoadingFor = ''
		}
		render()
	}
}

function connectAdminSocket() {
	const socket = new WebSocket(`${location.origin.replace(/^http/, 'ws')}/ws/admin`)
	wsStatusNode.textContent = 'connecting'
	socket.addEventListener('open', () => {
		wsStatusNode.textContent = 'connected'
	})
	socket.addEventListener('close', () => {
		wsStatusNode.textContent = 'disconnected'
		void loadRooms()
			.catch(() => {})
			.finally(() => window.setTimeout(connectAdminSocket, 2000))
	})
	socket.addEventListener('error', () => {
		wsStatusNode.textContent = 'error'
	})
	socket.addEventListener('message', event => {
		let message
		try {
			message = JSON.parse(event.data)
		} catch {
			return
		}
		handleMessage(message)
	})
}

function handleMessage(message) {
	switch (message.type) {
		case 'snapshot':
			state.rooms = new Map((message.rooms || []).map(room => [room.room_name, room]))
			render()
			break
		case 'room_snapshot':
			if (message.room?.room_name) {
				state.rooms.set(message.room.room_name, message.room)
				render()
			}
			break
		case 'alert':
			pushAlert(message.level || 'error', message.message || 'command update', message.room)
			if (message.room?.room_name) {
				state.rooms.set(message.room.room_name, message.room)
				render()
				if (message.room.room_name === state.selectedRoom) {
					void loadRoomMeetings(state.selectedRoom)
				}
			}
			break
	}
}

function pushAlert(level, message, room) {
	const normalizedLevel = level === 'info' ? 'info' : 'error'
	const alert = {
		id: ++state.nextAlertId,
		level: normalizedLevel,
		message,
		room_name: String(room?.room_name || '').trim(),
	}
	state.alerts.unshift(alert)
	if (normalizedLevel === 'info') {
		const timer = window.setTimeout(() => removeAlert(alert.id), 5000)
		state.alertTimers.set(alert.id, timer)
	}
	renderAlerts()
}

function removeAlert(alertId) {
	const index = state.alerts.findIndex(alert => alert.id === alertId)
	if (index < 0) return
	const timer = state.alertTimers.get(alertId)
	if (timer) window.clearTimeout(timer)
	state.alertTimers.delete(alertId)
	state.alerts.splice(index, 1)
	renderAlerts()
}

function render() {
	renderRooms()
	renderDetails()
	renderMeetingList()
	renderActions()
	renderAlerts()
}

function renderRooms() {
	const rooms = Array.from(state.rooms.values()).sort((a, b) => a.room_name.localeCompare(b.room_name))
	if (!rooms.length) {
		roomsBody.innerHTML = '<tr><td colspan="5">No configured rooms.</td></tr>'
		return
	}
	roomsBody.innerHTML = rooms
		.map(room => {
			const selected = room.room_name === state.selectedRoom ? 'selected' : ''
			return `
				<tr class="${selected}" data-room="${escapeAttr(room.room_name)}">
					<td>${escapeHtml(room.room_name)}</td>
					<td><span class="status ${escapeAttr(room.status || 'unknown')}">${escapeHtml(room.status || 'unknown')}</span></td>
					<td><span class="status ${escapeAttr(room.api_status || 'syncing')}">${escapeHtml(room.api_status || 'syncing')}</span></td>
					<td>${escapeHtml(resolveCurrentMeetingName(room))}</td>
					<td>${formatTimestamp(room.last_synced_at)}</td>
				</tr>
			`
		})
		.join('')
	roomsBody.querySelectorAll('tr[data-room]').forEach(row => {
		row.addEventListener('click', () => selectRoom(row.dataset.room, true))
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
		`meeting status: ${room.status || 'unknown'}`,
		`API status: ${room.api_status || 'syncing'}`,
		`current meeting: ${resolveCurrentMeetingName(room)}`,
		`last sync: ${formatTimestamp(room.last_synced_at)}`,
		`last command: ${room.last_command_action || '—'} / ${room.last_command_result || '—'}`,
		`last error: ${room.last_error || '—'}`,
	].join('\n')
}

function renderMeetingList() {
	const roomName = state.selectedRoom
	if (!roomName || !state.rooms.has(roomName)) {
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
	const targetID = targetMeetingInput.value.trim()
	roomMeetings.innerHTML = meetings
		.map(meeting => {
			const selected = meeting.id === targetID ? 'selected' : ''
			const meta = [
				meeting.id,
				meeting.status,
				meeting.source_language || '—',
				meeting.target_language || '—',
			].join(' · ')
			return `
				<button type="button" class="meeting-item ${selected}" data-meeting-id="${escapeAttr(meeting.id)}">
					<span class="meeting-title">${escapeHtml(meeting.title || meeting.id)}</span>
					<span class="meeting-meta">${escapeHtml(meta)}</span>
				</button>
			`
		})
		.join('')
	roomMeetings.querySelectorAll('[data-meeting-id]').forEach(button => {
		button.addEventListener('click', () => {
			targetMeetingInput.value = button.dataset.meetingId
			renderMeetingList()
			renderActions()
		})
	})
}

function renderActions() {
	const room = state.rooms.get(state.selectedRoom)
	const selectedMeeting = getSelectedMeeting()
	const targetID = targetMeetingInput.value.trim()
	const pending = Boolean(room?.pending_command_id)
	const synced = room?.api_status === 'synced'
	const canGoto = Boolean(
		room && targetID && room.api_status !== 'syncing' && room.api_status !== 'authentication_error',
	)
	const canControl = Boolean(room && synced && room.current_meeting_id && room.status !== 'completed')
	const canResume = Boolean(room && synced && selectedMeeting?.status === 'completed')

	document.querySelectorAll('[data-action]').forEach(button => {
		const action = button.dataset.action
		button.classList.toggle('loading', pending && room.pending_command_action === action)
		if (pending) {
			button.disabled = true
		} else if (action === 'goto') {
			button.disabled = !canGoto
		} else if (action === 'resume') {
			button.disabled = !canResume
		} else {
			button.disabled = !canControl
		}
	})
}

function renderAlerts() {
	if (!state.alerts.length) {
		alertsNode.innerHTML = '<div class="alert-empty">No active alerts.</div>'
		return
	}
	alertsNode.innerHTML = state.alerts
		.map(
			alert => `
			<article class="alert ${escapeAttr(alert.level)}">
				<div class="alert-copy">
					<span class="alert-level">${escapeHtml(alert.level)}</span>
					${alert.room_name ? `<span class="alert-room">${escapeHtml(alert.room_name)}</span>` : ''}
					<p>${escapeHtml(alert.message)}</p>
				</div>
				${alert.level === 'error' ? `<button type="button" class="alert-dismiss" data-alert-dismiss="${alert.id}">Dismiss</button>` : ''}
			</article>
		`,
		)
		.join('')
	alertsNode.querySelectorAll('[data-alert-dismiss]').forEach(button => {
		button.addEventListener('click', () => removeAlert(Number(button.dataset.alertDismiss)))
	})
}

function openResumeConfirmation() {
	const meeting = getSelectedMeeting()
	if (!meeting || meeting.status !== 'completed') return
	resumeMeetingName.textContent = meeting.title || meeting.id
	resumeDialog.showModal()
}

async function sendCommand(action) {
	const roomName = state.selectedRoom
	const targetMeetingId = targetMeetingInput.value.trim()
	if (!roomName) {
		pushAlert('error', 'Select a room first')
		return
	}
	try {
		const response = await apiFetch(`/api/rooms/${encodeURIComponent(roomName)}/commands`, {
			method: 'POST',
			headers: { 'content-type': 'application/json' },
			body: JSON.stringify({ action, target_meeting_id: targetMeetingId }),
		})
		const body = await response.json().catch(() => null)
		if (!response.ok) {
			throw new Error(body?.error || `Command failed for ${roomName}`)
		}
		await loadRooms()
	} catch (error) {
		pushAlert('error', error instanceof Error ? error.message : String(error), { room_name: roomName })
	}
}

function getSelectedMeeting() {
	const targetID = targetMeetingInput.value.trim()
	return (state.roomMeetings.get(state.selectedRoom) || []).find(meeting => meeting.id === targetID) || null
}

function resolveCurrentMeetingName(room) {
	const meetingID = String(room?.current_meeting_id || '').trim()
	if (!meetingID) return '—'
	const meeting = (state.roomMeetings.get(room.room_name) || []).find(item => item.id === meetingID)
	return meeting?.title || room.current_meeting_name || meetingID
}

function formatTimestamp(value) {
	if (!value || value.startsWith('0001-')) return '—'
	const date = new Date(value)
	return Number.isNaN(date.getTime()) ? '—' : date.toLocaleTimeString()
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
