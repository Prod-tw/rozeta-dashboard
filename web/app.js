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

const state = {
	epoch: '',
	websocketSnapshotReceived: false,
	socketSequence: 0,
	rooms: new Map(),
	meetings: new Map(),
	selectedRoom: '',
	formGeneration: 0,
	formDirty: false,
	pendingDesired: null,
	pendingReconciliation: null,
	requestPending: false,
}

const roomsBody = document.getElementById('rooms-body')
const roomCount = document.getElementById('room-count')
const targetMeeting = document.getElementById('target-meeting')
const selectedRoomLabel = document.getElementById('selected-room-label')
const roomDetails = document.getElementById('room-details')
const roomMeetings = document.getElementById('room-meetings')
const meetingsStatus = document.getElementById('meetings-status')
const alerts = document.getElementById('alerts')
const refreshButton = document.getElementById('refresh-btn')
const applyDesiredButton = document.getElementById('apply-desired')
const rearmDesiredButton = document.getElementById('rearm-desired')
const desiredConfirmationDialog = document.getElementById('desired-confirmation-dialog')
const desiredConfirmationConfirm = document.getElementById('desired-confirmation-confirm')
const reconciliationDialog = document.getElementById('reconciliation-dialog')
const reconciliationConfirm = document.getElementById('reconciliation-confirm')
const startAllButton = document.getElementById('start-all')
const stopAllButton = document.getElementById('stop-all')
const forceStopAllButton = document.getElementById('force-stop-all')

document.getElementById('logout-btn').addEventListener('click', () => void logout())
refreshButton.addEventListener('click', () => {
	if (state.selectedRoom) void observe(state.selectedRoom)
})
applyDesiredButton.addEventListener('click', () => openDesiredConfirmation(false))
rearmDesiredButton.addEventListener('click', () => openDesiredConfirmation(true))
startAllButton.addEventListener('click', () => void beginBulkReconciliation('start'))
stopAllButton.addEventListener('click', () => void beginBulkReconciliation('stop'))
forceStopAllButton.addEventListener('click', () => void beginBulkReconciliation('force-stop'))
targetMeeting.addEventListener('input', () => (state.formDirty = true))
desiredConfirmationConfirm.addEventListener('click', () => {
	const intent = state.pendingDesired
	desiredConfirmationDialog.close()
	state.pendingDesired = null
	if (intent) void sendDesired(intent, true)
})
desiredConfirmationDialog.addEventListener('close', () => {
	state.pendingDesired = null
})
reconciliationConfirm.addEventListener('click', () => {
	const intent = state.pendingReconciliation
	reconciliationDialog.close()
	state.pendingReconciliation = null
	if (intent) void sendReconciliation(intent)
})
reconciliationDialog.addEventListener('close', () => {
	state.pendingReconciliation = null
})

async function apiFetch(url, options, acceptedStatuses = []) {
	const response = await fetch(url, options)
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
}

async function logout() {
	try {
		await apiFetch('/api/logout', { method: 'POST' })
		window.location.assign('/login')
	} catch (error) {
		if (!error.authentication) showAlert(error instanceof Error ? error.message : String(error))
	}
}

async function loadRooms() {
	const response = await apiFetch('/api/rooms')
	const body = await response.json()
	setRooms(body.rooms || [], body.epoch, 'http')
	if (!state.selectedRoom && state.rooms.size) void selectRoom(Array.from(state.rooms.keys()).sort()[0])
	render()
}

async function selectRoom(roomName) {
	state.selectedRoom = roomName
	syncDesiredForm(state.rooms.get(roomName))
	meetingsStatus.textContent = '載入會議中'
	render()
	try {
		const response = await apiFetch(`/api/rooms/${encodeURIComponent(roomName)}/meetings`)
		const body = await response.json()
		const current = state.rooms.get(roomName)
		if (body.epoch === state.epoch && isCurrentVersion(body, current))
			state.meetings.set(roomName, body.meetings || [])
	} catch (error) {
		showAlert(error instanceof Error ? error.message : String(error))
	}
	render()
}

