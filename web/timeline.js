import {
	availableMeetingDates,
	isCurrentVersion,
	meetingsForDate,
	reconcileAuthoritativeRooms,
	shouldAcceptSnapshot,
} from './state.js'
import {
	intersectsTimelineWindow,
	shiftedMeetingTimes,
	timelineHalfWindowMinutes,
	timelinePositionPercent,
	timelineQuarterHourTicks,
	timelineWindow,
} from './timeline-model.js'

const state = {
	epoch: '',
	rooms: new Map(),
	meetings: new Map(),
	selectedDate: '',
	connected: false,
	websocketSnapshotReceived: false,
	socketSequence: 0,
	lastDataAt: null,
	lastRenderedAt: null,
}

const eventDaySelect = document.getElementById('event-day-select')
const roomColumns = document.getElementById('room-columns')
const timeRulerTrack = document.getElementById('time-ruler-track')
const timelineBody = document.getElementById('timeline-body')
const nowLine = document.getElementById('now-line')
const nowLabel = document.getElementById('now-label')
const timelineNotice = document.getElementById('timeline-notice')
const lastUpdated = document.getElementById('last-updated')
const statusPill = document.getElementById('ws-status')

const selectedDateStorageKey = 'coscup-caption.timeline-date.v1'
const minuteMilliseconds = 60_000
const windowMinutes = timelineHalfWindowMinutes

eventDaySelect.addEventListener('change', () => {
	state.selectedDate = eventDaySelect.value
	try {
		window.localStorage.setItem(selectedDateStorageKey, state.selectedDate)
	} catch {
		// The selected date remains usable for this page even when browser storage is unavailable.
	}
	render()
})

async function apiFetch(url) {
	const response = await fetch(url)
	if (response.status === 401) {
		window.location.assign(`/login?redirect=${encodeURIComponent(window.location.pathname)}`)
		const error = new Error('登入狀態已失效。')
		error.authentication = true
		throw error
	}
	if (!response.ok) throw new Error(`伺服器回傳 ${response.status}。`)
	return response
}

async function loadRooms() {
	const response = await apiFetch('/api/rooms')
	const body = await response.json()
	applyRooms(body.rooms || [], body.epoch, 'http')
	render()
}

function applyRooms(rooms, epoch, source) {
	if (!Array.isArray(rooms) || !epoch) return false
	if (!shouldAcceptSnapshot(state.epoch, epoch, source, state.websocketSnapshotReceived)) return false
	const epochChanged = state.epoch !== '' && state.epoch !== epoch
	const reconciled = reconcileAuthoritativeRooms(
		epochChanged ? new Map() : state.rooms,
		epochChanged ? new Map() : state.meetings,
		rooms,
	)
	state.epoch = epoch
	state.rooms = reconciled.rooms
	state.meetings = reconciled.meetings
	state.lastDataAt = new Date()
	if (source === 'websocket') state.websocketSnapshotReceived = true
	return true
}

function applyRoom(room) {
	if (!room?.room_name || !room.epoch) return false
	if (state.epoch && state.epoch !== room.epoch) return false
	const current = state.rooms.get(room.room_name)
	if (current && !isCurrentVersion(room, current)) return false
	state.rooms.set(room.room_name, room)
	if (Array.isArray(room.meetings)) state.meetings.set(room.room_name, room.meetings)
	state.lastDataAt = new Date()
	return true
}

function getNow() {
	return new Date()
}

function render() {
	const now = getNow()
	// Re-rendered content is positioned against this instant; keeping the anchor here prevents old clock drift from being applied twice.
	state.lastRenderedAt = now
	const dates = availableMeetingDates(state.meetings)
	selectDefaultDate(dates, now)
	renderDatePicker(dates)
	renderNotice(now)
	renderColumns(now)
	renderTimeRuler(now)
	updateConnectionState()
	updateClock(now)
}

function selectDefaultDate(dates, now) {
	if (dates.includes(state.selectedDate)) return
	let storedDate = ''
	try {
		storedDate = window.localStorage.getItem(selectedDateStorageKey) || ''
	} catch {
		storedDate = ''
	}
	const today = dateKey(now)
	state.selectedDate = dates.includes(storedDate)
		? storedDate
		: dates.includes(today)
			? today
			: nearestDate(dates, today)
}

