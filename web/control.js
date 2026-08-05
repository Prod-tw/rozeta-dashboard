import {
	availableMeetingDates,
	canEditDesired,
	cloneReconciliationTargets,
	confirmationTargets,
	isCurrentVersion,
	meetingsForDate,
	reconcileAuthoritativeRooms,
	reconciliationActionFor,
	reconciliationRequestBody,
	reconciliationTargets,
	roomNameIncludes,
	shouldAcceptSnapshot,
	visibleRooms,
} from './state.js'

const state = {
	epoch: '',
	rooms: new Map(),
	meetings: new Map(),
	selectedDate: '',
	hiddenRooms: new Set(),
	selectedRoom: '',
	operationRoom: '',
	roomPickerDraft: new Set(),
	roomVisibilityConfigured: false,
	pendingAction: null,
	pendingReset: null,
	resettingRooms: new Set(),
	resetProbeRooms: new Set(),
	pendingAgenda: null,
	requestPending: false,
	connected: false,
	websocketSnapshotReceived: false,
	socketSequence: 0,
	messages: [],
	lastRoomStates: new Map(),
}

const roomGrid = document.getElementById('rooms-grid')
const selectedRoomTabs = document.getElementById('selected-room-tabs')
const operatorContent = document.getElementById('operator-content')
const operatorStatus = document.getElementById('operator-status')
const roomSearch = document.getElementById('room-search')
const eventDaySelect = document.getElementById('event-day-select')
const messageDrawer = document.getElementById('message-drawer')
const messageList = document.getElementById('message-list')
const messageCount = document.getElementById('message-count')
const actionDialog = document.getElementById('action-dialog')
const agendaDialog = document.getElementById('agenda-dialog')
const roomPickerDialog = document.getElementById('room-picker-dialog')

const visibilityStorageKey = 'coscup-caption.admin-room-visibility.v1'
const operationRoomStorageKey = 'coscup-caption.control-operation-room.v1'

document.getElementById('menu-button').addEventListener('click', toggleUtilityMenu)
document.getElementById('messages-button').addEventListener('click', openMessages)
document.getElementById('close-messages').addEventListener('click', closeMessages)
document.getElementById('drawer-backdrop').addEventListener('click', closeMessages)
document.getElementById('logout-btn').addEventListener('click', () => void logout())
document.getElementById('choose-rooms-btn').addEventListener('click', openRoomPicker)
document.getElementById('room-picker-cancel').addEventListener('click', () => roomPickerDialog.close())
document.getElementById('room-picker-apply').addEventListener('click', applyRoomPicker)
document.getElementById('room-picker-search').addEventListener('input', renderRoomPicker)
document.getElementById('room-picker-form').addEventListener('submit', event => event.preventDefault())
document.getElementById('action-confirm').addEventListener('click', () => void confirmRoomAction())
document.getElementById('agenda-confirm').addEventListener('click', () => void confirmAgenda())
document.getElementById('start-all').addEventListener('click', () => void openBulkAction('start'))
document.getElementById('stop-all').addEventListener('click', () => void openBulkAction('stop'))
document.getElementById('force-stop-all').addEventListener('click', () => void openBulkAction('force-stop'))
actionDialog.addEventListener('close', () => {
	state.pendingAction = null
	state.pendingReset = null
})
roomSearch.addEventListener('input', renderRooms)
eventDaySelect.addEventListener('change', () => {
	state.selectedDate = eventDaySelect.value
	render()
})

function apiFetch(url, options, acceptedStatuses = []) {
	return fetch(url, options).then(async response => {
		if (response.status === 401) {
			window.location.assign('/login')
			const error = new Error('登入狀態已失效。')
			error.authentication = true
			throw error
		}
		if (!response.ok && !acceptedStatuses.includes(response.status)) {
			const body = await response.json().catch(() => ({}))
			throw new Error(body.error || `伺服器回傳 ${response.status}。`)
		}
		return response
	})
}

async function logout() {
	try {
		await apiFetch('/api/logout', { method: 'POST' })
		window.location.assign('/login')
	} catch (error) {
		if (!error.authentication) showToast(error.message || String(error))
	}
}

async function loadRooms() {
	const response = await apiFetch('/api/rooms')
	const body = await response.json()
	applyRooms(body.rooms || [], body.epoch, 'http')
	ensureOperationRoom()
	render()
	void probeResetReadiness()
}

