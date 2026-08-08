(function() {
	'use strict';

	function setText(id, value) {
		var target = document.getElementById(id);
		if (target) target.textContent = String(value);
	}

	function updateStatus() {
		var menu = document.getElementById('proxypool-global-menu');
		if (!menu || typeof window.fetch !== 'function') return;
		var statusURL = menu.getAttribute('data-status-url');
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
			setText('pp-stat-total', desired.length);
			setText('pp-stat-connected', online);
			setText('pp-stat-disconnected', Math.max(0, desired.length - online));
		}).catch(function() {});
	}

	if (document.readyState === 'loading') document.addEventListener('DOMContentLoaded', updateStatus);
	else updateStatus();
})();
