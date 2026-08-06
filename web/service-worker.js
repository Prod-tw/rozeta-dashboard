self.addEventListener('message', event => {
	// WHY: the page owns the clock and alert decision; this worker only renders a system
	// notification while the dashboard remains open and can provide current observations.
	if (event.data?.type !== 'show-schedule-alert') return
	const alert = event.data.alert
	if (!alert?.key) return
	event.waitUntil(
		self.registration.showNotification(`${alert.testMode ? '測試提醒：' : ''}議程提醒：${alert.roomName}`, {
			tag: `schedule-alert:${alert.key}`,
			body: `${alert.testMode ? '測試時間：' : ''}${alert.kind === 'early' ? `${alert.meetingTitle} 可能提前切換，上一個議程尚未進入容忍區間。` : `${alert.meetingTitle} 已達切換提醒時間，請確認是否切換。`}`,
			data: { roomName: alert.roomName },
		}),
	)
})

self.addEventListener('notificationclick', event => {
	event.notification.close()
	event.waitUntil(
		self.clients.matchAll({ type: 'window', includeUncontrolled: true }).then(clients => {
			const existing = clients.find(client => 'focus' in client)
			if (!existing) return self.clients.openWindow('/')
			existing.postMessage({ type: 'focus-room', roomName: event.notification.data?.roomName || '' })
			return existing.focus()
		}),
	)
})
