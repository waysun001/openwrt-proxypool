#!/bin/sh
# ProxyPool 全局菜单安装脚本
# 修改 LuCI header.htm，在顶部菜单添加快捷导航和统计

HEADER_FILE="/usr/lib/lua/luci/view/header.htm"
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

# 插入样式和脚本
sed -i '/<\/head>/i\
<!-- ProxyPool Global Menu -->\
<style>\
#proxypool-global-menu {\
    display: flex;\
    justify-content: space-between;\
    align-items: center;\
    background: #f8f9fa;\
    padding: 8px 20px;\
    border-bottom: 1px solid #ddd;\
    font-size: 14px;\
}\
#proxypool-global-menu .menu-links { display: flex; gap: 10px; flex-wrap: wrap; }\
#proxypool-global-menu .menu-links a {\
    padding: 6px 14px;\
    background: #fff;\
    color: #333;\
    border: 1px solid #ddd;\
    border-radius: 4px;\
    text-decoration: none;\
    transition: all 0.2s;\
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
echo "如需恢复原始 header.htm，执行："
echo "  cp $BACKUP_FILE $HEADER_FILE"
echo "  /etc/init.d/uhttpd restart"
