(function() {
    'use strict';

    // 默认首页：从 LuCI 默认落地页重定向到智联盒子
    var path = window.location.pathname.replace(/\/+$/, '');
    if (path === '/cgi-bin/luci' ||
        path === '/cgi-bin/luci/admin' ||
        path === '/cgi-bin/luci/admin/status' ||
        path === '/cgi-bin/luci/admin/status/overview') {
        window.location.replace(path.replace(/\/admin.*/, '/admin/services/proxypool'));
        return;
    }

    document.addEventListener('DOMContentLoaded', function() {
        // main.htm 已自行渲染 topnav 时跳过
        if (document.getElementById('pp-topnav')) return;

        buildTopNav();
    });

    function buildTopNav() {
        var targets = [
            { label: '智联盒子', keywords: ['智联盒子'] },
            { label: '信道分析', keywords: ['信道分析'] },
            { label: '备份与升级', keywords: ['备份与升级', '备份/升级'] },
            { label: '无线', keywords: ['无线'] },
            { label: '重启', keywords: ['重启'] }
        ];

        // 从隐藏的侧边栏 DOM 中发现真实链接 URL
        var allLinks = document.querySelectorAll('a[href]');
        var navItems = [];

        targets.forEach(function(t) {
            var href = null;
            // 精确匹配优先
            for (var i = 0; i < allLinks.length && !href; i++) {
                var text = allLinks[i].textContent.replace(/\s+/g, '');
                for (var k = 0; k < t.keywords.length; k++) {
                    if (text === t.keywords[k]) {
                        href = allLinks[i].href;
                        break;
                    }
                }
            }
            // 包含匹配兜底
            if (!href) {
                for (var i = 0; i < allLinks.length && !href; i++) {
                    var text = allLinks[i].textContent.replace(/\s+/g, '');
                    for (var k = 0; k < t.keywords.length; k++) {
                        if (text.indexOf(t.keywords[k]) !== -1 && allLinks[i].href.indexOf('/cgi-bin/') !== -1) {
                            href = allLinks[i].href;
                            break;
                        }
                    }
                }
            }
            if (href) {
                navItems.push({ label: t.label, href: href });
            }
        });

        if (navItems.length === 0) return;

        // 创建导航栏
        var nav = document.createElement('div');
        nav.id = 'pp-topnav';

        navItems.forEach(function(item) {
            var a = document.createElement('a');
            a.href = item.href;
            a.textContent = item.label;
            var loc = window.location.href.replace(/\/$/, '');
            var target = item.href.replace(/\/$/, '');
            if (loc === target || loc.indexOf(target + '/') === 0) {
                a.className = 'pp-nav-active';
            }
            nav.appendChild(a);
        });

        // 插入到页面顶部
        var anchor = document.querySelector('#maincontent') ||
                     document.querySelector('.main-right') ||
                     document.querySelector('.main');
        if (anchor && anchor.parentNode) {
            anchor.parentNode.insertBefore(nav, anchor);
        } else {
            document.body.insertBefore(nav, document.body.firstChild);
        }
    }
})();