function nearestDate(dates, reference) {
	if (!dates.length) return ''
	return dates.slice().sort((left, right) => {
		const leftDistance = Math.abs(parseDateKey(left) - parseDateKey(reference))
		const rightDistance = Math.abs(parseDateKey(right) - parseDateKey(reference))
		return leftDistance - rightDistance || left.localeCompare(right)
	})[0]
}

function parseDateKey(value) {
	const date = new Date(`${value}T00:00:00`)
	return Number.isNaN(date.getTime()) ? Number.POSITIVE_INFINITY : date.getTime()
}

function renderDatePicker(dates) {
	eventDaySelect.disabled = dates.length === 0
	eventDaySelect.innerHTML = dates
		.map(date => `<option value="${escapeAttr(date)}">${escapeHtml(formatEventDate(date))}</option>`)
		.join('')
	eventDaySelect.value = state.selectedDate
}

function renderNotice(now) {
	const today = dateKey(now)
	if (!state.selectedDate) {
		timelineNotice.hidden = false
		timelineNotice.textContent = '目前沒有可用的 OPASS 活動日。'
		return
	}
	if (state.selectedDate !== today) {
		timelineNotice.hidden = false
		timelineNotice.textContent = '目前時間不在所選活動日；時間線仍代表真實現在時間。'
		return
	}
	timelineNotice.hidden = true
	timelineNotice.textContent = ''
}

function renderColumns(now) {
	const rooms = Array.from(state.rooms.values()).sort((left, right) =>
		left.room_name.localeCompare(right.room_name, 'zh-Hant-TW', { numeric: true, sensitivity: 'base' }),
	)
	if (!rooms.length) {
		roomColumns.innerHTML = '<div class="empty-timeline">目前沒有可監看的教室。</div>'
		return
	}
	const { start: windowStart, end: windowEnd } = timelineWindow(now)
	const selectedMeetings = new Map(
		rooms.map(room => [
			room.room_name,
			meetingsForDate(state.meetings.get(room.room_name) || room.meetings, state.selectedDate),
		]),
	)
	roomColumns.innerHTML = rooms
		.map(room => renderRoomColumn(room, selectedMeetings.get(room.room_name) || [], windowStart, windowEnd))
		.join('')
	applyTimelinePositions(roomColumns)
}

function renderRoomColumn(room, meetings, windowStart, windowEnd) {
	const roomStatus = room.last_error
		? 'error'
		: room.active_set_stale && room.lifecycle !== 'suspended'
			? 'stale'
			: room.lifecycle || 'unknown'
	const scheduledMeetings = meetings.filter(meeting => validSchedule(meeting))
	const blocks = scheduledMeetings
		.flatMap(meeting => renderMeetingBlocks(room, meeting, windowStart, windowEnd))
		.join('')
	const ticks = renderTrackTicks(windowStart, windowEnd)
	const empty = scheduledMeetings.length ? '' : '<div class="room-empty">此活動日沒有排定議程</div>'
	return `<article class="room-column">
		<header class="room-header">
			<div class="room-name" title="${escapeAttr(room.room_name)}">${escapeHtml(room.room_name)}</div>
			<div class="room-state ${roomStatus}" title="${escapeAttr(roomDetails(room))}">${escapeHtml(roomSummary(room))}</div>
		</header>
		<div class="room-track">
			<div class="timeline-track-content">${ticks}${blocks}${empty}</div>
		</div>
	</article>`
}

function renderMeetingBlocks(room, meeting, windowStart, windowEnd) {
	const shifted = shiftedMeetingTimes(meeting, room.schedule_offset_minutes)
	if (!shifted) return []
	const { originalStart, originalEnd, adjustedStart, adjustedEnd, offset } = shifted
	const statusClassName = meetingStatusClass(meeting.status, room.last_error)
	const original = renderMeetingBlock({
		meeting,
		start: originalStart,
		end: originalEnd,
		windowStart,
		windowEnd,
		statusClassName,
		kind: 'original',
	})
	const adjusted = renderMeetingBlock({
		meeting,
		start: adjustedStart,
		end: adjustedEnd,
		windowStart,
		windowEnd,
		statusClassName,
		kind: 'adjusted',
		originalStart,
		originalEnd,
		offset,
	})
	return offset === 0 ? [adjusted] : [original, adjusted]
}

