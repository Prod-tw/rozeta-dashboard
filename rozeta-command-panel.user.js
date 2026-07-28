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

	const COMMAND_ENDPOINT = '/api/v1/commands'
	const PANEL_ID = 'rozeta-command-panel'
	const SERVER_URL_KEY = 'rozeta-agent-server-url'
	const ROOM_NAME_KEY = 'rozeta-agent-room-name'
	const AGENT_ID_KEY = 'rozeta-agent-id'
	const RECENT_COMMAND_LIMIT = 10
	const STORAGE_KEY = 'rozeta-command-panel-target-id'
	const PENDING_AUTO_START_KEY = 'rozeta-command-panel-pending-auto-start'

	let agentSocket = null
	let heartbeatTimer = null
	let reconnectTimer = null
	let agentManuallyDisconnected = false
	let agentConnected = false
	let recentCommandIds = []
	let agentState = {
		status: 'ready',
		currentMeetingId: '',
		lastCommandId: '',
		lastCommandResult: '',
		lastError: '',
	}

	// Keep the panel on top-right so it stays out of the main meeting controls.
	// The page previously had no in-page controller for remote commands; this adds
	// a lightweight overlay without depending on Rozeta's internal UI structure.
	const panel = document.createElement('div')
	panel.id = PANEL_ID
	panel.style.cssText = [
		'position:fixed',
		'top:12px',
		'right:12px',
		'z-index:2147483647',
		'width:360px',
		'padding:12px',
		'background:rgba(17, 24, 39, 0.96)',
		'color:#f9fafb',
		'border:1px solid rgba(255,255,255,0.12)',
		'border-radius:12px',
		'box-shadow:0 10px 30px rgba(0,0,0,0.35)',
		'font:12px/1.4 system-ui, -apple-system, BlinkMacSystemFont, "Segoe UI", sans-serif',
		'backdrop-filter:blur(8px)'
	].join(';')

	panel.innerHTML = `
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
		<label style="display:block;margin-bottom:8px;">
			<div style="margin-bottom:4px;color:#d1d5db;">Meeting ID</div>
			<input id="rozeta-command-panel-target" type="text" autocomplete="off" spellcheck="false"
				style="width:100%;box-sizing:border-box;padding:8px 10px;border-radius:8px;border:1px solid rgba(255,255,255,0.16);background:#111827;color:#f9fafb;outline:none;" />
		</label>
		<div style="display:grid;grid-template-columns:repeat(2, minmax(0, 1fr));gap:8px;margin-bottom:8px;">
			<button type="button" id="rozeta-command-panel-goto-url" style="padding:8px 10px;border:0;border-radius:8px;background:#2563eb;color:white;font-weight:600;cursor:pointer;">Goto URL</button>
			<button type="button" id="rozeta-command-panel-start-dom" style="padding:8px 10px;border:0;border-radius:8px;background:#16a34a;color:white;font-weight:600;cursor:pointer;">Start DOM</button>
			<button type="button" id="rozeta-command-panel-goto-start" style="padding:8px 10px;border:0;border-radius:8px;background:#7c3aed;color:white;font-weight:600;cursor:pointer;">Goto + Start</button>
			<button type="button" id="rozeta-command-panel-pause-dom" style="padding:8px 10px;border:0;border-radius:8px;background:#dc2626;color:white;font-weight:600;cursor:pointer;">Pause DOM</button>
			<button type="button" id="rozeta-command-panel-sync" style="padding:8px 10px;border:0;border-radius:8px;background:#374151;color:white;font-weight:600;cursor:pointer;">Sync URL</button>
		</div>
		<div style="display:grid;grid-template-columns:repeat(2, minmax(0, 1fr));gap:8px;margin-bottom:8px;">
			<button type="button" id="rozeta-command-panel-goto-api" style="padding:8px 10px;border:0;border-radius:8px;background:#1d4ed8;color:white;font-weight:600;cursor:pointer;">Goto API</button>
			<button type="button" id="rozeta-command-panel-start-api" style="padding:8px 10px;border:0;border-radius:8px;background:#15803d;color:white;font-weight:600;cursor:pointer;">Start API</button>
		</div>
		<pre id="rozeta-command-panel-log" style="margin:0;max-height:180px;overflow:auto;padding:8px;border-radius:8px;background:#030712;color:#cbd5e1;white-space:pre-wrap;word-break:break-word;"></pre>
	`

	let panelReattachObserver = null

	function shouldShowPanel() {
		return Boolean(currentMeetingIdFromUrl())
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

	function startPanelReattachObserver() {
		if (panelReattachObserver) {
			return
		}

		panelReattachObserver = new MutationObserver(() => {
			if (shouldShowPanel()) {
				mountPanel()
			} else {
				unmountPanel()
			}
		})

		const observeTarget = document.documentElement
		if (observeTarget) {
			panelReattachObserver.observe(observeTarget, { childList: true, subtree: true })
		}
	}

	syncPanelMount()
	startPanelReattachObserver()

	const targetInput = panel.querySelector('#rozeta-command-panel-target')
	const statusNode = panel.querySelector('#rozeta-command-panel-status')
	const agentStatusNode = panel.querySelector('#rozeta-agent-connection-status')
	const agentServerUrlInput = panel.querySelector('#rozeta-agent-server-url')
	const agentRoomNameInput = panel.querySelector('#rozeta-agent-room-name')
	const agentSummaryNode = panel.querySelector('#rozeta-agent-summary')
	const logNode = panel.querySelector('#rozeta-command-panel-log')

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

	function roomUrlForMeetingId(meetingId) {
		return `https://rozeta.app/en/meetings/${encodeURIComponent(meetingId)}/room`
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
		if (agentManuallyDisconnected || reconnectTimer) {
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

	function connectAgent() {
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

		ws.addEventListener('close', () => {
			stopHeartbeat()
			agentConnected = false
			setAgentStatusLabel('disconnected')
			if (!agentManuallyDisconnected) {
				scheduleReconnect()
			}
		})

		ws.addEventListener('error', () => {
			agentConnected = false
			setAgentStatusLabel('error')
		})
	}

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

	function findCaptionedControl(labelPattern, iconPattern) {
		return findCaptionButton(labelPattern) || findIconButton(iconPattern) || findButtonByText(labelPattern)
	}

	function findIconButton(iconPattern) {
		const buttons = Array.from(document.querySelectorAll('button'))
		return buttons.find(button => {
			if (button.closest(`#${PANEL_ID}`)) return false
			return iconPattern.test(button.innerHTML) || iconPattern.test(getClickableLabel(button))
		}) || null
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

	async function gotoMeetingByUrl(meetingId) {
		if (!meetingId) {
			throw new Error('missing meeting id')
		}

		const nextUrl = roomUrlForMeetingId(meetingId)
		log(`navigating to ${nextUrl}`)
		setStatus('navigating')
		location.href = nextUrl
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

	function syncInputFromUrl() {
		const meetingId = currentMeetingIdFromUrl()
		if (meetingId) {
			targetInput.value = meetingId
			localStorage.setItem(STORAGE_KEY, meetingId)
			log(`synced meeting id from URL: ${meetingId}`)
		}
	}

	async function sendCommand(action, targetId) {
		if (!targetId) {
			throw new Error('missing meeting id')
		}

		setStatus('sending')
		log(`POST ${COMMAND_ENDPOINT} action=${action} target_id=${targetId}`)

		const response = await fetch(COMMAND_ENDPOINT, {
			method: 'POST',
			credentials: 'include',
			headers: {
				'content-type': 'application/json',
			},
			body: JSON.stringify({ action, target_id: targetId }),
		})

		const contentType = response.headers.get('content-type') || ''
		const body = contentType.includes('application/json') ? await response.json().catch(() => null) : await response.text()

		if (!response.ok) {
			throw new Error(typeof body === 'string' ? `${response.status} ${response.statusText}: ${body}` : `${response.status} ${response.statusText}: ${JSON.stringify(body)}`)
		}

		setStatus('sent')
		log(`success: ${action}`)
		return body
	}

	function bindButton(selector, handler) {
		const button = panel.querySelector(selector)
		if (!button) {
			throw new Error(`missing panel button: ${selector}`)
		}
		button.addEventListener('click', async () => {
			try {
				button.disabled = true
				await handler()
			} catch (error) {
				setStatus('error')
				log(error instanceof Error ? error.message : String(error))
			} finally {
				button.disabled = false
			}
		})
	}

	bindButton('#rozeta-command-panel-goto-api', async () => {
		await sendCommand('goto_meeting', targetInput.value.trim())
	})

	bindButton('#rozeta-command-panel-start-api', async () => {
		const meetingId = targetInput.value.trim() || currentMeetingIdFromUrl()
		await sendCommand('start_meeting', meetingId)
	})

	bindButton('#rozeta-command-panel-goto-url', async () => {
		setAgentState({ status: 'switching', currentMeetingId: targetInput.value.trim() || currentMeetingIdFromUrl() })
		await gotoMeetingByUrl(targetInput.value.trim())
	})

	bindButton('#rozeta-command-panel-start-dom', async () => {
		await clickStartButtonDom()
		setAgentState({ status: 'in_progress', lastCommandResult: 'started', currentMeetingId: currentMeetingIdFromUrl() })
	})

	bindButton('#rozeta-command-panel-pause-dom', async () => {
		await clickPauseButtonDom()
		setAgentState({ status: 'paused', lastCommandResult: 'paused', currentMeetingId: currentMeetingIdFromUrl() })
	})

	bindButton('#rozeta-command-panel-goto-start', async () => {
		const meetingId = targetInput.value.trim()
		// The previous flow relied on command API events, which did not move the room.
		// This new flow switches the URL directly and then waits for the new page DOM
		// to expose the Start button so we can click it like a user would.
		setAgentState({ status: 'switching', currentMeetingId: meetingId })
		setPendingAutoStart(meetingId)
		await gotoMeetingByUrl(meetingId)
	})

	bindButton('#rozeta-command-panel-sync', async () => {
		syncInputFromUrl()
		setStatus('synced')
	})

	const savedTargetId = localStorage.getItem(STORAGE_KEY)
	targetInput.value = savedTargetId || currentMeetingIdFromUrl() || ''
	agentServerUrlInput.value = localStorage.getItem(SERVER_URL_KEY) || 'http://127.0.0.1:8080'
	agentRoomNameInput.value = localStorage.getItem(ROOM_NAME_KEY) || ''
	syncInputFromUrl()
	renderAgentSummary()
	setStatus('ready')
	log('panel loaded')

	agentServerUrlInput.addEventListener('change', () => {
		localStorage.setItem(SERVER_URL_KEY, agentServerUrlInput.value.trim())
	})
	agentRoomNameInput.addEventListener('change', () => {
		localStorage.setItem(ROOM_NAME_KEY, agentRoomNameInput.value.trim())
	})
	panel.querySelector('#rozeta-agent-connect').addEventListener('click', () => {
		localStorage.setItem(SERVER_URL_KEY, agentServerUrlInput.value.trim())
		localStorage.setItem(ROOM_NAME_KEY, agentRoomNameInput.value.trim())
		connectAgent()
	})
	panel.querySelector('#rozeta-agent-disconnect').addEventListener('click', () => {
		disconnectAgent(true)
	})

	if (agentServerUrlInput.value.trim() && agentRoomNameInput.value.trim()) {
		connectAgent()
	}
	maybeTriggerPendingAutoStart().catch(error => log(error instanceof Error ? error.message : String(error)))

	// Keep the panel in sync with manual URL changes so you can test goto_meeting
	// while the page stays open. The old behavior was no controller at all; now
	// the input follows the currently loaded room unless you override it.
	let lastPath = location.pathname
	setInterval(() => {
		if (location.pathname !== lastPath) {
			lastPath = location.pathname
			syncPanelMount()
			syncInputFromUrl()
			setAgentState({ currentMeetingId: currentMeetingIdFromUrl() })
			maybeTriggerPendingAutoStart().catch(error => log(error instanceof Error ? error.message : String(error)))
		}
	}, 1000)
})()