async function probeResetReadiness() {
	for (const room of getVisibleRooms()) {
		if (room.lifecycle !== 'suspended' || room.reset_ready || state.resetProbeRooms.has(room.room_name)) continue
		state.resetProbeRooms.add(room.room_name)
		try {
			const response = await apiFetch(
				`/api/rooms/${encodeURIComponent(room.room_name)}/reset-ready/preflight`,
				{
					method: 'POST',
					headers: { 'content-type': 'application/json' },
					body: JSON.stringify({
						epoch: state.epoch,
						expected_reconciliation_run: Number(room.reconciliation_run || 0),
						expected_generation: Number(room.generation || 0),
					}),
				},
				[409],
			)
			const body = await response.json().catch(() => ({}))
			if (body.room) applyRoom(body.room)
		} catch (error) {
			if (!error.authentication) showToast(error.message || String(error))
		} finally {
			state.resetProbeRooms.delete(room.room_name)
		}
	}
	render()
}

function applyRooms(rooms, epoch, source) {
	if (!Array.isArray(rooms) || !epoch) return false
	if (!shouldAcceptSnapshot(state.epoch, epoch, source, state.websocketSnapshotReceived)) return false
	const epochChanged = state.epoch !== '' && state.epoch !== epoch
	const currentRooms = epochChanged ? new Map() : state.rooms
	const currentMeetings = epochChanged ? new Map() : state.meetings
	const reconciled = reconcileAuthoritativeRooms(currentRooms, currentMeetings, rooms)
	state.epoch = epoch
	state.rooms = reconciled.rooms
	state.meetings = reconciled.meetings
	ensureDefaultRoomSelection()
	if (source === 'websocket') state.websocketSnapshotReceived = true
	if (epochChanged) {
		state.pendingAction = null
		state.pendingAgenda = null
		if (actionDialog.open) actionDialog.close()
		if (agendaDialog.open) agendaDialog.close()
	}
	for (const room of rooms) recordRoomChanges(room)
	ensureOperationRoom()
	return true
}

function applyRoom(room) {
	if (!room?.room_name || !room.epoch) return false
	if (state.epoch && state.epoch !== room.epoch) return false
	const current = state.rooms.get(room.room_name)
	if (current && !isCurrentVersion(room, current)) return false
	state.rooms.set(room.room_name, room)
	if (Array.isArray(room.meetings)) state.meetings.set(room.room_name, room.meetings)
	recordRoomChanges(room)
	ensureOperationRoom()
	return true
}

function recordRoomChanges(room) {
	const previous = state.lastRoomStates.get(room.room_name)
	const next = `${room.lifecycle}|${room.desired_meeting_id || ''}|${room.summary || ''}`
	state.lastRoomStates.set(room.room_name, next)
	if (!previous || previous === next) return
	const status = translateLifecycle(room.lifecycle)
	addMessage(
		room.last_error ? 'error' : 'info',
		room.room_name,
		`${status}：${room.desired_meeting_id || '尚未設定議程'}`,
	)
}

function ensureOperationRoom() {
	const visible = getVisibleRooms()
	if (state.operationRoom && visible.some(room => room.room_name === state.operationRoom)) return
	const saved = window.localStorage.getItem(operationRoomStorageKey)
	const savedRoom = visible.find(room => room.room_name === saved)
	state.operationRoom = savedRoom?.room_name || visible[0]?.room_name || ''
	if (state.operationRoom) window.localStorage.setItem(operationRoomStorageKey, state.operationRoom)
}

function render() {
	renderEventDayPicker()
	renderSelectedRooms()
	renderRooms()
	renderOperator()
	renderMessages()
	updateConnectionState()
}

function renderSelectedRooms() {
	const rooms = getVisibleRooms()
	selectedRoomTabs.innerHTML = rooms.length
		? rooms
				.map(
					room =>
						`<button type="button" class="room-tab ${room.room_name === state.operationRoom ? 'selected' : ''}" data-operation-room="${escapeAttr(room.room_name)}">${escapeHtml(room.room_name)}</button>`,
				)
				.join('')
		: '<div class="empty-state"><strong>尚未選擇教室</strong><span>請選擇要管理的教室。</span><button type="button" class="primary-button" data-empty-picker>選擇教室</button></div>'
	selectedRoomTabs.querySelector('[data-empty-picker]')?.addEventListener('click', openRoomPicker)
	selectedRoomTabs
		.querySelectorAll('[data-operation-room]')
		.forEach(button => button.addEventListener('click', () => selectOperationRoom(button.dataset.operationRoom)))
}

