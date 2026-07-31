const tooltip = document.createElement('div')
tooltip.id = 'global-tooltip'
tooltip.className = 'tooltip'
tooltip.setAttribute('role', 'tooltip')
tooltip.hidden = true
document.body.append(tooltip)

let tooltipTarget = null

function showTooltip(target) {
	const text = target.dataset.tooltip?.trim()
	if (!text) return

	tooltipTarget = target
	tooltip.textContent = text
	tooltip.hidden = false
	const describedBy = new Set((target.getAttribute('aria-describedby') || '').split(/\s+/u).filter(Boolean))
	describedBy.add(tooltip.id)
	target.setAttribute('aria-describedby', Array.from(describedBy).join(' '))

	const targetRect = target.getBoundingClientRect()
	const tooltipRect = tooltip.getBoundingClientRect()
	const gap = 10
	const edge = 8
	let top = targetRect.top - tooltipRect.height - gap
	if (top < edge) top = targetRect.bottom + gap
	const left = Math.min(
		Math.max(targetRect.left + targetRect.width / 2 - tooltipRect.width / 2, edge),
		window.innerWidth - tooltipRect.width - edge,
	)
	tooltip.style.transform = `translate(${Math.round(left)}px, ${Math.round(top)}px)`
}

function hideTooltip(target) {
	if (target && tooltipTarget !== target) return
	if (tooltipTarget) {
		const describedBy = (tooltipTarget.getAttribute('aria-describedby') || '')
			.split(/\s+/u)
			.filter(id => id && id !== tooltip.id)
		if (describedBy.length) {
			tooltipTarget.setAttribute('aria-describedby', describedBy.join(' '))
		} else {
			tooltipTarget.removeAttribute('aria-describedby')
		}
	}
	tooltipTarget = null
	tooltip.hidden = true
}

function findTooltipTarget(node) {
	return node instanceof Element ? node.closest('[data-tooltip]') : null
}

// Tooltips previously did not exist. Delegated hover and focus handling now covers static and dynamically rendered
// controls without rebinding listeners, while Escape gives keyboard and touch users a predictable way to dismiss them.
document.addEventListener('pointerover', event => {
	const target = findTooltipTarget(event.target)
	if (target) showTooltip(target)
})
document.addEventListener('pointerout', event => {
	const target = findTooltipTarget(event.target)
	if (target && !target.contains(event.relatedTarget)) hideTooltip(target)
})
document.addEventListener('focusin', event => {
	const target = findTooltipTarget(event.target)
	if (target) showTooltip(target)
})
document.addEventListener('focusout', event => {
	const target = findTooltipTarget(event.target)
	if (target && !target.contains(event.relatedTarget)) hideTooltip(target)
})
document.addEventListener('keydown', event => {
	if (event.key === 'Escape') hideTooltip()
})
window.addEventListener('resize', () => hideTooltip())
window.addEventListener('scroll', () => hideTooltip(), true)