function setRooms(rooms, epoch, source) {
	if (!Array.isArray(rooms)) return false
	if (!shouldAcceptSnapshot(state.epoch, epoch, source, state.websocketSnapshotReceived)) return false
	const epochChanged = state.epoch !== '' && state.epoch !== epoch
	const currentRooms = epochChanged ? new Map() : state.rooms
	const currentMeetings = epochChanged ? new Map() : state.meetings
	const reconciled = reconcileAuthoritativeRooms(currentRooms, currentMeetings, rooms)
	state.epoch = epoch
	state.rooms = reconciled.rooms
	state.meetings = reconciled.meetings
	if (epochChanged) {
		// WHY: revisions restart at zero after a process replacement. The previous UI kept dirty forms and confirmations
		// from the old process; clearing them prevents those stale fences from being submitted against the new epoch.
		state.formDirty = false
		closePendingDialogs()
	}
	if (!state.rooms.has(state.selectedRoom)) {
		state.selectedRoom = Array.from(state.rooms.keys()).sort()[0] || ''
		state.formDirty = false
		closePendingDialogs()
	}
	if (!state.selectedRoom) syncDesiredForm()
	else if (!state.formDirty) syncDesiredForm(state.rooms.get(state.selectedRoom))
	if (source === 'websocket') state.websocketSnapshotReceived = true
	return true
}

function setRoom(room, epoch = room?.epoch) {
	if (!room?.room_name || !epoch) return false
	if (state.epoch && epoch !== state.epoch) return false
	if (!state.epoch) state.epoch = epoch
	const current = state.rooms.get(room.room_name)
	if (current && !isCurrentVersion(room, current)) return false
	state.rooms.set(room.room_name, room)
	if (Array.isArray(room.meetings)) state.meetings.set(room.room_name, room.meetings)
	if (room.room_name === state.selectedRoom && !state.formDirty) syncDesiredForm(room)
	return true
}

function closePendingDialogs() {
	state.pendingDesired = null
	state.pendingReconciliation = null
	if (desiredConfirmationDialog.open) desiredConfirmationDialog.close()
	if (reconciliationDialog.open) reconciliationDialog.close()
}

function syncDesiredForm(room) {
	targetMeeting.value = room?.desired_meeting_id || ''
	state.formGeneration = Number(room?.generation || 0)
	state.formDirty = false
}

async function observe(roomName) {
	const room = state.rooms.get(roomName)
	if (!canObserve(room)) return
	try {
		await apiFetch(`/api/rooms/${encodeURIComponent(roomName)}/observe`, {
			method: 'POST',
			headers: { 'content-type': 'application/json' },
			body: JSON.stringify({
				epoch: state.epoch,
				expected_reconciliation_run: Number(room.reconciliation_run || 0),
				expected_generation: Number(room.generation || 0),
			}),
		})
	} catch (error) {
		showAlert(error instanceof Error ? error.message : String(error))
	}
}

function desiredIntent(rearm) {
	const room = state.rooms.get(state.selectedRoom)
	const meetingID = String(rearm ? room?.desired_meeting_id || '' : targetMeeting.value).trim()
	if (!room || !meetingID) {
		showAlert('請選擇房間與目標會議。')
		return null
	}
	if (!canEditDesired(room)) {
		showAlert('停止中的房間不能修改期望會議。')
		return null
	}
	return {
		roomName: room.room_name,
		meetingID,
		epoch: state.epoch,
		expectedRun: Number(room.reconciliation_run || 0),
		expectedGeneration: Number(room.generation || 0),
		rearm,
	}
}

function openDesiredConfirmation(rearm) {
	const intent = desiredIntent(rearm)
	if (!intent) return
	const selected = (state.meetings.get(intent.roomName) || []).find(meeting => meeting.id === intent.meetingID)
	if (!rearm && selected?.status !== 'completed') {
		void sendDesired(intent, false)
		return
	}
	showDesiredConfirmation(intent)
}

