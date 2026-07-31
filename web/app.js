const state = {
	rooms: new Map(),
	roomMeetings: new Map(),
	selectedRoom: '',
	meetingsLoadingFor: '',
	// Room visibility used to follow the shared server snapshot. Keeping hidden room names separately makes the
	// display browser-local, preserves the preference across reloads, and leaves newly configured rooms visible.
	hiddenRooms: new Set(),
	roomPickerDraft: new Set(),
	alerts: [],
	alertTimers: new Map(),
	nextAlertId: 0,
}

const roomVisibilityStorageKey = 'coscup-caption.admin-room-visibility.v1'

const meetingStatusLabels = {
	unknown: '未知',
	ready: '可用',
	in_progress: '進行中',
	paused: '已暫停',
	completed: '已完成',
}
const apiStatusLabels = {
	syncing: '同步中',
	synced: '已同步',
	stale: '同步失敗',
	authentication_error: '驗證失敗',
}
const actionLabels = {
	goto: '切換會議',
	start: '開始',
	pause: '暫停',
	resume: '重設會議',
}
const commandResultLabels = {
	pending: '執行中',
	confirmed: '已確認',
	failed: '失敗',
	confirmation_timeout: '確認逾時',
	confirmed_late: '延遲確認成功',
}
const alertLevelLabels = {
	info: '資訊',
	error: '錯誤',
}
const knownErrorMessages = {
	'authentication required': '登入狀態已失效，請重新登入。',
	'meeting lookup failed': '無法取得會議清單。',
	'command update': '指令狀態已更新。',
	'unknown room': '找不到指定的房間。',
	'invalid command request': '指令內容格式不正確。',
	'unsupported action': '不支援這項操作。',
	'target meeting is required': '請指定會議 ID。',
	'failed to verify completed meeting': '無法確認會議是否已完成。',
	'only completed meetings can be resumed': '只有已完成的會議可以重設。',
	'room already has a pending command': '這個房間已有指令正在執行。',
	'room meeting state is not ready': '房間的會議狀態尚未就緒。',
	'current meeting is unknown; send goto first': '無法判斷目前會議，請先執行「切換會議」。',
	'command confirmation timed out': '等待指令結果逾時。',
	'current goto meeting was not found in Rozeta': 'Rozeta 中找不到目前切換的會議。',
	'multiple in-progress meetings; send goto first': '有多場進行中的會議，請先執行「切換會議」。',
	'multiple paused meetings; send goto first': '有多場已暫停的會議，請先執行「切換會議」。',
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
const roomVisibilitySummary = document.getElementById('room-visibility-summary')
const roomPickerDialog = document.getElementById('room-picker-dialog')
const roomPickerSearch = document.getElementById('room-picker-search')
const roomPickerCount = document.getElementById('room-picker-count')
const roomPickerResults = document.getElementById('room-picker-results')
const roomPickerOptions = document.getElementById('room-picker-options')

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
document.getElementById('choose-rooms-btn').addEventListener('click', openRoomPicker)
document.getElementById('show-room-results').addEventListener('click', () => setRoomPickerResultsVisible(true))
document.getElementById('show-only-room-results').addEventListener('click', showOnlyRoomPickerResults)
document.getElementById('hide-room-results').addEventListener('click', () => setRoomPickerResultsVisible(false))
document.getElementById('room-picker-cancel').addEventListener('click', () => roomPickerDialog.close())
document.getElementById('room-picker-apply').addEventListener('click', applyRoomPicker)
document.getElementById('room-picker-form').addEventListener('submit', event => event.preventDefault())

targetMeetingInput.addEventListener('input', renderActions)
roomPickerSearch.addEventListener('input', renderRoomPicker)

function loadRoomVisibility() {
	try {
		const stored = window.localStorage.getItem(roomVisibilityStorageKey)
		if (!stored) return
		const preference = JSON.parse(stored)
		if (preference?.version !== 1 || !Array.isArray(preference.hiddenRooms)) return
		state.hiddenRooms = new Set(
			preference.hiddenRooms.map(roomName => String(roomName).trim()).filter(roomName => roomName),
		)
	} catch {
		pushAlert('error', '無法載入房間顯示設定，將顯示所有房間。')
	}
}

function saveRoomVisibility() {
	try {
		window.localStorage.setItem(
			roomVisibilityStorageKey,
			JSON.stringify({ version: 1, hiddenRooms: Array.from(state.hiddenRooms).sort() }),
		)
	} catch {
		pushAlert('error', '無法儲存房間顯示設定，重新整理後可能會遺失這次選擇。')
	}
}

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
	const firstVisibleRoom = rooms.find(room => !state.hiddenRooms.has(room.room_name))
	if (!state.selectedRoom && firstVisibleRoom) {
		selectRoom(firstVisibleRoom.room_name, true)
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
		pushAlert('error', localizeError(error instanceof Error ? error.message : String(error)), {
			room_name: normalizedRoom,
		})
	} finally {
		if (state.meetingsLoadingFor === normalizedRoom) {
			state.meetingsLoadingFor = ''
		}
		render()
	}
}