function ensureDefaultRoomSelection() {
	if (state.roomVisibilityConfigured || state.rooms.size === 0) return
	// WHY: the first visit previously showed every room, which made the new scope selector look pre-populated.
	// Defaulting all rooms to unchecked makes the operator intentionally choose the working scope; saved preferences
	// continue to control later visits.
	state.hiddenRooms = new Set(state.rooms.keys())
	state.roomVisibilityConfigured = true
	try {
		window.localStorage.setItem(
			visibilityStorageKey,
			JSON.stringify({ version: 1, hiddenRooms: [...state.hiddenRooms].sort() }),
		)
	} catch {
		showToast('無法儲存教室選擇，重新整理後可能會遺失。')
	}
}

function renderRooms() {
	const pattern = roomSearch.value.trim()
	const rooms = getVisibleRooms().filter(room => !pattern || roomNameIncludes(room.room_name, pattern))
	if (!rooms.length) {
		roomGrid.innerHTML =
			'<div class="empty-state"><strong>找不到符合的教室</strong><span>請調整搜尋名稱，或從「選擇教室」更新工作範圍。</span></div>'
		return
	}
	roomGrid.innerHTML = rooms.map(roomCardMarkup).join('')
	roomGrid
		.querySelectorAll('[data-room-action]')
		.forEach(button =>
			button.addEventListener(
				'click',
				() => void openRoomAction(button.dataset.roomAction, button.dataset.roomName),
			),
		)
	roomGrid
		.querySelectorAll('[data-room-reset]')
		.forEach(button => button.addEventListener('click', () => void openRoomReset(button.dataset.roomReset)))
}

function roomCardMarkup(room) {
	const action = reconciliationActionFor(room)
	const actionLabel = { start: '開始教室', stop: '停止教室', 'force-stop': '強制停止' }[action]
	const actionClass = action === 'force-stop' ? 'danger-button' : 'primary-button'
	const resetVisible = room.lifecycle === 'suspended'
	const resetDisabled = room.resetting || state.resettingRooms.has(room.room_name) || !room.reset_ready
	return `<article class="room-card">
		<div class="room-card-header"><div><h3>${escapeHtml(room.room_name)}</h3><p class="room-meta">目前議程：${escapeHtml(room.desired_meeting_id ? meetingTitle(room.room_name, room.desired_meeting_id) : '尚未設定')}</p></div><span class="status-label ${statusClass(room)}">${escapeHtml(translateLifecycle(room.lifecycle))}</span></div>
		<div class="room-card-footer"><div><p class="room-meta">${escapeHtml(room.summary || '等待伺服器狀態')}</p>${room.last_error ? `<p class="room-meta error-copy">${escapeHtml(room.last_error)}</p>` : ''}</div><div class="room-card-actions">${action ? `<button type="button" class="room-action ${actionClass}" data-room-action="${action}" data-room-name="${escapeAttr(room.room_name)}" ${state.requestPending || !state.connected ? 'disabled' : ''}>${actionLabel}</button>` : '<span class="room-meta">處理中</span>'}${resetVisible ? `<button type="button" class="room-action danger-button" data-room-reset="${escapeAttr(room.room_name)}" ${resetDisabled ? 'disabled' : ''}>${state.resettingRooms.has(room.room_name) ? '重置中' : '重置 Ready'}</button>` : ''}</div></div>
	</article>`
}