function showDesiredConfirmation(intent) {
	state.pendingDesired = { ...intent }
	document.getElementById('desired-confirmation-title').textContent = intent.rearm
		? '重新授權此會議一次 Resume？'
		: '把已完成的會議設為期望目標？'
	document.getElementById('desired-confirmation-copy').textContent =
		'自動 Resume 會永久刪除 Rozeta 既有逐字稿與翻譯。這個動作無法復原，且每個 generation 最多自動執行一次。'
	document.getElementById('desired-confirmation-target').textContent = `${intent.roomName} / ${intent.meetingID}`
	desiredConfirmationDialog.showModal()
}

async function sendDesired(intent, confirmDestructiveResume) {
	if (state.requestPending) return
	state.requestPending = true
	render()
	try {
		const response = await apiFetch(
			`/api/rooms/${encodeURIComponent(intent.roomName)}/desired-state`,
			{
				method: 'PUT',
				headers: { 'content-type': 'application/json' },
				body: JSON.stringify({
					meeting_id: intent.meetingID,
					epoch: intent.epoch,
					expected_reconciliation_run: intent.expectedRun,
					expected_generation: intent.expectedGeneration,
					confirm_destructive_resume: confirmDestructiveResume,
					rearm: intent.rearm,
				}),
			},
			[409, 422],
		)
		const body = await response.json().catch(() => ({}))
		if (response.status === 409) {
			if (body.room) setRoom(body.room)
			render()
			showAlert(body.error || '期望狀態已被其他管理員更新，請確認後重試。')
			return
		}
		if (response.status === 422) {
			if (!confirmDestructiveResume) showDesiredConfirmation(intent)
			else showAlert(body.error || '破壞性期望狀態更新失敗。')
			return
		}
		if (setRoom(body) && state.selectedRoom === intent.roomName) syncDesiredForm(body)
	} catch (error) {
		showAlert(error instanceof Error ? error.message : String(error))
	} finally {
		state.requestPending = false
		render()
	}
}

async function beginBulkReconciliation(action) {
	const targets = reconciliationTargets(state.rooms.values(), action)
	if (!targets.length) return
	await beginReconciliation({ action, targets, bulk: true, epoch: state.epoch })
}

async function beginRoomReconciliation(roomName) {
	const room = state.rooms.get(roomName)
	const action = reconciliationActionFor(room)
	if (!action) return
	const targets = reconciliationTargets([room], action)
	await beginReconciliation({ action, targets, bulk: false, epoch: state.epoch })
}

async function beginReconciliation(intent) {
	// WHY: live WebSocket snapshots may change while an administrator reviews the dialog. The previous flow rebuilt
	// targets from live state; this frozen copy preserves the exact browser target set and all optimistic fences.
	const frozen = { ...intent, targets: cloneReconciliationTargets(intent.targets) }
	if (intent.action === 'force-stop') {
		showReconciliationDialog({ ...frozen, confirmedTargets: frozen.targets }, [])
		return
	}
	await preflightReconciliation(frozen)
}

async function preflightReconciliation(intent) {
	if (state.requestPending) return
	state.requestPending = true
	render()
	try {
		const roomName = intent.targets[0].room_name
		const url = intent.bulk
			? `/api/reconciliation/${intent.action}/preflight`
			: `/api/rooms/${encodeURIComponent(roomName)}/reconciliation/${intent.action}/preflight`
		const requestBody = intent.bulk
			? { epoch: intent.epoch, rooms: intent.targets }
			: {
					epoch: intent.epoch,
					expected_reconciliation_run: intent.targets[0].expected_reconciliation_run,
					expected_generation: intent.targets[0].expected_generation,
				}
		const response = await apiFetch(
			url,
			{
				method: 'POST',
				headers: { 'content-type': 'application/json' },
				body: JSON.stringify(requestBody),
			},
			[409],
		)
		const body = await response.json().catch(() => ({}))
		if (response.status === 409) {
			applyAuthoritativeConflict(body, intent.epoch)
			showAlert(body.error || 'Preflight 的 optimistic fence 已失效，請重試。')
			return
		}
		applyAuthoritativeRooms(body, 'http')
		const results = Array.isArray(body.results) ? body.results : []
		showReconciliationDialog(
			{ ...intent, confirmedTargets: confirmationTargets(intent.targets, results, intent.action) },
			results,
		)
	} catch (error) {
		showAlert(error instanceof Error ? error.message : String(error))
	} finally {
		state.requestPending = false
		render()
	}
}

