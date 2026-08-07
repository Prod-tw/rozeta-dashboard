export const timelineHalfWindowMinutes = 60
export const timelineTickMinutes = 15

export function timelineWindow(now, halfWindowMinutes = timelineHalfWindowMinutes) {
	const duration = halfWindowMinutes * 60_000
	return {
		start: new Date(now.getTime() - duration),
		end: new Date(now.getTime() + duration),
	}
}

export function timelineQuarterHourTicks(start, end) {
	const interval = timelineTickMinutes * 60_000
	const first = Math.ceil(start.getTime() / interval) * interval
	const ticks = []
	for (let value = first; value <= end.getTime(); value += interval) ticks.push(new Date(value))
	return ticks
}

export function shiftedMeetingTimes(meeting, offsetMinutes) {
	const start = parseTime(meeting?.scheduled_start)
	const end = parseTime(meeting?.scheduled_end)
	if (!start || !end || end <= start) return null
	const offset = Number.isFinite(Number(offsetMinutes)) ? Number(offsetMinutes) : 0
	const milliseconds = offset * 60_000
	return {
		originalStart: start,
		originalEnd: end,
		adjustedStart: new Date(start.getTime() + milliseconds),
		adjustedEnd: new Date(end.getTime() + milliseconds),
		offset,
	}
}

export function intersectsTimelineWindow(start, end, window) {
	return end > window.start && start < window.end
}

export function timelinePositionPercent(value, window) {
	const duration = window.end.getTime() - window.start.getTime()
	if (!Number.isFinite(duration) || duration <= 0) return null
	const position = ((value.getTime() - window.start.getTime()) / duration) * 100
	return Number.isFinite(position) ? position : null
}

function parseTime(value) {
	if (!value) return null
	const date = value instanceof Date ? value : new Date(value)
	return Number.isNaN(date.getTime()) ? null : date
}
