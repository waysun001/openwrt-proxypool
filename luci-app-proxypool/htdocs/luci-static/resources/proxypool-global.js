(function() {
    'use strict';

    // 注入菜单样式
    injectCSS();

    if (document.readyState === 'loading') {
        document.addEventListener('DOMContentLoaded', build);
    } else {
        build();
    }

    function build() {
        if (document.getElementById('proxypool-global-menu')) return;

        var menuItems = [
            { label: '\u667A\u8054\u76D2\u5B50', href: '/cgi-bin/luci/admin/services/proxypool' },
            { label: '\u7CFB\u7EDF\u65E5\u5FD7', href: '/cgi-bin/luci/admin/services/proxypool?tab=log' },
            { label: '\u4FE1\u9053\u5206\u6790', href: '/cgi-bin/luci/admin/network/wireless' },
            { label: '\u5907\u4EFD\u4E0E\u5347\u7EA7', href: '/cgi-bin/luci/admin/system/flash' },
            { label: '\u65E0\u7EBF', href: '/cgi-bin/luci/admin/network/wireless' },
            { label: '\u91CD\u542F', href: '/cgi-bin/luci/admin/system/reboot' }
        ];

        var menu = document.createElement('div');
        menu.id = 'proxypool-global-menu';

        var linksDiv = document.createElement('div');
        linksDiv.className = 'menu-links';

        var path = window.location.pathname.replace(/\/+$/, '');
        var search = window.location.search || '';

        menuItems.forEach(function(item) {
            var a = document.createElement('a');
            a.href = item.href;
            a.textContent = item.label;

            var qIdx = item.href.indexOf('?');
            var itemPath = qIdx >= 0 ? item.href.substring(0, qIdx) : item.href;
            var itemSearch = qIdx >= 0 ? item.href.substring(qIdx) : '';

            if (itemSearch) {
                if (path === itemPath && search === itemSearch) a.className = 'active';
            } else {
                if (path === itemPath && !search) a.className = 'active';
            }

            linksDiv.appendChild(a);
        });

        var statsDiv = document.createElement('div');
        statsDiv.className = 'menu-stats';
        statsDiv.innerHTML =
            '<span>\u603B <strong id="pp-stat-total">-</strong></span>' +
            '<span class="stat-connected">\u5DF2\u8FDE\u63A5 <strong id="pp-stat-connected">-</strong></span>' +
            '<span class="stat-disconnected">\u672A\u8FDE\u63A5 <strong id="pp-stat-disconnected">-</strong></span>';

        menu.appendChild(linksDiv);
        menu.appendChild(statsDiv);

        var anchor = document.querySelector('#maincontent') ||
                     document.querySelector('.main-right') ||
                     document.querySelector('.main') ||
                     document.querySelector('#content');
        if (anchor && anchor.parentNode) {
            anchor.parentNode.insertBefore(menu, anchor);
        } else {
            document.body.insertBefore(menu, document.body.firstChild);
        }

        updateStats();
        setInterval(updateStats, 10000);
    }

    function injectCSS() {
        if (document.getElementById('pp-global-menu-css')) return;
        var style = document.createElement('style');
        style.id = 'pp-global-menu-css';
        style.textContent =
            '#proxypool-global-menu{display:flex;justify-content:space-between;align-items:center;background:#f8f9fa;padding:10px 20px;border-bottom:2px solid #ddd;font-size:14px;box-shadow:0 2px 4px rgba(0,0,0,0.1);max-width:1840px;margin:0 auto}' +
            '#proxypool-global-menu .menu-links{display:flex;gap:10px;flex-wrap:wrap}' +
            '#proxypool-global-menu .menu-links a{padding:8px 16px;background:#fff;color:#333;border:1px solid #ddd;border-radius:4px;text-decoration:none;transition:all 0.2s;font-weight:500}' +
            '#proxypool-global-menu .menu-links a:hover{background:#007bff;color:#fff;border-color:#007bff}' +
            '#proxypool-global-menu .menu-links a.active{background:#007bff;color:#fff;border-color:#007bff}' +
            '#proxypool-global-menu .menu-stats{display:flex;gap:15px;font-size:13px;white-space:nowrap}' +
            '#proxypool-global-menu .menu-stats span{color:#666}' +
            '#proxypool-global-menu .menu-stats strong{font-weight:600}' +
            '#proxypool-global-menu .menu-stats .stat-connected{color:#28a745}' +
            '#proxypool-global-menu .menu-stats .stat-disconnected{color:#dc3545}';
        document.head.appendChild(style);
    }

    function updateStats() {
        fetch('/cgi-bin/luci/admin/services/proxypool/api?action=status')
            .then(function(r) { return r.json(); })
            .then(function(data) {
                if (data && data.summary) {
                    var el;
                    el = document.getElementById('pp-stat-total');
                    if (el) el.textContent = data.summary.total;
                    el = document.getElementById('pp-stat-connected');
                    if (el) el.textContent = data.summary.connected;
                    el = document.getElementById('pp-stat-disconnected');
                    if (el) el.textContent = data.summary.disconnected;
                }
            })
            .catch(function() {});
    }
})();