function showReconciliationDialog(intent, results) {
	const labels = {
		start: intent.bulk ? '開始可觀測的房間？' : '開始此房間？',
		stop: intent.bulk ? '停止可觀測的房間？' : '停止此房間？',
		'force-stop': intent.bulk ? '強制停止所有卡住的房間？' : '強制停止此房間？',
	}
	const descriptions = {
		start: '以下是剛完成的 fresh preflight。確認 Start 代表本 run 未來可能自動 Resume 一次；Resume 會永久刪除既有逐字稿與翻譯，即使目前狀態不是 completed 也有此風險。',
		stop: '以下是剛完成的 fresh preflight。確認後會 Pause 每個列出的 active meeting。',
		'force-stop':
			'Force-stop 不做正常 preflight，會放棄本機 run；已送到 Rozeta 的命令無法撤銷，遠端結果將標為未知。',
	}
	state.pendingReconciliation = {
		...intent,
		confirmedTargets: cloneReconciliationTargets(intent.confirmedTargets),
	}
	document.getElementById('reconciliation-dialog-title').textContent = labels[intent.action]
	document.getElementById('reconciliation-dialog-copy').textContent = descriptions[intent.action]
	document.getElementById('reconciliation-dialog-rooms').innerHTML = intent.targets
		.map(target => preflightResultMarkup(intent.action, target, results))
		.join('')
	reconciliationConfirm.disabled = intent.confirmedTargets.length === 0
	reconciliationConfirm.textContent =
		intent.action === 'force-stop' ? '確認強制停止' : `確認 ${intent.confirmedTargets.length} 個房間`
	reconciliationConfirm.classList.toggle(
		'danger-action',
		intent.action === 'start' ||
			intent.action === 'force-stop' ||
			results.some(result => result.destructive_resume),
	)
	reconciliationDialog.showModal()
}

function preflightResultMarkup(action, target, results) {
	const result = results.find(candidate => candidate.room_name === target.room_name)
	if (!result) {
		return `<article class="preflight-room high-risk"><strong>${escapeHtml(target.room_name)}</strong><span>未執行 preflight；遠端結果可能未知。</span></article>`
	}
	if (!result.observable) {
		return `<article class="preflight-room unobservable"><strong>${escapeHtml(target.room_name)}</strong><span>不可觀測，不會送出確認動作：${escapeHtml(result.error || '未知錯誤')}</span></article>`
	}
	const activeIDs = formatIDs(result.active_meeting_ids)
	if (action === 'stop') {
		return `<article class="preflight-room"><strong>${escapeHtml(target.room_name)}</strong><span>將 Pause：${escapeHtml(activeIDs)}</span></article>`
	}
	const destructive = result.destructive_resume
		? '<b>目前已 completed：Start 後可能立即自動 Resume，並永久刪除逐字稿與翻譯。</b>'
		: '<span>本 run 仍可能在未來自動 Resume 一次，屆時會永久刪除逐字稿與翻譯。</span>'
	return `<article class="preflight-room ${result.destructive_resume ? 'high-risk' : ''}"><strong>${escapeHtml(target.room_name)}</strong><span>Desired ${escapeHtml(result.desired_meeting_id || '未設定')} / ${escapeHtml(result.desired_status || 'unknown')}</span><span>目前 active：${escapeHtml(activeIDs)}</span>${destructive}</article>`
}