function renderOperator() {
	const room = state.rooms.get(state.operationRoom)
	if (!room) {
		operatorStatus.textContent = '尚未選擇'
		operatorContent.innerHTML =
			'<div class="empty-state"><strong>請先選擇教室</strong><span>選擇教室後，可以在這裡切換目前議程。</span><button type="button" class="primary-button" data-empty-picker>選擇教室</button></div>'
		operatorContent.querySelector('[data-empty-picker]')?.addEventListener('click', openRoomPicker)
		return
	}
	operatorStatus.textContent = translateLifecycle(room.lifecycle)
	const meetings = meetingsForDate(state.meetings.get(room.room_name), state.selectedDate)
	const action = reconciliationActionFor(room)
	operatorContent.innerHTML = `<div class="operator-card">
		<div class="operator-card-header"><div><h3>${escapeHtml(room.room_name)}</h3><p class="room-meta">${escapeHtml(room.summary || '等待伺服器狀態')}</p></div><span class="status-label ${statusClass(room)}">${escapeHtml(translateLifecycle(room.lifecycle))}</span></div>
		<div class="agenda-summary"><span>目前議程</span><strong>${escapeHtml(room.desired_meeting_id ? meetingTitle(room.room_name, room.desired_meeting_id) : '尚未設定議程')}</strong><span>${escapeHtml(room.desired_meeting_id || '請從下方選擇議程')}</span></div>
		<div class="operator-actions"><button type="button" class="secondary-button" data-agenda-toggle ${state.requestPending || !state.connected ? 'disabled' : ''}>切換議程</button>${action ? `<button type="button" class="${action === 'force-stop' ? 'danger-button' : 'primary-button'}" data-room-action="${action}" data-room-name="${escapeAttr(room.room_name)}" ${state.requestPending || !state.connected ? 'disabled' : ''}>${action === 'start' ? '開始教室' : action === 'stop' ? '停止教室' : '強制停止教室'}</button>` : '<button type="button" disabled>處理中</button>'}</div>
		<div class="agenda-list" id="agenda-list" hidden>${meetings.length ? meetings.map(meetingMarkup).join('') : '<p class="room-meta">目前沒有可選擇的議程。</p>'}</div>
	</div>`
	operatorContent.querySelector('[data-agenda-toggle]').addEventListener('click', () => {
		const list = document.getElementById('agenda-list')
		list.hidden = !list.hidden
	})
	operatorContent
		.querySelectorAll('[data-room-action]')
		.forEach(button =>
			button.addEventListener(
				'click',
				() => void openRoomAction(button.dataset.roomAction, button.dataset.roomName),
			),
		)
	operatorContent
		.querySelectorAll('[data-agenda-id]')
		.forEach(button => button.addEventListener('click', () => openAgendaConfirmation(button.dataset.agendaId)))
}

function meetingMarkup(meeting) {
	const selected = meeting.id === state.rooms.get(state.operationRoom)?.desired_meeting_id
	return `<button type="button" class="agenda-choice ${selected ? 'selected' : ''}" data-agenda-id="${escapeAttr(meeting.id)}" ${state.requestPending || !state.connected ? 'disabled' : ''}><strong>${escapeHtml(meeting.title || meeting.id)}</strong><small>${escapeHtml(formatTime(meeting.start_time || meeting.starts_at))} · ${escapeHtml(translateMeetingStatus(meeting.status))}</small></button>`
}

async function openAgendaConfirmation(meetingID) {
	const room = state.rooms.get(state.operationRoom)
	const meeting = (state.meetings.get(state.operationRoom) || []).find(item => item.id === meetingID)
	if (!room || !meeting || !canEditDesired(room)) return
	state.pendingAgenda = { room, meeting }
	document.getElementById('agenda-confirmation').innerHTML =
		`<div><span>議程</span><strong>${escapeHtml(meeting.title || meeting.id)}</strong></div><div><span>開始時間</span><strong>${escapeHtml(formatTime(meeting.start_time || meeting.starts_at))}</strong></div>`
	agendaDialog.showModal()
}

async function confirmAgenda() {
	const pending = state.pendingAgenda
	agendaDialog.close()
	state.pendingAgenda = null
	if (!pending || state.requestPending) return
	state.requestPending = true
	render()
	try {
		const response = await apiFetch(
			`/api/rooms/${encodeURIComponent(pending.room.room_name)}/desired-state`,
			{
				method: 'PUT',
				headers: { 'content-type': 'application/json' },
				body: JSON.stringify({
					meeting_id: pending.meeting.id,
					epoch: state.epoch,
					expected_reconciliation_run: Number(pending.room.reconciliation_run || 0),
					expected_generation: Number(pending.room.generation || 0),
					confirm_destructive_resume: pending.meeting.status === 'completed',
				}),
			},
			[409, 422],
		)
		const body = await response.json().catch(() => ({}))
		if (response.status === 409) throw new Error(body.error || '教室狀態已更新，請重新確認。')
		if (response.status === 422) throw new Error(body.error || '這個議程目前無法切換。')
		applyRoom(body)
		addMessage('info', pending.room.room_name, `已選擇議程：${pending.meeting.title || pending.meeting.id}`)
	} catch (error) {
		showToast(error.message || String(error))
	} finally {
		state.requestPending = false
		render()
	}
}

