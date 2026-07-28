// ==UserScript==
// @name         Rozeta Command Panel
// @namespace    https://rozeta.app/
// @version      0.1.0
// @description  Small top-right panel to send Rozeta remote-control commands from the meeting room page.
// @match        https://rozeta.app/en/meetings/*/room*
// @match        https://rozeta.app/*
// @run-at       document-end
// @grant        none
// ==/UserScript==

(function () {
	'use strict'

	const PANEL_ID = 'rozeta-command-panel'
	const SERVER_URL_KEY = 'rozeta-agent-server-url'
	const ROOM_NAME_KEY = 'rozeta-agent-room-name'
	const AGENT_ID_KEY = 'rozeta-agent-id'
	const PENDING_AUTO_START_KEY = 'rozeta-command-panel-pending-auto-start'
	const RECENT_COMMAND_LIMIT = 50

	let agentSocket = null
	let heartbeatTimer = null
	let reconnectTimer = null
	let agentManuallyDisconnected = false
	let agentConnected = false
	let agentConnectionTerminal = false
	let recentCommandIds = []
	let agentState = {
		status: 'ready',
		currentMeetingId: '',
		lastCommandId: '',
		lastCommandResult: '',
		lastError: '',
	}

	// Keep the panel on bottom-right so it stays out of the main meeting controls.
	// The page previously had no in-page controller for remote commands; this adds
	// a lightweight overlay without depending on Rozeta's internal UI structure.
	const panel = document.createElement('div')
	panel.id = PANEL_ID
	panel.style.cssText = [
		'position:fixed',
		'bottom:12px',
		'right:12px',
		'z-index:2147483647',
		'width:48px',
		'height:48px',
		'padding:0',
		'overflow:hidden',
		'box-sizing:border-box',
		'background:rgba(17, 24, 39, 0.96)',
		'color:#f9fafb',
		'border:1px solid rgba(255,255,255,0.12)',
		'border-radius:14px',
		'box-shadow:0 10px 30px rgba(0,0,0,0.35)',
		'font:12px/1.4 system-ui, -apple-system, BlinkMacSystemFont, "Segoe UI", sans-serif',
		'backdrop-filter:blur(8px)',
		'transition:width 180ms ease, height 180ms ease, padding 180ms ease, border-radius 180ms ease'
	].join(';')

	const trigger = document.createElement('div')
	// The collapsed state should look like a launcher, not an empty box, so the icon
	// now makes the hidden panel obvious before hover expands it.
	trigger.textContent = '⋮'
	Object.assign(trigger.style, {
		position: 'absolute',
		right: '0',
		top: '0',
		display: 'flex',
		alignItems: 'center',
		justifyContent: 'center',
		width: '48px',
		height: '48px',
		cursor: 'default',
		fontSize: '18px',
		transition: 'opacity 120ms ease',
		userSelect: 'none',
	})

	const content = document.createElement('div')
	content.innerHTML = `
		<div style="display:flex;align-items:center;justify-content:space-between;gap:8px;margin-bottom:10px;">
			<strong style="font-size:13px;">Rozeta Command Panel</strong>
			<span id="rozeta-command-panel-status" style="color:#9ca3af;">idle</span>
		</div>
		<div style="padding:10px;margin-bottom:10px;border:1px solid rgba(255,255,255,0.10);border-radius:10px;background:rgba(255,255,255,0.03);">
			<div style="display:flex;align-items:center;justify-content:space-between;gap:8px;margin-bottom:8px;">
				<strong style="font-size:12px;">Agent</strong>
				<span id="rozeta-agent-connection-status" style="color:#9ca3af;">disconnected</span>
			</div>
			<label style="display:block;margin-bottom:8px;">
				<div style="margin-bottom:4px;color:#d1d5db;">Server URL</div>
				<input id="rozeta-agent-server-url" type="text" autocomplete="off" spellcheck="false"
					style="width:100%;box-sizing:border-box;padding:8px 10px;border-radius:8px;border:1px solid rgba(255,255,255,0.16);background:#111827;color:#f9fafb;outline:none;" />
			</label>
			<label style="display:block;margin-bottom:8px;">
				<div style="margin-bottom:4px;color:#d1d5db;">Room Name</div>
				<input id="rozeta-agent-room-name" type="text" autocomplete="off" spellcheck="false"
					style="width:100%;box-sizing:border-box;padding:8px 10px;border-radius:8px;border:1px solid rgba(255,255,255,0.16);background:#111827;color:#f9fafb;outline:none;" />
			</label>
			<div style="display:grid;grid-template-columns:repeat(2, minmax(0, 1fr));gap:8px;">
				<button type="button" id="rozeta-agent-connect" style="padding:8px 10px;border:0;border-radius:8px;background:#2563eb;color:white;font-weight:600;cursor:pointer;">Connect</button>
				<button type="button" id="rozeta-agent-disconnect" style="padding:8px 10px;border:0;border-radius:8px;background:#374151;color:white;font-weight:600;cursor:pointer;">Disconnect</button>
			</div>
			<div id="rozeta-agent-summary" style="margin-top:8px;color:#9ca3af;word-break:break-word;"></div>
		</div>
		<pre id="rozeta-command-panel-log" style="margin:0;max-height:180px;overflow:auto;padding:8px;border-radius:8px;background:#030712;color:#cbd5e1;white-space:pre-wrap;word-break:break-word;"></pre>
	`
	Object.assign(content.style, {
		width: '336px',
		opacity: '0',
		visibility: 'hidden',
		transition: 'opacity 120ms ease',
	})

	panel.append(trigger, content)

	let routeWatchTimer = null
	let lastObservedHref = location.href

	function isRoomRoute() {
		return /^\/en\/meetings\/[^/]+\/room/.test(location.pathname)
	}

	function shouldShowPanel() {
		return true
	}

	function getPanelHost() {
		return document.body || document.documentElement
	}

	function mountPanel() {
		const host = getPanelHost()
		if (!host) {
			return false
		}

		// Rozeta is a client-routed app, so the meeting room DOM can be replaced after
		// startup. The old one-shot append could disappear during navigation; this keeps
		// one panel instance and reattaches it whenever the room route is active.
		if (panel.parentElement !== host) {
			host.appendChild(panel)
		}

		return true
	}

	function unmountPanel() {
		if (panel.isConnected) {
			panel.remove()
		}
	}

	function syncPanelMount() {
		if (shouldShowPanel()) {
			mountPanel()
			return
		}

		unmountPanel()
	}

	function startRouteWatcher() {
		if (routeWatchTimer) {
			return
		}

		const syncIfRouteChanged = () => {
			if (location.href === lastObservedHref) {
				return
			}

			lastObservedHref = location.href
			// Rozeta is a client-routed app, so we only need to react when the URL changes.
			// The previous DOM-wide observer retriggered on routine page mutations and could
			// churn the home page. This keeps the panel responsive without watching the whole tree.
			mountPanel()
			syncPanelState()
		}

		window.addEventListener('popstate', syncIfRouteChanged)
		window.addEventListener('hashchange', syncIfRouteChanged)
		routeWatchTimer = window.setInterval(syncIfRouteChanged, 500)
	}

	syncPanelMount()
	startRouteWatcher()

	const statusNode = panel.querySelector('#rozeta-command-panel-status')
	const agentStatusNode = panel.querySelector('#rozeta-agent-connection-status')
	const agentServerUrlInput = panel.querySelector('#rozeta-agent-server-url')
	const agentRoomNameInput = panel.querySelector('#rozeta-agent-room-name')
	const agentSummaryNode = panel.querySelector('#rozeta-agent-summary')
	const logNode = panel.querySelector('#rozeta-command-panel-log')
	const connectButton = panel.querySelector('#rozeta-agent-connect')
	const disconnectButton = panel.querySelector('#rozeta-agent-disconnect')

	let hideTimer = null
	let lastRoomRoute = null
	let isPanelDisabled = false

	function expandPanel() {
		if (isPanelDisabled) {
			return
		}

		window.clearTimeout(hideTimer)

		Object.assign(panel.style, {
			width: '360px',
			height: 'auto',
			padding: '12px',
			borderRadius: '12px',
		})

		trigger.style.opacity = '0'
		content.style.visibility = 'visible'

		requestAnimationFrame(() => {
			content.style.opacity = '1'
		})
	}

	function collapsePanel() {
		hideTimer = window.setTimeout(() => {
			content.style.opacity = '0'

			Object.assign(panel.style, {
				width: '48px',
				height: '48px',
				padding: '0',
				borderRadius: '14px',
			})

			window.setTimeout(() => {
				content.style.visibility = 'hidden'
				trigger.style.opacity = '1'
			}, 160)
		}, 250)
	}

	panel.addEventListener('mouseenter', () => {
		if (isPanelDisabled) {
			return
		}

		expandPanel()
	})

	panel.addEventListener('mouseleave', () => {
		collapsePanel()
	})

	function setControlsDisabled(disabled) {
		isPanelDisabled = disabled
		agentServerUrlInput.disabled = disabled
		agentRoomNameInput.disabled = disabled
		connectButton.disabled = disabled
		disconnectButton.disabled = disabled
		connectButton.style.cursor = disabled ? 'not-allowed' : 'pointer'
		disconnectButton.style.cursor = disabled ? 'not-allowed' : 'pointer'
		panel.style.opacity = disabled ? '0.72' : '1'
		panel.dataset.disabled = disabled ? 'true' : 'false'

		if (disabled) {
			// Disabled mode should remain collapsed so it behaves like an unavailable
			// launcher rather than an interactive panel that still opens on hover.
			collapsePanel()
		}
	}

	function syncPanelState() {
		const roomPage = isRoomRoute()
		setControlsDisabled(!roomPage)

		if (lastRoomRoute !== roomPage) {
			lastRoomRoute = roomPage
			setStatus(roomPage ? 'ready' : 'room page only')
		}

		// The panel used to vanish off room pages, which made it look like the feature
		// was broken rather than unavailable. Now the same shell stays visible and the
		// controls are explicitly disabled outside the meeting room route.
		if (roomPage) {
			syncAgentConnection()
			maybeTriggerPendingAutoStart().catch(error => log(error instanceof Error ? error.message : String(error)))
			return
		}

		disconnectAgent(true)
		setAgentStatusLabel('disabled')
		setAgentState({ status: 'disabled', lastError: '', lastCommandResult: '' })
	}

	function setStatus(text) {
		statusNode.textContent = text
	}

	function log(message) {
		const line = `[${new Date().toLocaleTimeString()}] ${message}`
		logNode.textContent = `${line}\n${logNode.textContent}`.slice(0, 4000)
		console.log('[Rozeta Command Panel]', message)
	}

	function renderAgentSummary() {
		const meetingLabel = agentState.currentMeetingId || 'none'
		agentSummaryNode.textContent = `status: ${agentState.status} | meeting: ${meetingLabel}${agentState.lastError ? ` | error: ${agentState.lastError}` : ''}`
	}

	function setAgentStatusLabel(text) {
		agentStatusNode.textContent = text
	}

	function setAgentState(next) {
		agentState = { ...agentState, ...next }
		renderAgentSummary()
	}

	function currentMeetingIdFromUrl() {
		const match = location.pathname.match(/^\/en\/meetings\/([^/]+)\/room/)
		return match ? decodeURIComponent(match[1]) : ''
	}

	function normalizeServerUrl(url) {
		return (url || '').trim().replace(/\/$/, '')
	}

	function websocketUrlFromServerUrl(url) {
		const normalized = normalizeServerUrl(url)
		if (!normalized) {
			return ''
		}
		if (normalized.startsWith('ws://') || normalized.startsWith('wss://')) {
			return normalized + '/ws/agent'
		}
		if (normalized.startsWith('https://')) {
			return `wss://${normalized.slice('https://'.length)}/ws/agent`
		}
		if (normalized.startsWith('http://')) {
			return `ws://${normalized.slice('http://'.length)}/ws/agent`
		}
		return `ws://${normalized}/ws/agent`
	}

	function getAgentId() {
		let agentId = localStorage.getItem(AGENT_ID_KEY)
		if (!agentId) {
			agentId = `agent-${Math.random().toString(16).slice(2)}-${Date.now().toString(16)}`
			localStorage.setItem(AGENT_ID_KEY, agentId)
		}
		return agentId
	}

	function getRoomName() {
		return agentRoomNameInput.value.trim()
	}

	function getServerUrl() {
		return normalizeServerUrl(agentServerUrlInput.value)
	}

	function isRecentCommand(commandId) {
		return recentCommandIds.includes(commandId)
	}

	// The websocket can replay the same command after reconnects, so the agent keeps
	// a small in-memory history. Before this fix the listener called an undefined
	// helper and crashed; now duplicate commands are skipped safely.
	function rememberCommand(commandId) {
		if (!commandId || isRecentCommand(commandId)) {
			return false
		}

		recentCommandIds.push(commandId)
		if (recentCommandIds.length > RECENT_COMMAND_LIMIT) {
			recentCommandIds = recentCommandIds.slice(-RECENT_COMMAND_LIMIT)
		}
		return true
	}

	function roomUrlForMeetingId(meetingId) {
		return new URL(`/en/meetings/${encodeURIComponent(meetingId)}/room`, location.origin).toString()
	}

	function getClickableLabel(node) {
		return [node.textContent, node.getAttribute?.('aria-label'), node.getAttribute?.('title'), node.getAttribute?.('data-tooltip')]
			.filter(Boolean)
			.join(' ')
			.trim()
	}

	function describeElement(node) {
		if (!node) return 'null'
		const tag = node.tagName?.toLowerCase() || 'unknown'
		const id = node.id ? `#${node.id}` : ''
		const cls = node.className && typeof node.className === 'string' ? `.${node.className.trim().split(/\s+/).slice(0, 4).join('.')}` : ''
		const label = getClickableLabel(node)
		return `${tag}${id}${cls}${label ? ` :: ${label}` : ''}`
	}

	function clickLikeUser(element) {
		if (!element) return false

		const options = { bubbles: true, cancelable: true, view: window }
		const PointerEvt = window.PointerEvent || MouseEvent
		element.dispatchEvent(new PointerEvt('pointerdown', options))
		element.dispatchEvent(new MouseEvent('mousedown', options))
		element.dispatchEvent(new PointerEvt('pointerup', options))
		element.dispatchEvent(new MouseEvent('mouseup', options))
		element.dispatchEvent(new MouseEvent('click', options))
		element.click()
		return true
	}

	function findButtonByText(pattern) {
		const buttons = Array.from(document.querySelectorAll('button, [role="button"]'))
		return buttons.find(button => !button.closest(`#${PANEL_ID}`) && pattern.test(getClickableLabel(button))) || null
	}

	function findCaptionButton(labelPattern) {
		const captions = Array.from(document.querySelectorAll('span, div, small'))
			.filter(node => !node.closest(`#${PANEL_ID}`))
			.filter(node => labelPattern.test(node.textContent?.trim() || ''))

		for (const caption of captions) {
			const previous = caption.previousElementSibling
			if (previous instanceof HTMLButtonElement && !previous.closest(`#${PANEL_ID}`)) {
				return previous
			}

			const parentButton = caption.parentElement?.querySelector('button')
			if (parentButton instanceof HTMLButtonElement && !parentButton.closest(`#${PANEL_ID}`)) {
				return parentButton
			}

			const nearbyButton = caption.closest('div')?.querySelector('button')
			if (nearbyButton instanceof HTMLButtonElement && !nearbyButton.closest(`#${PANEL_ID}`)) {
				return nearbyButton
			}
		}

		return null
	}

	function findIconButton(iconPattern) {
		const buttons = Array.from(document.querySelectorAll('button'))
		return buttons.find(button => {
			if (button.closest(`#${PANEL_ID}`)) return false
			return iconPattern.test(button.innerHTML) || iconPattern.test(getClickableLabel(button))
		}) || null
	}

	function findCaptionedControl(labelPattern, iconPattern) {
		return findCaptionButton(labelPattern) || findIconButton(iconPattern) || findButtonByText(labelPattern)
	}

	function waitFor(predicate, timeoutMs = 8000, intervalMs = 100) {
		return new Promise((resolve, reject) => {
			const startedAt = Date.now()
			const tick = () => {
				const value = predicate()
				if (value) {
					resolve(value)
					return
				}
				if (Date.now() - startedAt >= timeoutMs) {
					reject(new Error('timed out waiting for DOM target'))
					return
				}
				setTimeout(tick, intervalMs)
			}
			tick()
		})
	}

	async function clickStartButtonDom() {
		setStatus('searching DOM')
		log('searching DOM for start control')

		const button = await waitFor(() => findCaptionedControl(/^(start|開始|開始辨識)$/iu, /i-lucide:play|lucide:play|aria-label=["']?start["']?/iu))

		log(`found start control: ${describeElement(button)}`)
		clickLikeUser(button)
		await waitFor(() => findCaptionedControl(/^(pause|暫停|停止)$/iu, /i-lucide:pause|lucide:pause|aria-label=["']?pause["']?/iu), 10000)
		setStatus('clicked start')
		log('start state confirmed')
	}

	async function clickPauseButtonDom() {
		setStatus('searching DOM')
		log('searching DOM for pause control')

		const button = await waitFor(() => findCaptionedControl(/^(pause|暫停|停止)$/iu, /i-lucide:pause|lucide:pause|aria-label=["']?pause["']?/iu))

		log(`found pause control: ${describeElement(button)}`)
		clickLikeUser(button)
		await waitFor(() => findCaptionedControl(/^(start|開始|開始辨識)$/iu, /i-lucide:play|lucide:play|aria-label=["']?start["']?/iu), 10000)
		setStatus('clicked pause')
		log('pause state confirmed')
	}

	function setPendingAutoStart(meetingId) {
		localStorage.setItem(PENDING_AUTO_START_KEY, meetingId)
	}

	function clearPendingAutoStart() {
		localStorage.removeItem(PENDING_AUTO_START_KEY)
	}

	function getPendingAutoStart() {
		return localStorage.getItem(PENDING_AUTO_START_KEY) || ''
	}

	async function maybeTriggerPendingAutoStart() {
		const pendingMeetingId = getPendingAutoStart()
		const currentMeetingId = currentMeetingIdFromUrl()
		if (!pendingMeetingId || !currentMeetingId || pendingMeetingId !== currentMeetingId) {
			return
		}

		clearPendingAutoStart()
		log(`pending auto-start matched URL: ${currentMeetingId}`)
		setTimeout(() => {
			clickStartButtonDom()
				.then(() => {
					setAgentState({ status: 'in_progress', lastCommandResult: 'started', currentMeetingId: currentMeetingIdFromUrl() })
				})
				.catch(error => log(error instanceof Error ? error.message : String(error)))
		}, 800)
	}

	async function gotoMeetingByUrl(meetingId) {
		if (!meetingId) {
			throw new Error('missing meeting id')
		}

		const nextUrl = roomUrlForMeetingId(meetingId)
		log(`navigating to ${nextUrl}`)
		setStatus('navigating')
		location.href = nextUrl
	}

	// The server now broadcasts `command` envelopes to agents, so the browser-side
	// handler has to execute them directly. Before this fix the websocket listener
	// crashed at the missing helper and never reached the DOM click path.
	async function handleAgentCommand(command) {
		setAgentState({
			lastCommandId: command.command_id,
			lastCommandResult: 'running',
			lastError: '',
			currentMeetingId: currentMeetingIdFromUrl(),
		})
		log(`command ${command.action} received for ${command.room_name}`)

		switch (command.action) {
			case 'goto':
				setAgentState({ status: 'switching' })
				await gotoMeetingByUrl(command.target_meeting_id)
				setAgentState({ lastCommandResult: 'done' })
				return
			case 'start':
				setAgentState({ status: 'in_progress' })
				await clickStartButtonDom()
				setAgentState({ lastCommandResult: 'started', status: 'in_progress' })
				return
			case 'pause':
				setAgentState({ status: 'paused' })
				await clickPauseButtonDom()
				setAgentState({ lastCommandResult: 'paused', status: 'paused' })
				return
			case 'goto_and_start':
				setAgentState({ status: 'switching' })
				setPendingAutoStart(command.target_meeting_id)
				await gotoMeetingByUrl(command.target_meeting_id)
				setAgentState({ lastCommandResult: 'navigating' })
				return
			default:
				setAgentState({ lastCommandResult: 'ignored' })
				return
		}
	}

	function sendAgentMessage(message) {
		if (!agentSocket || agentSocket.readyState !== WebSocket.OPEN) {
			return false
		}
		agentSocket.send(JSON.stringify(message))
		return true
	}

	function stopHeartbeat() {
		if (heartbeatTimer) {
			clearInterval(heartbeatTimer)
			heartbeatTimer = null
		}
	}

	function startHeartbeat() {
		stopHeartbeat()
		heartbeatTimer = setInterval(() => {
			if (!agentSocket || agentSocket.readyState !== WebSocket.OPEN) {
				return
			}
			const currentMeetingId = currentMeetingIdFromUrl()
			setAgentState({ currentMeetingId })
			sendAgentMessage({
				type: 'agent_heartbeat',
				room_name: getRoomName(),
				agent_id: getAgentId(),
				status: agentState.status,
				current_meeting_id: currentMeetingId,
				timestamp: new Date().toISOString(),
				last_command_id: agentState.lastCommandId,
				last_command_result: agentState.lastCommandResult,
				last_error: agentState.lastError,
			})
		}, 1000)
	}

	function scheduleReconnect() {
		if (agentManuallyDisconnected || agentConnectionTerminal || reconnectTimer) {
			return
		}
		reconnectTimer = setTimeout(() => {
			reconnectTimer = null
			connectAgent()
		}, 2000)
	}

	function disconnectAgent(manual = true) {
		if (manual) {
			agentManuallyDisconnected = true
			agentConnectionTerminal = false
		}
		if (reconnectTimer) {
			clearTimeout(reconnectTimer)
			reconnectTimer = null
		}
		stopHeartbeat()
		if (agentSocket) {
			try {
				agentSocket.close()
			} catch {
				// ignore
			}
			agentSocket = null
		}
		agentConnected = false
		setAgentStatusLabel('disconnected')
	}

	function shouldConnectAgent() {
		return isRoomRoute() && Boolean(agentServerUrlInput.value.trim()) && Boolean(agentRoomNameInput.value.trim())
	}

	function syncAgentConnection() {
		if (shouldConnectAgent()) {
			if (!agentSocket || agentSocket.readyState === WebSocket.CLOSED || agentSocket.readyState === WebSocket.CLOSING) {
				connectAgent()
			}
			return
		}

		// Leaving the room route should stop the agent entirely; the old behavior kept
		// the websocket alive and allowed `/en` to connect to the API server.
		disconnectAgent(true)
	}

	function connectAgent() {
		if (!isRoomRoute()) {
			setAgentStatusLabel('room page only')
			log('agent connection skipped: not on a room page')
			return
		}
		const roomName = getRoomName()
		const serverUrl = getServerUrl()
		if (!roomName) {
			setAgentStatusLabel('missing room name')
			log('agent connection skipped: missing room name')
			return
		}
		if (!serverUrl) {
			setAgentStatusLabel('missing server url')
			log('agent connection skipped: missing server url')
			return
		}

		agentManuallyDisconnected = false
		agentConnectionTerminal = false
		if (reconnectTimer) {
			clearTimeout(reconnectTimer)
			reconnectTimer = null
		}
		if (agentSocket && agentSocket.readyState === WebSocket.OPEN) {
			return
		}

		const ws = new WebSocket(websocketUrlFromServerUrl(serverUrl))
		agentSocket = ws
		setAgentStatusLabel('connecting')

		ws.addEventListener('open', () => {
			agentConnected = true
			setAgentStatusLabel('connected')
			log(`agent connected to ${serverUrl} as ${roomName}`)
			setAgentState({ currentMeetingId: currentMeetingIdFromUrl() })
			sendAgentMessage({
				type: 'agent_hello',
				room_name: roomName,
				agent_id: getAgentId(),
				status: agentState.status,
				current_meeting_id: currentMeetingIdFromUrl(),
				timestamp: new Date().toISOString(),
			})
			startHeartbeat()
		})

		ws.addEventListener('message', event => {
			let message
			try {
				message = JSON.parse(event.data)
			} catch {
				return
			}
			if (!message || message.type !== 'command') {
				return
			}
			if (message.room_name !== getRoomName()) {
				return
			}
			if (!rememberCommand(message.command_id)) {
				return
			}
			handleAgentCommand(message).catch(error => {
				const messageText = error instanceof Error ? error.message : String(error)
				setAgentState({ status: 'error', lastError: messageText, lastCommandId: message.command_id, lastCommandResult: 'failed' })
				log(messageText)
			})
		})

		ws.addEventListener('close', event => {
			stopHeartbeat()
			agentConnected = false
			agentSocket = null
			if (event.code === 4409) {
				agentConnectionTerminal = true
				agentManuallyDisconnected = true
				setAgentState({ status: 'error', lastError: event.reason || 'room already has an agent connected' })
				setAgentStatusLabel('room already connected')
				log(event.reason || 'agent connection failed: room already has an agent connected')
				return
			}
			setAgentStatusLabel('disconnected')
			if (!agentManuallyDisconnected && !agentConnectionTerminal) {
				scheduleReconnect()
			}
		})

		ws.addEventListener('error', () => {
			agentConnected = false
			setAgentState({ status: 'error', lastError: 'websocket error' })
			setAgentStatusLabel('error')
		})

	}

	agentServerUrlInput.value = localStorage.getItem(SERVER_URL_KEY) || 'http://127.0.0.1:8080'
	agentRoomNameInput.value = localStorage.getItem(ROOM_NAME_KEY) || ''
	renderAgentSummary()
	setStatus(isRoomRoute() ? 'ready' : 'room page only')
	log('panel loaded')

	agentServerUrlInput.addEventListener('change', () => {
		localStorage.setItem(SERVER_URL_KEY, agentServerUrlInput.value.trim())
	})
	agentRoomNameInput.addEventListener('change', () => {
		localStorage.setItem(ROOM_NAME_KEY, agentRoomNameInput.value.trim())
	})
	connectButton.addEventListener('click', () => {
		if (!isRoomRoute()) {
			setStatus('room page only')
			return
		}
		localStorage.setItem(SERVER_URL_KEY, agentServerUrlInput.value.trim())
		localStorage.setItem(ROOM_NAME_KEY, agentRoomNameInput.value.trim())
		connectAgent()
	})
	disconnectButton.addEventListener('click', () => {
		disconnectAgent(true)
	})

	syncPanelState()
})()