async function sendReconciliation(intent) {
	if (state.requestPending || !intent.confirmedTargets.length) return
	state.requestPending = true
	render()
	try {
		const roomName = intent.confirmedTargets[0].room_name
		const url = intent.bulk
			? `/api/reconciliation/${intent.action}`
			: `/api/rooms/${encodeURIComponent(roomName)}/reconciliation/${intent.action}`
		const body = reconciliationRequestBody(intent)
		const response = await apiFetch(
			url,
			{
				method: 'POST',
				headers: { 'content-type': 'application/json' },
				body: JSON.stringify(body),
			},
			[409],
		)
		const responseBody = await response.json().catch(() => ({}))
		if (response.status === 409) {
			applyAuthoritativeConflict(responseBody, intent.epoch)
			showAlert(
				isPreflightFactsChanged(responseBody.error)
					? 'Preflight facts 已改變，未執行操作。畫面已更新，請重新執行 preflight 並再次確認。'
					: responseBody.error || 'Lifecycle 已被其他管理員更新，請確認目前狀態。',
			)
			return
		}
		applyAuthoritativeRooms(responseBody, 'http')
		const failed = Array.isArray(responseBody.results)
			? responseBody.results.filter(result => !result.applied || result.error)
			: []
		if (failed.length)
			showAlert(failed.map(result => `${result.room_name}: ${result.error || '未套用'}`).join('；'))
	} catch (error) {
		showAlert(error instanceof Error ? error.message : String(error))
	} finally {
		state.requestPending = false
		render()
	}
}

function applyAuthoritativeRooms(body, source) {
	if (!Array.isArray(body.rooms)) return false
	return setRooms(body.rooms, body.epoch || body.rooms[0]?.epoch, source)
}

function applyAuthoritativeConflict(body, requestEpoch) {
	if (!Array.isArray(body.rooms) || !shouldAcceptConflictSnapshot(state.epoch, body.epoch, requestEpoch)) return
	const replacedSocketEpoch = state.websocketSnapshotReceived && Boolean(state.epoch) && state.epoch !== body.epoch
	setRooms(body.rooms, body.epoch, 'authoritative-conflict')
	if (replacedSocketEpoch) {
		// WHY: the conflict proves the request reached a replacement process. The previous socket could still have queued
		// old-epoch messages; retiring its sequence prevents them from reinstalling stale state before reconnecting.
		state.socketSequence++
		state.websocketSnapshotReceived = false
		connectAdminSocket()
	}
	render()
}

function render() {
	renderRooms()
	renderDetails()
	renderMeetings()
}

function renderRooms() {
	const rooms = Array.from(state.rooms.values()).sort((left, right) => left.room_name.localeCompare(right.room_name))
	roomCount.textContent = `${rooms.length} 個房間`
	const startTargets = reconciliationTargets(rooms, 'start')
	const stopTargets = reconciliationTargets(rooms, 'stop')
	const forceStopTargets = reconciliationTargets(rooms, 'force-stop')
	startAllButton.disabled = state.requestPending || startTargets.length === 0
	stopAllButton.disabled = state.requestPending || stopTargets.length === 0
	forceStopAllButton.hidden = forceStopTargets.length === 0
	forceStopAllButton.disabled = state.requestPending
	roomsBody.innerHTML = rooms
		.map(room => {
			const action = reconciliationActionFor(room)
			const actionLabel = { start: '開始', stop: '停止', 'force-stop': '強制停止' }[action] || ''
			return `<tr class="${room.room_name === state.selectedRoom ? 'selected' : ''}" data-room="${escapeHtml(room.room_name)}" tabindex="0">
				<td>${escapeHtml(room.room_name)}</td>
				<td><span class="status ${escapeHtml(room.lifecycle || 'unknown')}">${escapeHtml(room.lifecycle || 'unknown')}</span><small class="run-number">run ${Number(room.reconciliation_run || 0)} / gen ${Number(room.generation || 0)}</small></td>
				<td>${escapeHtml(room.desired_meeting_id || 'InitialMeetingRequired')}<small class="run-number">${escapeHtml(room.desired_status || 'status unknown')}</small></td>
				<td>${escapeHtml(formatIDs(room.active_meeting_ids))}<small class="run-number ${room.active_set_stale ? 'stale-observation' : ''}">${room.active_set_stale ? 'stale / ' : 'fresh / '}${escapeHtml(formatTime(room.active_observed_at))}</small></td>
				<td><strong>${escapeHtml(room.summary || 'Unknown')}</strong><small class="run-number">${escapeHtml(room.summary_reason || '—')}</small></td>
				<td>${action ? `<button type="button" class="row-action ${action === 'force-stop' ? 'danger-action' : ''}" data-reconciliation-room="${escapeHtml(room.room_name)}" ${state.requestPending ? 'disabled' : ''}>${actionLabel}</button>` : '—'}</td>
			</tr>`
		})
		.join('')
	roomsBody.querySelectorAll('[data-room]').forEach(row => {
		row.addEventListener('click', () => void selectRoom(row.dataset.room))
		row.addEventListener('keydown', event => {
			if (event.key === 'Enter' || event.key === ' ') void selectRoom(row.dataset.room)
		})
	})
	roomsBody.querySelectorAll('[data-reconciliation-room]').forEach(button =>
		button.addEventListener('click', event => {
			event.stopPropagation()
			void beginRoomReconciliation(button.dataset.reconciliationRoom)
		}),
	)
}