async function openRoomAction(action, roomName) {
	const room = state.rooms.get(roomName)
	if (!room || state.requestPending || !state.connected) return
	const target = reconciliationTargets([room], action)[0]
	if (!target) return
	if (action === 'force-stop') {
		showActionDialog({ action, bulk: false, epoch: state.epoch, targets: [target], confirmedTargets: [target] }, [])
		return
	}
	state.requestPending = true
	render()
	try {
		const response = await apiFetch(
			`/api/rooms/${encodeURIComponent(roomName)}/reconciliation/${action}/preflight`,
			{
				method: 'POST',
				headers: { 'content-type': 'application/json' },
				body: JSON.stringify({
					epoch: state.epoch,
					expected_reconciliation_run: target.expected_reconciliation_run,
					expected_generation: target.expected_generation,
				}),
			},
			[409],
		)
		const body = await response.json().catch(() => ({}))
		if (response.status === 409) throw new Error(body.error || '教室狀態已改變，請重新操作。')
		applyRooms(body.rooms || [], body.epoch, 'http')
		showActionDialog(
			{
				action,
				bulk: false,
				epoch: state.epoch,
				targets: [target],
				confirmedTargets: confirmationTargets([target], body.results || [], action),
			},
			body.results || [],
		)
	} catch (error) {
		showToast(error.message || String(error))
	} finally {
		state.requestPending = false
		render()
	}
}

async function openBulkAction(action) {
	const targets = reconciliationTargets(getVisibleRooms(), action)
	if (!targets.length || state.requestPending || !state.connected) return
	if (action === 'force-stop') {
		showActionDialog({ action, bulk: true, epoch: state.epoch, targets, confirmedTargets: targets }, [])
		return
	}
	state.requestPending = true
	render()
	try {
		const response = await apiFetch(
			`/api/reconciliation/${action}/preflight`,
			{
				method: 'POST',
				headers: { 'content-type': 'application/json' },
				body: JSON.stringify({ epoch: state.epoch, rooms: targets }),
			},
			[409],
		)
		const body = await response.json().catch(() => ({}))
		if (response.status === 409) throw new Error(body.error || '選取的教室狀態已改變，請重新操作。')
		applyRooms(body.rooms || [], body.epoch, 'http')
		showActionDialog(
			{
				action,
				bulk: true,
				epoch: state.epoch,
				targets,
				confirmedTargets: confirmationTargets(targets, body.results || [], action),
			},
			body.results || [],
		)
	} catch (error) {
		showToast(error.message || String(error))
	} finally {
		state.requestPending = false
		render()
	}
}

function showActionDialog(intent, results) {
	state.pendingAction = {
		...intent,
		targets: cloneReconciliationTargets(intent.targets),
		confirmedTargets: cloneReconciliationTargets(intent.confirmedTargets),
	}
	const count = intent.confirmedTargets.length
	const label = intent.action === 'start' ? '開始教室' : intent.action === 'stop' ? '停止教室' : '強制停止教室'
	document.getElementById('action-dialog-title').textContent = intent.bulk ? `${label}（${count} 間）` : `${label}？`
	document.getElementById('action-dialog-copy').textContent =
		intent.action === 'force-stop'
			? '遠端結果可能未知。'
			: `請確認要${intent.action === 'start' ? '開始' : '停止'}以下教室。`
	document.getElementById('action-dialog-facts').innerHTML = intent.targets
		.map(target => {
			const room = state.rooms.get(target.room_name)
			const result = results.find(item => item.room_name === target.room_name)
			return `<div><span>教室</span><strong>${escapeHtml(target.room_name)}</strong></div><div><span>目前議程</span><strong>${escapeHtml(room?.desired_meeting_id ? meetingTitle(target.room_name, room.desired_meeting_id) : '尚未設定')}</strong></div>${result && !result.observable ? `<div class="error-copy"><span>狀態</span><strong>目前無法確認</strong></div>` : ''}`
		})
		.join('')
	const confirmButton = document.getElementById('action-confirm')
	confirmButton.textContent = `確認${label}`
	confirmButton.className = `primary-button ${intent.action === 'force-stop' ? 'danger-button' : ''}`
	confirmButton.disabled = count === 0
	actionDialog.showModal()
}

