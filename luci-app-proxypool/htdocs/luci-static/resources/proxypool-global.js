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
            { label: '\u4FE1\u9053\u5206\u6790', href: '/cgi-bin/luci/admin/status/channel_analysis' },
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

        // 覆盖页面标题为"智联盒子"，并锁定防止 LuCI 动态改回
        document.title = '智联盒子';
        var titleEl = document.querySelector('title');
        if (titleEl) {
            new MutationObserver(function() {
                if (document.title !== '智联盒子') document.title = '智联盒子';
            }).observe(titleEl, { childList: true, characterData: true, subtree: true });
        }

        updateStats();
        // 去重：主页（/proxypool 无 tab= 参数）已有自己的 10s 轮询，跳过 global.js 的轮询
        var path = window.location.pathname.replace(/\/+$/, '');
        var search = window.location.search || '';
        var isMainPage = (path.indexOf('/proxypool') >= 0) && (search.indexOf('tab=') < 0);
        if (!isMainPage) {
            setInterval(updateStats, 10000);
        }
    }

    function injectCSS() {
        if (document.getElementById('pp-global-menu-css')) return;
        var style = document.createElement('style');
        style.id = 'pp-global-menu-css';
        style.textContent =
            '#proxypool-global-menu{display:flex;justify-content:space-between;align-items:center;background:linear-gradient(135deg,#1a1a2e 0%,#16213e 100%);padding:0 24px;height:52px;font-size:14px;box-shadow:0 2px 8px rgba(0,0,0,0.15);max-width:1840px;margin:0 auto;border-radius:0 0 8px 8px}' +
            '#proxypool-global-menu .menu-links{display:flex;gap:4px;flex-wrap:wrap;align-items:center;height:100%}' +
            '#proxypool-global-menu .menu-links a{padding:6px 18px;background:transparent;color:rgba(255,255,255,0.75);border:none;border-radius:6px;text-decoration:none;transition:all 0.2s ease;font-weight:500;font-size:14px;letter-spacing:0.3px}' +
            '#proxypool-global-menu .menu-links a:hover{background:rgba(255,255,255,0.12);color:#fff}' +
            '#proxypool-global-menu .menu-links a.active{background:rgba(255,255,255,0.18);color:#fff;box-shadow:inset 0 -2px 0 #4fc3f7}' +
            '#proxypool-global-menu .menu-stats{display:flex;gap:16px;font-size:13px;white-space:nowrap;align-items:center}' +
            '#proxypool-global-menu .menu-stats span{color:rgba(255,255,255,0.6)}' +
            '#proxypool-global-menu .menu-stats strong{font-weight:700;font-size:15px;color:rgba(255,255,255,0.9)}' +
            '#proxypool-global-menu .menu-stats .stat-connected{color:#66bb6a}' +
            '#proxypool-global-menu .menu-stats .stat-connected strong{color:#66bb6a}' +
            '#proxypool-global-menu .menu-stats .stat-disconnected{color:#ef5350}' +
            '#proxypool-global-menu .menu-stats .stat-disconnected strong{color:#ef5350}';
        document.head.appendChild(style);
    }

    function safeJson(r) {
        if (!r.ok) return Promise.reject(new Error('HTTP ' + r.status));
        return r.text().then(function(text) {
            if (!text || !text.trim()) return Promise.reject(new Error('Empty response'));
            try { return JSON.parse(text); } catch (e) { return Promise.reject(e); }
        });
    }

    function updateStats() {
        fetch('/cgi-bin/luci/admin/services/proxypool/api?action=status')
            .then(safeJson)
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