function renderDetails() {
	const room = state.rooms.get(state.selectedRoom)
	selectedRoomLabel.textContent = room ? room.room_name : '尚未選擇房間'
	const editable = canEditDesired(room)
	refreshButton.disabled = state.requestPending || !canObserve(room)
	targetMeeting.disabled = state.requestPending || !editable
	applyDesiredButton.disabled = state.requestPending || !editable
	rearmDesiredButton.disabled = state.requestPending || !editable || !room?.desired_meeting_id
	if (!room) {
		roomDetails.textContent = '請選擇房間。'
		return
	}
	roomDetails.innerHTML = `<dl class="detail-list">
		<div class="detail-row"><dt>Lifecycle</dt><dd>${escapeHtml(room.lifecycle)} / run ${Number(room.reconciliation_run || 0)}</dd></div>
		<div class="detail-row"><dt>Generation</dt><dd>${Number(room.generation || 0)}${room.resume_consumed ? ' / Resume 已使用' : ''}</dd></div>
		<div class="detail-row"><dt>Desired</dt><dd>${escapeHtml(room.desired_meeting_id || 'InitialMeetingRequired')} / ${escapeHtml(room.desired_status || 'unknown')}</dd></div>
		<div class="detail-row"><dt>Active set</dt><dd>${escapeHtml(formatIDs(room.active_meeting_ids))}</dd></div>
		<div class="detail-row"><dt>觀測</dt><dd>${room.active_set_stale ? 'stale / ' : 'fresh / '}${escapeHtml(formatTime(room.active_observed_at))}</dd></div>
		<div class="detail-row"><dt>摘要</dt><dd>${escapeHtml(room.summary || 'Unknown')} / ${escapeHtml(room.summary_reason || '—')}</dd></div>
		<div class="detail-row detail-stack"><dt>Conditions</dt><dd>${conditionsMarkup(room.conditions)}</dd></div>
		<div class="detail-row detail-stack"><dt>Recent actions</dt><dd>${actionsMarkup(room.recent_actions)}</dd></div>
		<div class="detail-row"><dt>錯誤</dt><dd class="${room.last_error ? 'error-text' : ''}">${escapeHtml(room.last_error || '—')}</dd></div>
	</dl>`
}

function conditionsMarkup(conditions) {
	if (!Array.isArray(conditions) || !conditions.length) return '—'
	return `<ul class="structured-list">${conditions
		.map(
			condition =>
				`<li><strong>${escapeHtml(condition.type)}</strong><span>${escapeHtml(condition.status)} / ${escapeHtml(condition.reason || '—')}${condition.message ? ` / ${escapeHtml(condition.message)}` : ''}</span></li>`,
		)
		.join('')}</ul>`
}

function actionsMarkup(actions) {
	if (!Array.isArray(actions) || !actions.length) return '—'
	return `<ul class="structured-list">${actions
		.map(
			action =>
				`<li><strong>${escapeHtml(action.action)}${action.meeting_id ? ` / ${escapeHtml(action.meeting_id)}` : ''}</strong><span>${action.succeeded ? '成功' : '失敗'} / ${escapeHtml(formatTime(action.dispatched_at))}${action.error ? ` / ${escapeHtml(action.error)}` : ''}</span></li>`,
		)
		.join('')}</ul>`
}

