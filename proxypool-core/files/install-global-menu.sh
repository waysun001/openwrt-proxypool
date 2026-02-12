#!/bin/sh
# ProxyPool 全局菜单安装脚本
# 修改 LuCI header.htm，替换原有菜单为自定义快捷导航和统计

HEADER_FILE="/usr/lib/lua/luci/view/cbi/header.htm"
BACKUP_FILE="${HEADER_FILE}.proxypool-backup"

# 检查 header.htm 是否存在
if [ ! -f "$HEADER_FILE" ]; then
    echo "错误：找不到 LuCI header.htm 文件"
    exit 1
fi

# 备份原文件（如果还没备份过）
if [ ! -f "$BACKUP_FILE" ]; then
    echo "备份原始 header.htm..."
    cp "$HEADER_FILE" "$BACKUP_FILE"
fi

# 检查是否已经安装过
if grep -q "proxypool-global-menu" "$HEADER_FILE"; then
    echo "全局菜单已经安装，跳过"
    exit 0
fi

# 查找插入位置（在 </head> 之前插入样式和脚本）
if ! grep -q "</head>" "$HEADER_FILE"; then
    echo "错误：header.htm 格式不符合预期（找不到 </head> 标签）"
    exit 1
fi

echo "安装全局菜单..."

# 插入样式和脚本（隐藏 LuCI 原有菜单）
sed -i '/<\/head>/i\
<!-- ProxyPool Global Menu -->\
<style>\
/* 隐藏 LuCI 原有菜单 */\
#mainnav, .mainmenu, .main > .luci, .main > div[class*="menu"] { display: none !important; }\
\
#proxypool-global-menu {\
    display: flex;\
    justify-content: space-between;\
    align-items: center;\
    background: #f8f9fa;\
    padding: 10px 20px;\
    border-bottom: 2px solid #ddd;\
    font-size: 14px;\
    box-shadow: 0 2px 4px rgba(0,0,0,0.1);\
}\
#proxypool-global-menu .menu-links { display: flex; gap: 10px; flex-wrap: wrap; }\
#proxypool-global-menu .menu-links a {\
    padding: 8px 16px;\
    background: #fff;\
    color: #333;\
    border: 1px solid #ddd;\
    border-radius: 4px;\
    text-decoration: none;\
    transition: all 0.2s;\
    font-weight: 500;\
}\
#proxypool-global-menu .menu-links a:hover { background: #007bff; color: #fff; border-color: #007bff; }\
#proxypool-global-menu .menu-links a.active { background: #007bff; color: #fff; border-color: #007bff; }\
#proxypool-global-menu .menu-stats { display: flex; gap: 15px; font-size: 13px; white-space: nowrap; }\
#proxypool-global-menu .menu-stats span { color: #666; }\
#proxypool-global-menu .menu-stats strong { font-weight: 600; }\
#proxypool-global-menu .menu-stats .stat-connected { color: #28a745; }\
#proxypool-global-menu .menu-stats .stat-disconnected { color: #dc3545; }\
</style>\
<script>\
function updateProxyPoolStats() {\
    fetch("/cgi-bin/luci/admin/services/proxypool?action=status")\
        .then(r => r.json())\
        .then(data => {\
            if (data && data.summary) {\
                document.getElementById("pp-stat-total").textContent = data.summary.total;\
                document.getElementById("pp-stat-connected").textContent = data.summary.connected;\
                document.getElementById("pp-stat-disconnected").textContent = data.summary.disconnected;\
            }\
        })\
        .catch(() => {});\
}\
if (window.location.pathname.includes("/admin/services/proxypool")) {\
    setTimeout(updateProxyPoolStats, 500);\
    setInterval(updateProxyPoolStats, 10000);\
}\
</script>
' "$HEADER_FILE"

# 查找 <body> 标签后的位置插入菜单
sed -i '/<body[^>]*>/a\
<!-- proxypool-global-menu -->\
<div id="proxypool-global-menu">\
    <div class="menu-links">\
        <a href="/cgi-bin/luci/admin/services/proxypool">智联盒子</a>\
        <a href="/cgi-bin/luci/admin/services/proxypool?tab=log">系统日志</a>\
        <a href="/cgi-bin/luci/admin/network/wireless">信道分析</a>\
        <a href="/cgi-bin/luci/admin/system/flashops">备份与升级</a>\
        <a href="/cgi-bin/luci/admin/network/wireless">无线</a>\
        <a href="/cgi-bin/luci/admin/system/reboot">重启</a>\
    </div>\
    <div class="menu-stats">\
        <span>总 <strong id="pp-stat-total">-</strong></span>\
        <span class="stat-connected">已连接 <strong id="pp-stat-connected">-</strong></span>\
        <span class="stat-disconnected">未连接 <strong id="pp-stat-disconnected">-</strong></span>\
    </div>\
</div>
' "$HEADER_FILE"

echo "全局菜单安装完成！"
echo "✓ 已隐藏 LuCI 原有菜单（状态|系统|服务|网络|注销）"
echo "✓ 已安装自定义快捷菜单和统计显示"
echo ""
echo "如需恢复原始 header.htm，执行："
echo "  cp $BACKUP_FILE $HEADER_FILE"
echo "  /etc/init.d/uhttpd restart"
