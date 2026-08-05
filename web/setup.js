const roomSelect = document.getElementById('room-select')
const generateButton = document.getElementById('generate-button')
const meetingPreview = document.getElementById('meeting-preview')
const setupError = document.getElementById('setup-error')
const results = document.getElementById('results')
const resultRoom = document.getElementById('result-room')
const cookieScript = document.getElementById('cookie-script')
const obsURL = document.getElementById('obs-url')
const copyOBSButton = document.getElementById('copy-obs-button')
const copyStatus = document.getElementById('copy-status')

async function apiFetch(url, options = {}) {
	const response = await fetch(url, options)
	if (response.status === 401) {
		const redirect = `${window.location.pathname}${window.location.search}`
		window.location.assign(`/login?redirect=${encodeURIComponent(redirect)}`)
		throw new Error('登入狀態已失效。')
	}
	const body = await response.json().catch(() => ({}))
	if (!response.ok) throw new Error(body.error || `伺服器回傳 ${response.status}。`)
	return body
}

async function loadRooms() {
	try {
		const body = await apiFetch('/api/rooms')
		const rooms = Array.isArray(body.rooms) ? body.rooms : []
		roomSelect.replaceChildren(new Option(rooms.length ? '請選擇房間' : '沒有可用房間', ''))
		for (const room of rooms) roomSelect.add(new Option(room.room_name, room.room_name))
		roomSelect.disabled = rooms.length === 0
		generateButton.disabled = true
	} catch (error) {
		showError(error)
	}
}

function showError(error) {
	setupError.textContent = error instanceof Error ? error.message : String(error)
}

roomSelect.addEventListener('change', () => {
	const selected = roomSelect.value
	generateButton.disabled = !selected
	results.hidden = true
	setupError.textContent = ''
	meetingPreview.textContent = selected
		? '產生後會使用該房間排序後的第一個議程。'
		: '選擇房間後，會使用該房間排序後的第一個議程。'
})

generateButton.addEventListener('click', async () => {
	generateButton.disabled = true
	generateButton.classList.add('loading')
	setupError.textContent = ''
	results.hidden = true
	try {
		const body = await apiFetch('/api/setup/artifacts', {
			method: 'POST',
			headers: { 'content-type': 'application/json' },
			body: JSON.stringify({ room_name: roomSelect.value }),
		})
		resultRoom.textContent = body.room_name
		cookieScript.value = body.cookie_script || ''
		obsURL.value = body.obs_url || ''
		copyOBSButton.disabled = !body.obs_url
		results.hidden = false
	} catch (error) {
		showError(error)
	} finally {
		generateButton.disabled = !roomSelect.value
		generateButton.classList.remove('loading')
	}
})

document.querySelectorAll('[data-copy-target]').forEach(button => {
	button.addEventListener('click', async () => {
		const target = document.getElementById(button.dataset.copyTarget)
		if (!target?.value) return
		try {
			await navigator.clipboard.writeText(target.value)
			copyStatus.textContent = '已複製。'
		} catch {
			target.focus()
			target.select()
			copyStatus.textContent = '無法自動複製，請按 Ctrl/Cmd+C。'
		}
	})
})

void loadRooms()
