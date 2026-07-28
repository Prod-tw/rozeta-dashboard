const state = {
	rooms: new Map(),
	selectedRoom: '',
	alerts: [],
	connected: false,
}

const roomsBody = document.getElementById('rooms-body')
const selectedRoomInput = document.getElementById('selected-room')
const selectedRoomLabel = document.getElementById('selected-room-label')
const targetMeetingInput = document.getElementById('target-meeting')
const roomDetails = document.getElementById('room-details')
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
		state.selectedRoom = rooms[0].room_name
		selectedRoomInput.value = state.selectedRoom
	}
	render()
}

function connectAdminSocket() {
	const ws = new WebSocket(`${location.origin.replace(/^http/, 'ws')}/ws/admin`)
	wsStatusNode.textContent = 'connecting'
	ws.addEventListener('open', () => {
		state.connected = true
		wsStatusNode.textContent = 'connected'
	})
	ws.addEventListener('close', () => {
		state.connected = false
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
			render()
			break
		case 'room_snapshot':
			if (message.room?.room_name) {
				state.rooms.set(message.room.room_name, message.room)
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
	state.alerts.unshift({ level, message, room, at: new Date() })
	state.alerts = state.alerts.slice(0, 5)
	renderAlerts()
}

function renderAlerts() {
	alertsNode.innerHTML = state.alerts.map((alert, index) => {
		const roomLabel = alert.room?.room_name ? `<strong>${escapeHtml(alert.room.room_name)}</strong>: ` : ''
		return `<button type="button" class="alert" data-alert-index="${index}" title="Click to dismiss">${roomLabel}${escapeHtml(alert.message)}</button>`
	}).join('')

	// Alerts used to stay visible until another render replaced them; clicking now dismisses only the chosen alert.
	alertsNode.querySelectorAll('[data-alert-index]').forEach(alertNode => {
		alertNode.addEventListener('click', () => dismissAlert(Number(alertNode.dataset.alertIndex)))
	})
}

function dismissAlert(index) {
	if (!Number.isInteger(index) || index < 0 || index >= state.alerts.length) {
		return
	}
	state.alerts.splice(index, 1)
	renderAlerts()
}

function render() {
	renderRooms()
	renderDetails()
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
		return `
			<tr class="${selected}" data-room="${escapeAttr(room.room_name)}">
				<td>${escapeHtml(room.room_name)}</td>
				<td><span class="status ${escapeHtml(room.status || 'ready')}">${escapeHtml(room.status || 'ready')}</span></td>
				<td>${escapeHtml(room.current_meeting_name || room.current_meeting_id || '—')}</td>
				<td>${formatHeartbeat(room.heartbeat_age_seconds)}</td>
			</tr>
		`
	}).join('')

	roomsBody.querySelectorAll('tr[data-room]').forEach(row => {
		row.addEventListener('click', () => {
			state.selectedRoom = row.dataset.room
			selectedRoomInput.value = state.selectedRoom
			render()
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
		`meeting name: ${room.current_meeting_name || '—'}`,
		`heartbeat age: ${formatHeartbeat(room.heartbeat_age_seconds)}`,
		`last error: ${room.last_error || '—'}`,
	].join('\n')
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