async function confirmRoomAction() {
	const reset = state.pendingReset
	if (reset) {
		actionDialog.close()
		state.pendingReset = null
		void sendRoomReset(reset)
		return
	}
	const intent = state.pendingAction
	actionDialog.close()
	state.pendingAction = null
	if (!intent || !intent.confirmedTargets.length || state.requestPending) return
	state.requestPending = true
	render()
	try {
		const url = intent.bulk
			? `/api/reconciliation/${intent.action}`
			: `/api/rooms/${encodeURIComponent(intent.confirmedTargets[0].room_name)}/reconciliation/${intent.action}`
		const response = await apiFetch(
			url,
			{
				method: 'POST',
				headers: { 'content-type': 'application/json' },
				body: JSON.stringify(reconciliationRequestBody(intent)),
			},
			[409],
		)
		const body = await response.json().catch(() => ({}))
		if (response.status === 409) throw new Error(body.error || '教室狀態已改變，請重新操作。')
		applyRooms(body.rooms || [], body.epoch, 'http')
		addMessage(
			'info',
			intent.bulk ? '批次操作' : intent.confirmedTargets[0].room_name,
			`${intent.action === 'start' ? '開始' : intent.action === 'stop' ? '停止' : '強制停止'}流程已送出`,
		)
	} catch (error) {
		showToast(error.message || String(error))
	} finally {
		state.requestPending = false
		render()
	}
}

async function openRoomReset(roomName) {
	const room = state.rooms.get(roomName)
	if (
		!room ||
		room.lifecycle !== 'suspended' ||
		!room.reset_ready ||
		room.resetting ||
		state.resettingRooms.has(roomName)
	)
		return
	try {
		const response = await apiFetch(
			`/api/rooms/${encodeURIComponent(roomName)}/reset-ready/preflight`,
			{
				method: 'POST',
				headers: { 'content-type': 'application/json' },
				body: JSON.stringify({
					epoch: state.epoch,
					expected_reconciliation_run: Number(room.reconciliation_run || 0),
					expected_generation: Number(room.generation || 0),
				}),
			},
			[409],
		)
		const body = await response.json().catch(() => ({}))
		if (body.room) applyRoom(body.room)
		if (response.status === 409) {
			showToast(body.error || '教室目前無法重置，請重新觀測。')
			return
		}
		const meetings = Array.isArray(body.meetings) ? body.meetings : []
		state.pendingReset = {
			roomName,
			epoch: state.epoch,
			expectedRun: Number(room.reconciliation_run || 0),
			expectedGeneration: Number(room.generation || 0),
			meetingIDs: meetings.map(meeting => meeting.meeting_id),
		}
		document.getElementById('action-dialog-kicker').textContent = '破壞性操作'
		document.getElementById('action-dialog-title').textContent = `重置 ${roomName} 的所有議程？`
		document.getElementById('action-dialog-copy').textContent =
			'重置會永久刪除 paused 與 completed 議程的逐字稿及翻譯；ready 議程不會變更。'
		document.getElementById('action-dialog-facts').innerHTML =
			meetings
				.map(
					meeting =>
						`<div><span>議程</span><strong>${escapeHtml(meeting.meeting_id)}</strong></div><div><span>狀態</span><strong>${escapeHtml(meeting.status)} / ${escapeHtml(meeting.action)}</strong></div>`,
				)
				.join('') || '<div><span>結果</span><strong>沒有需要重置的議程</strong></div>'
		const confirmButton = document.getElementById('action-confirm')
		confirmButton.textContent = '確認重置'
		confirmButton.className = 'primary-button danger-button'
		confirmButton.disabled = false
		actionDialog.showModal()
	} catch (error) {
		showToast(error.message || String(error))
	}
}

async function sendRoomReset(intent) {
	if (state.resettingRooms.has(intent.roomName)) return
	state.resettingRooms.add(intent.roomName)
	render()
	try {
		const response = await apiFetch(
			`/api/rooms/${encodeURIComponent(intent.roomName)}/reset-ready`,
			{
				method: 'POST',
				headers: { 'content-type': 'application/json' },
				body: JSON.stringify({
					epoch: intent.epoch,
					expected_reconciliation_run: intent.expectedRun,
					expected_generation: intent.expectedGeneration,
					meeting_ids: intent.meetingIDs,
					confirmed: true,
				}),
			},
			[409],
		)
		const body = await response.json().catch(() => ({}))
		if (body.room) applyRoom(body.room)
		if (response.status === 409) throw new Error(body.error || '教室狀態已改變，請重新操作。')
		const results = Array.isArray(body.results) ? body.results : []
		const failed = results.filter(result => result.outcome === 'failed')
		addMessage(
			failed.length ? 'error' : 'info',
			intent.roomName,
			`Ready 重置完成：${results.length - failed.length} 成功，${failed.length} 失敗`,
		)
		if (failed.length)
			showToast(failed.map(result => `${result.meeting_id}: ${result.error || '重置失敗'}`).join('；'))
	} catch (error) {
		showToast(error.message || String(error))
	} finally {
		state.resettingRooms.delete(intent.roomName)
		render()
	}
}

