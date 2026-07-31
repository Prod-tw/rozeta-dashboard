const form = document.getElementById('login-form')
const passwordInput = document.getElementById('password')
const loginButton = document.getElementById('login-button')
const errorNode = document.getElementById('login-error')

form.addEventListener('submit', async event => {
	event.preventDefault()
	loginButton.disabled = true
	loginButton.classList.add('loading')
	errorNode.textContent = ''

	try {
		const response = await fetch('/api/login', {
			method: 'POST',
			headers: { 'content-type': 'application/json' },
			body: JSON.stringify({ password: passwordInput.value }),
		})
		const body = await response.json().catch(() => null)
		if (!response.ok) {
			throw new Error(body?.error || 'Sign in failed')
		}
		window.location.assign('/')
	} catch (error) {
		errorNode.textContent = error instanceof Error ? error.message : String(error)
		loginButton.disabled = false
		loginButton.classList.remove('loading')
		passwordInput.select()
	}
})