function renderMeetings() {
	const meetings = state.meetings.get(state.selectedRoom) || []
	const editable = canEditDesired(state.rooms.get(state.selectedRoom))
	meetingsStatus.textContent = state.selectedRoom ? `${meetings.length} 場會議` : '請選擇房間'
	roomMeetings.innerHTML = meetings.length
		? meetings
				.map(
					meeting => `<button type="button" class="meeting-item ${meeting.id === targetMeeting.value.trim() ? 'selected' : ''}" data-meeting="${escapeHtml(meeting.id)}" ${editable && !state.requestPending ? '' : 'disabled'}>
			<span class="meeting-title">${escapeHtml(meeting.title || meeting.id)}</span><span class="meeting-meta">${escapeHtml(meeting.id)} · ${escapeHtml(meeting.status)}</span>
		</button>`,
				)
				.join('')
		: '<p class="meeting-empty">目前沒有可選擇的會議。</p>'
	roomMeetings.querySelectorAll('[data-meeting]').forEach(button =>
		button.addEventListener('click', () => {
			targetMeeting.value = button.dataset.meeting
			state.formDirty = true
			renderMeetings()
		}),
	)
}

function formatIDs(ids) {
	return Array.isArray(ids) && ids.length ? ids.join(', ') : '空集合'
}

function formatTime(value) {
	if (!value || String(value).startsWith('0001-')) return '時間未知'
	const date = new Date(value)
	if (Number.isNaN(date.getTime())) return '時間未知'
	return new Intl.DateTimeFormat('zh-TW', { dateStyle: 'short', timeStyle: 'medium' }).format(date)
}

function connectAdminSocket() {
	const socketSequence = ++state.socketSequence
	const socket = new WebSocket(`${location.origin.replace(/^http/, 'ws')}/ws/admin`)
	const status = document.getElementById('ws-status')
	const pendingRooms = new Map()
	let snapshotReceived = false
	socket.addEventListener('open', () => {
		if (socketSequence === state.socketSequence) status.textContent = '已連線'
	})
	socket.addEventListener('close', () => {
		if (socketSequence !== state.socketSequence) return
		// WHY: events queued by the closed socket previously remained eligible after reconnect. Advancing the sequence
		// fences those old callbacks before HTTP discovers the replacement process and the next socket is established.
		const reconnectSequence = ++state.socketSequence
		state.websocketSnapshotReceived = false
		status.textContent = '已中斷，正在重新連線'
		window.setTimeout(() => void reconnectAdminSocket(reconnectSequence), 2000)
	})
	socket.addEventListener('message', event => {
		if (socketSequence !== state.socketSequence) return
		try {
			const message = JSON.parse(event.data)
			if (message.type === 'room_snapshot' && !snapshotReceived) {
				bufferRoomSnapshot(pendingRooms, message)
				return
			}
			if (message.type === 'snapshot') {
				if (!setRooms(message.rooms || [], message.epoch, 'websocket')) return
				snapshotReceived = true
				// WHY: registration precedes the server's full snapshot, so room updates can arrive first. The previous ordering
				// lost those updates; replaying only newer same-epoch revisions preserves authoritative snapshot ordering.
				for (const pending of takeBufferedRoomSnapshots(pendingRooms, state.epoch)) {
					setRoom(pending.room, pending.epoch)
				}
			}
			if (message.type === 'room_snapshot') setRoom(message.room, message.epoch)
			render()
		} catch (error) {
			showAlert(error instanceof Error ? error.message : String(error))
		}
	})
}

async function reconnectAdminSocket(reconnectSequence) {
	if (reconnectSequence !== state.socketSequence) return
	try {
		await loadRooms()
	} catch (error) {
		if (error.authentication) return
		showAlert(error instanceof Error ? error.message : String(error))
	}
	if (reconnectSequence === state.socketSequence) connectAdminSocket()
}

function showAlert(message) {
	alerts.innerHTML = `<article class="alert error"><p>${escapeHtml(message)}</p></article>`
}

function escapeHtml(value) {
	return String(value)
		.replaceAll('&', '&amp;')
		.replaceAll('<', '&lt;')
		.replaceAll('>', '&gt;')
		.replaceAll('"', '&quot;')
		.replaceAll("'", '&#39;')
}

loadRooms()
	.then(connectAdminSocket)
	.catch(error => {
		if (!error.authentication) showAlert(error instanceof Error ? error.message : String(error))
	})
