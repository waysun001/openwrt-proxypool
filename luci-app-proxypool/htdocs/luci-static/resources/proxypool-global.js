(function() {
    'use strict';

    function ready(callback) {
        if (document.readyState === 'loading') document.addEventListener('DOMContentLoaded', callback);
        else callback();
    }

    function setText(id, value) {
        var target = document.getElementById(id);
        if (target) target.textContent = String(value);
    }

    function enhanceMenu() {
        var menu = document.getElementById('proxypool-global-menu');
        if (!menu) return;
        document.title = 'ProxyPool';
        var statusURL = menu.getAttribute('data-status-url');
        if (!statusURL || typeof window.fetch !== 'function') return;
        window.fetch(statusURL, { credentials: 'same-origin', headers: { Accept: 'application/json' } })
            .then(function(response) { return response.json(); })
            .then(function(envelope) {
                var data = envelope && envelope.success && envelope.result;
                if (!data) return;
                var desired = data.desired && data.desired.nodes || [];
                var runtime = data.runtime && data.runtime.nodes || [];
                var online = runtime.filter(function(node) { return node.state === 'online'; }).length;
                setText('pp-stat-total', desired.length);
                setText('pp-stat-connected', online);
                setText('pp-stat-disconnected', Math.max(0, desired.length - online));
            })
            .catch(function() {});
    }

    ready(enhanceMenu);
})();