function connectAdminSocket() {
	const socket = new WebSocket(`${location.origin.replace(/^http/, 'ws')}/ws/admin`)
	wsStatusNode.textContent = '連線中'
	socket.addEventListener('open', () => {
		wsStatusNode.textContent = '已連線'
	})
	socket.addEventListener('close', () => {
		wsStatusNode.textContent = '已中斷，正在重新連線'
		void loadRooms()
			.catch(() => {})
			.finally(() => window.setTimeout(connectAdminSocket, 2000))
	})
	socket.addEventListener('error', () => {
		wsStatusNode.textContent = '連線錯誤'
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
			pushAlert(message.level || 'error', localizeError(message.message || 'command update'), message.room)
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
	if (roomPickerDialog.open) renderRoomPicker()
}

function renderRooms() {
	const allRooms = getSortedRooms()
	const rooms = allRooms.filter(room => !state.hiddenRooms.has(room.room_name))
	roomVisibilitySummary.textContent = `顯示 ${rooms.length} / ${allRooms.length} 個房間`
	if (!rooms.length) {
		const message = allRooms.length
			? '目前沒有選擇要顯示的房間，請使用「選擇房間」更新清單。'
			: '尚未設定任何房間。'
		roomsBody.innerHTML = `<tr><td colspan="5">${message}</td></tr>`
		return
	}
	roomsBody.innerHTML = rooms
		.map(room => {
			const selected = room.room_name === state.selectedRoom ? 'selected' : ''
			return `
				<tr class="${selected}" data-room="${escapeAttr(room.room_name)}" tabindex="0" aria-selected="${selected ? 'true' : 'false'}" aria-label="選擇 ${escapeAttr(room.room_name)} 並載入會議清單" data-tooltip="選擇 ${escapeAttr(room.room_name)} 並載入會議清單。">
					<td>${escapeHtml(room.room_name)}</td>
					<td><span class="status ${escapeAttr(room.status || 'unknown')}">${escapeHtml(labelFor(meetingStatusLabels, room.status, 'unknown'))}</span></td>
					<td><span class="status ${escapeAttr(room.api_status || 'syncing')}">${escapeHtml(labelFor(apiStatusLabels, room.api_status, 'syncing'))}</span></td>
					<td>${escapeHtml(resolveCurrentMeetingName(room))}</td>
					<td>${formatTimestamp(room.last_synced_at)}</td>
				</tr>
			`
		})
		.join('')
	roomsBody.querySelectorAll('tr[data-room]').forEach(row => {
		row.addEventListener('click', () => selectRoom(row.dataset.room, true))
		row.addEventListener('keydown', event => {
			if (event.key !== 'Enter' && event.key !== ' ') return
			event.preventDefault()
			selectRoom(row.dataset.room, true)
		})
	})
}

function renderDetails() {
	const room = state.rooms.get(state.selectedRoom)
	selectedRoomLabel.textContent = room ? `已選擇：${room.room_name}` : '尚未選擇房間'
	if (!room) {
		roomDetails.textContent = '請選擇房間以查看詳細資訊。'
		return
	}
	roomDetails.textContent = [
		`會議狀態：${labelFor(meetingStatusLabels, room.status, 'unknown')}`,
		`API 狀態：${labelFor(apiStatusLabels, room.api_status, 'syncing')}`,
		`目前會議：${resolveCurrentMeetingName(room)}`,
		`上次同步：${formatTimestamp(room.last_synced_at)}`,
		`上次指令：${room.last_command_action ? labelFor(actionLabels, room.last_command_action) : '—'} / ${room.last_command_result ? labelFor(commandResultLabels, room.last_command_result) : '—'}`,
		`上次錯誤：${room.last_error ? localizeError(room.last_error) : '—'}`,
	].join('\n')
}

function renderMeetingList() {
	const roomName = state.selectedRoom
	if (!roomName || !state.rooms.has(roomName)) {
		meetingsStatus.textContent = '請選擇房間'
		roomMeetings.innerHTML = '<div class="meeting-empty">請選擇房間以載入會議。</div>'
		return
	}
	const meetings = state.roomMeetings.get(roomName) || []
	if (state.meetingsLoadingFor === roomName) {
		meetingsStatus.textContent = '正在載入會議…'
		roomMeetings.innerHTML = '<div class="meeting-empty">正在從 Rozeta 載入會議。</div>'
		return
	}
	meetingsStatus.textContent = `${meetings.length} 場會議`
	if (!meetings.length) {
		roomMeetings.innerHTML = '<div class="meeting-empty">這個房間沒有會議。</div>'
		return
	}
	const targetID = targetMeetingInput.value.trim()
	roomMeetings.innerHTML = meetings
		.map(meeting => {
			const selected = meeting.id === targetID ? 'selected' : ''
			const meta = [
				meeting.id,
				labelFor(meetingStatusLabels, meeting.status),
				meeting.source_language || '—',
				meeting.target_language || '—',
			].join(' · ')
			return `
				<button type="button" class="meeting-item ${selected}" data-meeting-id="${escapeAttr(meeting.id)}" data-tooltip="選擇這場會議，供「切換會議」或「重設會議」使用。">
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
		const tooltipAnchor = document.querySelector(`[data-action-tooltip="${action}"]`)
		const tooltip = getActionTooltip(action, room, selectedMeeting, targetID, pending)
		button.dataset.tooltip = tooltip
		tooltipAnchor.dataset.tooltip = tooltip
		// Disabled buttons previously could not receive keyboard focus, hiding why an action was unavailable. The wrapper
		// now enters the tab order only while disabled; enabled buttons remain the single interactive focus target.
		tooltipAnchor.tabIndex = button.disabled ? 0 : -1
	})
}

function renderAlerts() {
	const alerts = state.alerts.filter(alert => !alert.room_name || !state.hiddenRooms.has(alert.room_name))
	if (!alerts.length) {
		// The previous in-flow panel needed an empty-state label to explain its reserved space. The floating stack now
		// disappears completely when empty so it neither blocks controls nor leaves a non-actionable overlay behind.
		alertsNode.replaceChildren()
		return
	}
	alertsNode.innerHTML = alerts
		.map(
			alert => `
			<article class="alert ${escapeAttr(alert.level)}">
				<div class="alert-copy">
					<div class="alert-meta">
						<span class="alert-level">${escapeHtml(labelFor(alertLevelLabels, alert.level))}</span>
						${alert.room_name ? `<span class="alert-room">${escapeHtml(alert.room_name)}</span>` : ''}
					</div>
					<p>${escapeHtml(alert.message)}</p>
				</div>
				${alert.level === 'error' ? `<button type="button" class="alert-dismiss" data-alert-dismiss="${alert.id}" data-tooltip="關閉這則錯誤通知。">關閉</button>` : ''}
			</article>
		`,
		)
		.join('')
	alertsNode.querySelectorAll('[data-alert-dismiss]').forEach(button => {
		button.addEventListener('click', () => removeAlert(Number(button.dataset.alertDismiss)))
	})
}

function openRoomPicker() {
	state.roomPickerDraft = new Set(state.hiddenRooms)
	roomPickerSearch.value = ''
	renderRoomPicker()
	roomPickerDialog.showModal()
	roomPickerSearch.focus()
}

function renderRoomPicker() {
	const rooms = getRoomPickerResults()
	renderRoomPickerCount()
	roomPickerResults.textContent = `符合條件：${rooms.length} 個房間`
	if (!rooms.length) {
		roomPickerOptions.innerHTML = '<div class="room-picker-empty">沒有符合搜尋條件的房間。</div>'
		return
	}
	roomPickerOptions.innerHTML = rooms
		.map(
			room => `
				<label class="room-picker-option" data-tooltip="控制是否在房間表格顯示 ${escapeAttr(room.room_name)}。">
					<input type="checkbox" data-room-picker="${escapeAttr(room.room_name)}" data-tooltip="控制是否在房間表格顯示 ${escapeAttr(room.room_name)}。" ${state.roomPickerDraft.has(room.room_name) ? '' : 'checked'} />
					<span>${escapeHtml(room.room_name)}</span>
				</label>
			`,
		)
		.join('')
	roomPickerOptions.querySelectorAll('[data-room-picker]').forEach(checkbox => {
		checkbox.addEventListener('change', () => {
			if (checkbox.checked) {
				state.roomPickerDraft.delete(checkbox.dataset.roomPicker)
			} else {
				state.roomPickerDraft.add(checkbox.dataset.roomPicker)
			}
			renderRoomPickerCount()
		})
	})
}

function renderRoomPickerCount() {
	const visibleCount = getSortedRooms().filter(room => !state.roomPickerDraft.has(room.room_name)).length
	roomPickerCount.textContent = `已選擇 ${visibleCount} 個房間`
}

function setRoomPickerResultsVisible(visible) {
	getRoomPickerResults().forEach(room => {
		if (visible) {
			state.roomPickerDraft.delete(room.room_name)
		} else {
			state.roomPickerDraft.add(room.room_name)
		}
	})
	renderRoomPicker()
}

function showOnlyRoomPickerResults() {
	const matchingRooms = new Set(getRoomPickerResults().map(room => room.room_name))
	// The existing batch actions changed only matching rooms. Show Only also hides every current non-match while
	// retaining hidden entries for rooms absent from the server, so a temporarily removed room keeps its preference.
	getSortedRooms().forEach(room => {
		if (matchingRooms.has(room.room_name)) {
			state.roomPickerDraft.delete(room.room_name)
		} else {
			state.roomPickerDraft.add(room.room_name)
		}
	})
	renderRoomPicker()
}

function applyRoomPicker() {
	state.hiddenRooms = new Set(state.roomPickerDraft)
	saveRoomVisibility()
	roomPickerDialog.close()
	if (state.hiddenRooms.has(state.selectedRoom)) {
		selectRoom('')
		return
	}
	render()
}

function getSortedRooms() {
	return Array.from(state.rooms.values()).sort((a, b) => a.room_name.localeCompare(b.room_name))
}

function getRoomPickerResults() {
	const pattern = roomPickerSearch.value.trim()
	return getSortedRooms().filter(room => !pattern || roomNameMatchesPattern(room.room_name, pattern))
}

function roomNameMatchesPattern(roomName, pattern) {
	// Search previously treated every character literally. Only ? and * now act as wildcards; escaping all other
	// regular-expression syntax keeps the original literal substring behavior safe for room names and user input.
	const expression = Array.from(pattern, character => {
		if (character === '?') return '.'
		if (character === '*') return '.*'
		return character.replace(/[\\^$.*+?()[\]{}|]/gu, '\\$&')
	}).join('')
	return new RegExp(expression, 'iu').test(roomName)
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
		pushAlert('error', '請先選擇房間。')
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
			throw new Error(body?.error || `無法對 ${roomName} 執行指令。`)
		}
		await loadRooms()
	} catch (error) {
		pushAlert('error', localizeError(error instanceof Error ? error.message : String(error)), {
			room_name: roomName,
		})
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
	return Number.isNaN(date.getTime()) ? '—' : date.toLocaleTimeString('zh-TW')
}

function labelFor(labels, value, fallback = '') {
	const normalized = String(value || fallback)
	return labels[normalized] || normalized || '—'
}

function localizeError(message) {
	const normalized = String(message || '').trim()
	if (!normalized) return '發生未知錯誤。'
	if (knownErrorMessages[normalized]) return knownErrorMessages[normalized]
	if (/\p{Script=Han}/u.test(normalized)) return normalized

	const commandMatch = normalized.match(
		/^(goto|start|pause|resume) (pending|confirmed|failed|confirmation_timeout|confirmed_late) for (.+)$/u,
	)
	if (commandMatch) {
		return `${labelFor(actionLabels, commandMatch[1])}：${labelFor(commandResultLabels, commandMatch[2])}（${commandMatch[3]}）`
	}
	const lateMatch = normalized.match(/^(goto|start|pause|resume) confirmed late for (.+)$/u)
	if (lateMatch) return `${labelFor(actionLabels, lateMatch[1])}：延遲確認成功（${lateMatch[2]}）`

	const timeoutPrefix = 'command confirmation timed out: '
	if (normalized.startsWith(timeoutPrefix)) {
		return `等待指令結果逾時。技術資訊：${normalized.slice(timeoutPrefix.length)}`
	}
	return `系統或 Rozeta 回報錯誤。技術資訊：${normalized}`
}

function getActionTooltip(action, room, selectedMeeting, targetID, pending) {
	if (pending) return '這個房間已有指令正在執行，請等待完成。'
	if (!room) return '請先從房間表格選擇房間。'
	if (action === 'goto') {
		if (!targetID) return '請輸入會議 ID，或從會議清單選取一場會議。'
		if (room.api_status === 'syncing') return '房間仍在同步中，請稍候。'
		if (room.api_status === 'authentication_error') return 'Rozeta 驗證失敗，無法切換會議。'
		return '將指定會議設為這個房間的目前會議。'
	}
	if (action === 'resume') {
		if (!selectedMeeting) return '請從會議清單選取一場已完成的會議。'
		if (selectedMeeting.status !== 'completed') return '只有已完成的會議可以重設。'
		if (room.api_status !== 'synced') return '房間尚未完成同步，無法重設會議。'
		return '永久刪除已完成會議的逐字稿與翻譯，並重設為可用狀態。'
	}
	if (room.api_status !== 'synced') return '房間尚未完成同步，無法執行這項指令。'
	if (!room.current_meeting_id) return '無法判斷目前會議，請先執行「切換會議」。'
	if (room.status === 'completed') return '目前會議已完成，無法開始或暫停。'
	return action === 'start' ? '開始目前房間的目前會議。' : '暫停目前房間的目前會議。'
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

loadRoomVisibility()
loadRooms().catch(error => pushAlert('error', localizeError(error.message)))
connectAdminSocket()
