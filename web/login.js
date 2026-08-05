const form = document.getElementById('login-form')
const passwordInput = document.getElementById('password')
const loginButton = document.getElementById('login-button')
const errorNode = document.getElementById('login-error')
const redirect = new URLSearchParams(window.location.search).get('redirect') || '/'

const loginErrorMessages = {
	'too many login attempts': '登入嘗試次數過多，請稍後再試。',
	'invalid login request': '登入資料格式不正確。',
	'invalid password': '密碼不正確。',
	'failed to create session': '無法建立登入階段，請稍後再試。',
}

form.addEventListener('submit', async event => {
	event.preventDefault()
	loginButton.disabled = true
	loginButton.classList.add('loading')
	errorNode.textContent = ''

	try {
		const response = await fetch('/api/login', {
			method: 'POST',
			headers: { 'content-type': 'application/json' },
			body: JSON.stringify({ password: passwordInput.value, redirect }),
		})
		const body = await response.json().catch(() => null)
		if (!response.ok) {
			throw new Error(body?.error || 'sign in failed')
		}
		window.location.assign(body?.redirect || '/')
	} catch (error) {
		const message = error instanceof Error ? error.message : String(error)
		errorNode.textContent = loginErrorMessages[message] || `登入失敗。技術資訊：${message}`
		loginButton.disabled = false
		loginButton.classList.remove('loading')
		passwordInput.select()
	}
})
