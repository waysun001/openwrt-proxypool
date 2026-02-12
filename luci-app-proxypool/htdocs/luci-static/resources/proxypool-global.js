(function() {
    'use strict';

    // 等 DOM 就绪后构建顶部导航
    if (document.readyState === 'loading') {
        document.addEventListener('DOMContentLoaded', tryBuild);
    } else {
        tryBuild();
    }

    function tryBuild() {
        if (document.getElementById('pp-topnav')) return;
        buildTopNav(0);
    }

    function buildTopNav(attempt) {
        var targets = [
            { label: '\u667A\u8054\u76D2\u5B50', keywords: ['\u667A\u8054\u76D2\u5B50'] },
            { label: '\u4FE1\u9053\u5206\u6790', keywords: ['\u4FE1\u9053\u5206\u6790'] },
            { label: '\u5907\u4EFD\u4E0E\u5347\u7EA7', keywords: ['\u5907\u4EFD\u4E0E\u5347\u7EA7', '\u5907\u4EFD/\u5347\u7EA7'] },
            { label: '\u65E0\u7EBF', keywords: ['\u65E0\u7EBF'] },
            { label: '\u91CD\u542F', keywords: ['\u91CD\u542F'] }
        ];

        var allLinks = document.querySelectorAll('a[href]');
        var navItems = [];

        targets.forEach(function(t) {
            var href = null;
            // 精确匹配
            for (var i = 0; i < allLinks.length && !href; i++) {
                var text = allLinks[i].textContent.replace(/\s+/g, '');
                for (var k = 0; k < t.keywords.length; k++) {
                    if (text === t.keywords[k]) { href = allLinks[i].href; break; }
                }
            }
            // 包含匹配兜底
            if (!href) {
                for (var i = 0; i < allLinks.length && !href; i++) {
                    var text = allLinks[i].textContent.replace(/\s+/g, '');
                    for (var k = 0; k < t.keywords.length; k++) {
                        if (text.indexOf(t.keywords[k]) !== -1 && allLinks[i].href.indexOf('/cgi-bin/') !== -1) {
                            href = allLinks[i].href; break;
                        }
                    }
                }
            }
            if (href) navItems.push({ label: t.label, href: href });
        });

        // JS版LuCI菜单可能延迟渲染，重试最多20次（10秒）
        if (navItems.length === 0 && attempt < 20) {
            setTimeout(function() { buildTopNav(attempt + 1); }, 500);
            return;
        }
        if (navItems.length === 0) return;

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

        var anchor = document.querySelector('#maincontent') ||
                     document.querySelector('.main-right') ||
                     document.querySelector('.main') ||
                     document.querySelector('#content');
        if (anchor && anchor.parentNode) {
            anchor.parentNode.insertBefore(nav, anchor);
        } else {
            document.body.insertBefore(nav, document.body.firstChild);
        }
    }
})();