function openRoomPicker() {
	state.roomPickerDraft = new Set(state.hiddenRooms)
	document.getElementById('room-picker-search').value = ''
	renderRoomPicker()
	roomPickerDialog.showModal()
}

function renderRoomPicker() {
	const pattern = document.getElementById('room-picker-search').value.trim()
	const rooms = getSortedRooms().filter(room => !pattern || roomNameIncludes(room.room_name, pattern))
	const visibleCount = getSortedRooms().filter(room => !state.roomPickerDraft.has(room.room_name)).length
	document.getElementById('room-picker-count').textContent = `已選取 ${visibleCount} 間教室`
	document.getElementById('room-picker-options').innerHTML = rooms.length
		? rooms
				.map(
					room =>
						`<label class="picker-option"><input type="checkbox" data-picker-room="${escapeAttr(room.room_name)}" ${state.roomPickerDraft.has(room.room_name) ? '' : 'checked'} /><span>${escapeHtml(room.room_name)}</span></label>`,
				)
				.join('')
		: '<p class="room-meta">沒有符合的教室。</p>'
	document.querySelectorAll('[data-picker-room]').forEach(input =>
		input.addEventListener('change', () => {
			if (input.checked) state.roomPickerDraft.delete(input.dataset.pickerRoom)
			else state.roomPickerDraft.add(input.dataset.pickerRoom)
			renderRoomPicker()
		}),
	)
}

function applyRoomPicker() {
	state.hiddenRooms = new Set(state.roomPickerDraft)
	try {
		window.localStorage.setItem(
			visibilityStorageKey,
			JSON.stringify({ version: 1, hiddenRooms: [...state.hiddenRooms].sort() }),
		)
	} catch {
		showToast('無法儲存教室選擇，重新整理後可能會遺失。')
	}
	roomPickerDialog.close()
	ensureOperationRoom()
	render()
}

function selectOperationRoom(roomName) {
	if (!state.rooms.has(roomName) || state.hiddenRooms.has(roomName)) return
	state.operationRoom = roomName
	try {
		window.localStorage.setItem(operationRoomStorageKey, roomName)
	} catch {
		// The selection still works for this session when browser storage is unavailable.
	}
	void loadMeetings(roomName)
	render()
}

async function loadMeetings(roomName) {
	try {
		const response = await apiFetch(`/api/rooms/${encodeURIComponent(roomName)}/meetings`)
		const body = await response.json()
		if (body.epoch === state.epoch) state.meetings.set(roomName, body.meetings || [])
		render()
	} catch (error) {
		if (!error.authentication) showToast(error.message || String(error))
	}
}

function toggleUtilityMenu() {
	const button = document.getElementById('menu-button')
	const menu = document.getElementById('utility-menu')
	const open = menu.hidden
	menu.hidden = !open
	button.setAttribute('aria-expanded', String(open))
}

function openMessages() {
	document.getElementById('utility-menu').hidden = true
	document.getElementById('menu-button').setAttribute('aria-expanded', 'false')
	messageDrawer.setAttribute('aria-hidden', 'false')
	document.getElementById('drawer-backdrop').hidden = false
	state.messages = state.messages.map(message => ({ ...message, unread: false }))
	renderMessages()
}

function closeMessages() {
	messageDrawer.setAttribute('aria-hidden', 'true')
	document.getElementById('drawer-backdrop').hidden = true
}

function addMessage(level, roomName, text) {
	state.messages.unshift({ level, roomName, text, at: new Date(), unread: true })
	state.messages = state.messages.slice(0, 50)
	renderMessages()
}

function renderMessages() {
	messageList.innerHTML = state.messages.length
		? state.messages
				.map(
					message =>
						`<article class="message-item ${message.level === 'error' ? 'error' : ''}"><strong>${escapeHtml(message.text)}</strong><small>${escapeHtml(message.roomName)} · ${escapeHtml(formatTime(message.at))}</small></article>`,
				)
				.join('')
		: '<p class="room-meta">目前沒有伺服器訊息。</p>'
	const unread = state.messages.filter(message => message.unread).length
	messageCount.hidden = unread === 0
	messageCount.textContent = unread ? String(unread) : ''
}

function updateConnectionState() {
	const status = document.getElementById('ws-status')
	status.textContent = state.connected ? '已連線' : '連線中斷'
	status.className = `connection-pill ${state.connected ? 'connected' : 'disconnected'}`
}