function renderMeetingBlock({
	meeting,
	start,
	end,
	windowStart,
	windowEnd,
	statusClassName,
	kind,
	originalStart,
	originalEnd,
	offset,
}) {
	if (!intersectsTimelineWindow(start, end, { start: windowStart, end: windowEnd })) return ''
	const clippedStart = start < windowStart ? windowStart : start
	const clippedEnd = end > windowEnd ? windowEnd : end
	const top = timelinePositionPercent(clippedStart, { start: windowStart, end: windowEnd })
	const endPosition = timelinePositionPercent(clippedEnd, { start: windowStart, end: windowEnd })
	if (top === null || endPosition === null) return ''
	const height = Math.max(endPosition - top, 1.4)
	const clipsTop = start < windowStart
	const clipsBottom = end > windowEnd
	const title = escapeHtml(meeting.title || meeting.id)
	const edgeClasses = `${clipsTop ? ' clips-top' : ''}${clipsBottom ? ' clips-bottom' : ''}`
	if (kind === 'original') {
		return `<div class="meeting-block original ${statusClassName}${edgeClasses}" style="top:${top}%;height:${height}%" aria-hidden="true"></div>`
	}
	const originalTime = `${formatClockTime(originalStart)} - ${formatClockTime(originalEnd)}`
	const adjustedTime = `${formatClockTime(start)} - ${formatClockTime(end)}`
	const offsetLabel = offset ? ` (${offset > 0 ? '+' : ''}${offset} 分鐘)` : ''
	return `<div class="meeting-block adjusted ${statusClassName}${edgeClasses}" style="top:${top}%;height:${height}%" title="${escapeAttr(meeting.title || meeting.id)}">
		<div class="meeting-title">${title}</div>
		<div class="meeting-status">${escapeHtml(translateMeetingStatus(meeting.status))}</div>
		<div class="meeting-time">原始 ${escapeHtml(originalTime)}</div>
		<div class="meeting-time">調整後 ${escapeHtml(adjustedTime)}${escapeHtml(offsetLabel)}</div>
	</div>`
}

function renderTimeRuler(now) {
	const { start: windowStart, end: windowEnd } = timelineWindow(now)
	timeRulerTrack.innerHTML = `<div class="ruler-track-content timeline-track-content">${renderTrackTicks(windowStart, windowEnd, true)}</div>`
	applyTimelinePositions(timeRulerTrack)
	nowLabel.textContent = formatClockTime(now)
}

function renderTrackTicks(windowStart, windowEnd, ruler = false) {
	const ticks = []
	for (const tick of timelineQuarterHourTicks(windowStart, windowEnd)) {
		const top = timelinePositionPercent(tick, { start: windowStart, end: windowEnd })
		if (top === null) continue
		const isHour = tick.getMinutes() === 0
		const position = top.toFixed(4)
		ticks.push(`<div class="timeline-tick-line${isHour ? ' hour' : ''}" data-timeline-top="${position}"></div>`)
		if (ruler)
			ticks.push(
				`<span class="timeline-tick-label" data-timeline-top="${position}">${formatClockTime(tick)}</span>`,
			)
	}
	return ticks.join('')
}

function applyTimelinePositions(root) {
	for (const element of root.querySelectorAll('[data-timeline-top]')) {
		const top = Number(element.dataset.timelineTop)
		if (!Number.isFinite(top)) {
			element.remove()
			continue
		}
		element.style.top = `${top}%`
	}
}

function updateClock(now = getNow()) {
	if (!state.lastRenderedAt) state.lastRenderedAt = now
	const trackHeight = timelineBody.querySelector('.room-track')?.clientHeight || 0
	const drift = trackHeight
		? -((now.getTime() - state.lastRenderedAt.getTime()) / (2 * windowMinutes * minuteMilliseconds)) * trackHeight
		: 0
	document.documentElement.style.setProperty('--timeline-drift', `${drift}px`)
	const roomTrack = timelineBody.querySelector('.room-track')
	if (roomTrack) nowLine.style.top = `${roomTrack.offsetTop + roomTrack.clientHeight / 2}px`
	nowLabel.textContent = formatClockTime(now)
	if (now.getMinutes() !== state.lastRenderedAt.getMinutes() || now.getDate() !== state.lastRenderedAt.getDate()) {
		state.lastRenderedAt = now
		render()
	}
	updateConnectionState()
}

