(function() {
	'use strict';

	function setText(target, value) {
		if (target) target.textContent = String(value);
	}

	function updateStatus() {
		var menu = document.getElementById('proxypool-global-menu');
		if (!menu || typeof window.fetch !== 'function') return;
		var statusURL = menu.getAttribute('data-status-url');
		var totalTarget = document.getElementById('pp-stat-total');
		var connectedTarget = document.getElementById('pp-stat-connected');
		var disconnectedTarget = document.getElementById('pp-stat-disconnected');
		if (!statusURL) return;

		window.fetch(statusURL, {
			credentials: 'same-origin',
			headers: { Accept: 'application/json' }
		}).then(function(response) {
			if (!response.ok) throw new Error('status unavailable');
			return response.json();
		}).then(function(envelope) {
			var data = envelope && envelope.success && envelope.result;
			if (!data) return;
			var desired = data.desired && data.desired.nodes || [];
			var runtime = data.runtime && data.runtime.nodes || [];
			var online = runtime.filter(function(node) { return node.state === 'online'; }).length;
			setText(totalTarget, desired.length);
			setText(connectedTarget, online);
			setText(disconnectedTarget, Math.max(0, desired.length - online));
		}).catch(function() {});
	}

	if (document.readyState === 'loading') document.addEventListener('DOMContentLoaded', updateStatus);
	else updateStatus();
})();
