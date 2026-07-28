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

	let agentSocket = null
	let heartbeatTimer = null
	let reconnectTimer = null
	let agentManuallyDisconnected = false
	let agentConnected = false
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

	agentServerUrlInput.value = localStorage.getItem(SERVER_URL_KEY) || 'http://127.0.0.1:8080'
	agentRoomNameInput.value = localStorage.getItem(ROOM_NAME_KEY) || ''
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
})()