function updateConnectionState() {
	statusPill.textContent = state.connected ? '已連線' : '資料更新中斷'
	statusPill.className = `connection-pill ${state.connected ? 'connected' : 'disconnected'}`
	document.body.classList.toggle('is-stale', !state.connected)
	lastUpdated.textContent = state.lastDataAt ? `最後更新 ${formatDateTime(state.lastDataAt)}` : '尚未收到資料'
}

function connectSocket() {
	const sequence = ++state.socketSequence
	const socket = new WebSocket(`${location.origin.replace(/^http/, 'ws')}/ws/admin`)
	socket.addEventListener('open', () => {
		if (sequence !== state.socketSequence) return
		state.connected = true
		updateConnectionState()
	})
	socket.addEventListener('close', () => {
		if (sequence !== state.socketSequence) return
		state.connected = false
		state.websocketSnapshotReceived = false
		updateConnectionState()
		window.setTimeout(() => {
			if (sequence === state.socketSequence) void loadRooms().finally(connectSocket)
		}, 2000)
	})
	socket.addEventListener('message', event => {
		if (sequence !== state.socketSequence) return
		try {
			const message = JSON.parse(event.data)
			const accepted =
				message.type === 'snapshot'
					? applyRooms(message.rooms || [], message.epoch, 'websocket')
					: message.type === 'room_snapshot'
						? applyRoom(message.room)
						: false
			if (accepted) render()
		} catch {
			// A malformed incremental message must not discard the last usable snapshot on a read-only monitor.
		}
	})
}

function validSchedule(meeting) {
	return Boolean(shiftedMeetingTimes(meeting, 0))
}

function meetingStatusClass(status, hasError) {
	if (hasError) return 'status-error'
	return (
		{
			ready: 'status-ready',
			in_progress: 'status-in-progress',
			paused: 'status-paused',
			completed: 'status-completed',
		}[status] || 'status-error'
	)
}

function roomSummary(room) {
	if (room.last_error) return '資料異常'
	if (room.active_set_stale && room.lifecycle !== 'suspended') return '觀測過期'
	return (
		{
			active: '教室使用中',
			starting: '正在開始',
			stopping: '正在停止',
			suspended: '教室已停止',
		}[room.lifecycle] || '狀態未知'
	)
}

function roomDetails(room) {
	const summary = roomSummary(room)
	const reason = room.summary_reason ? `：${room.summary_reason}` : ''
	return `${summary}${reason}`
}

function translateMeetingStatus(status) {
	return { ready: '尚未開始', in_progress: '進行中', paused: '已暫停', completed: '已完成' }[status] || '狀態未知'
}

function formatClockTime(value) {
	const date = value instanceof Date ? value : new Date(value)
	if (Number.isNaN(date.getTime())) return '時間未知'
	return new Intl.DateTimeFormat('zh-TW', { hour: '2-digit', minute: '2-digit', hour12: false }).format(date)
}

function formatDateTime(value) {
	const date = value instanceof Date ? value : new Date(value)
	if (Number.isNaN(date.getTime())) return '時間未知'
	return new Intl.DateTimeFormat('zh-TW', {
		hour: '2-digit',
		minute: '2-digit',
		second: '2-digit',
		hour12: false,
	}).format(date)
}

function formatEventDate(value) {
	const date = new Date(`${value}T00:00:00`)
	if (Number.isNaN(date.getTime())) return value
	return new Intl.DateTimeFormat('zh-TW', { month: 'numeric', day: 'numeric', weekday: 'short' }).format(date)
}

function dateKey(date) {
	const pad = value => String(value).padStart(2, '0')
	return `${date.getFullYear()}-${pad(date.getMonth() + 1)}-${pad(date.getDate())}`
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

loadRooms()
	.then(() => {
		render()
		connectSocket()
	})
	.catch(error => {
		if (!error.authentication) {
			state.connected = false
			updateConnectionState()
		}
		connectSocket()
	})

window.setInterval(() => updateClock(), 1000)
window.addEventListener('resize', () => render())