function connectSocket() {
	const sequence = ++state.socketSequence
	const socket = new WebSocket(`${location.origin.replace(/^http/, 'ws')}/ws/admin`)
	socket.addEventListener('open', () => {
		if (sequence !== state.socketSequence) return
		state.connected = true
		render()
	})
	socket.addEventListener('close', () => {
		if (sequence !== state.socketSequence) return
		state.connected = false
		state.websocketSnapshotReceived = false
		render()
		window.setTimeout(() => {
			if (sequence === state.socketSequence) {
				void loadRooms().finally(connectSocket)
			}
		}, 2000)
	})
	socket.addEventListener('message', event => {
		if (sequence !== state.socketSequence) return
		try {
			const message = JSON.parse(event.data)
			if (message.type === 'snapshot') applyRooms(message.rooms || [], message.epoch, 'websocket')
			if (message.type === 'room_snapshot') applyRoom(message.room)
			render()
		} catch (error) {
			showToast(error.message || String(error))
		}
	})
}

function loadVisibility() {
	try {
		const stored = JSON.parse(window.localStorage.getItem(visibilityStorageKey) || 'null')
		if (stored?.version === 1 && Array.isArray(stored.hiddenRooms)) {
			state.hiddenRooms = new Set(stored.hiddenRooms)
			state.roomVisibilityConfigured = true
		}
	} catch {
		showToast('無法載入教室選擇，將顯示所有教室。')
	}
}

function getSortedRooms() {
	return Array.from(state.rooms.values()).sort((left, right) => left.room_name.localeCompare(right.room_name))
}

function getVisibleRooms() {
	return visibleRooms(getSortedRooms(), state.hiddenRooms)
}

function meetingTitle(roomName, meetingID) {
	return (state.meetings.get(roomName) || []).find(meeting => meeting.id === meetingID)?.title || meetingID
}

function statusClass(room) {
	if (room.last_error) return 'error'
	if (room.lifecycle === 'active') return 'active'
	if (room.lifecycle === 'starting' || room.lifecycle === 'stopping') return room.lifecycle
	return room.lifecycle || 'unknown'
}

function translateLifecycle(lifecycle) {
	return (
		{ active: '教室使用中', starting: '正在開始教室', stopping: '正在停止教室', suspended: '教室已停止' }[
			lifecycle
		] || '需要處理'
	)
}

function translateMeetingStatus(status) {
	return (
		{ ready: '準備中', paused: '已暫停', in_progress: '進行中', completed: '已完成' }[status] ||
		status ||
		'狀態未知'
	)
}

function formatTime(value) {
	if (!value) return '時間未知'
	const date = value instanceof Date ? value : new Date(value)
	if (Number.isNaN(date.getTime())) return '時間未知'
	return new Intl.DateTimeFormat('zh-TW', { dateStyle: 'short', timeStyle: 'short' }).format(date)
}

function renderEventDayPicker() {
	const dates = availableMeetingDates(state.meetings)
	if (!dates.includes(state.selectedDate)) {
		const today = dateKey(new Date())
		state.selectedDate = dates.includes(today) ? today : dates[0] || ''
	}
	eventDaySelect.disabled = dates.length === 0
	eventDaySelect.innerHTML = dates
		.map(date => `<option value="${date}">${escapeHtml(formatEventDate(date))}</option>`)
		.join('')
	eventDaySelect.value = state.selectedDate
}

function formatEventDate(dateKeyValue) {
	const date = new Date(`${dateKeyValue}T00:00:00`)
	if (Number.isNaN(date.getTime())) return dateKeyValue
	return new Intl.DateTimeFormat('zh-TW', { month: 'numeric', day: 'numeric', weekday: 'short' }).format(date)
}

function dateKey(date) {
	const pad = value => String(value).padStart(2, '0')
	return `${date.getFullYear()}-${pad(date.getMonth() + 1)}-${pad(date.getDate())}`
}

function showToast(message) {
	const toast = document.createElement('div')
	toast.className = 'toast'
	toast.textContent = message
	document.getElementById('toast-stack').append(toast)
	window.setTimeout(() => toast.remove(), 6000)
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

loadVisibility()
loadRooms()
	.then(() => {
		if (state.operationRoom) void loadMeetings(state.operationRoom)
		connectSocket()
	})
	.catch(error => {
		if (!error.authentication) showToast(error.message || String(error))
		connectSocket()
	})
