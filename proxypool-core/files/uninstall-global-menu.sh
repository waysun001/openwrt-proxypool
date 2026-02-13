#!/bin/sh
# ProxyPool 全局菜单卸载脚本
# 恢复 LuCI header.htm 到原始状态

# 查找正确的 header.htm（与安装脚本保持一致）
HEADER_FILE=""
for f in \
    /usr/lib/lua/luci/view/themes/bootstrap/header.htm \
    /usr/lib/lua/luci/view/header.htm \
    /usr/lib/lua/luci/view/cbi/header.htm; do
    [ -f "$f" ] && HEADER_FILE="$f" && break
done
BACKUP_FILE="${HEADER_FILE}.proxypool-backup"

if [ -z "$HEADER_FILE" ] || [ ! -f "$HEADER_FILE" ]; then
    echo "错误：找不到 LuCI header.htm 文件"
    exit 1
fi

# 检查是否安装了菜单
if ! grep -q "proxypool-global-menu" "$HEADER_FILE"; then
    echo "全局菜单未安装，无需卸载"
    exit 0
fi

# 优先从备份恢复
if [ -f "$BACKUP_FILE" ]; then
    echo "从备份恢复 header.htm..."
    cp "$BACKUP_FILE" "$HEADER_FILE"
else
    echo "警告：找不到备份文件，尝试 sed 清除..."
    sed -i '/<!-- ProxyPool Global Menu -->/,/<\/style>/d' "$HEADER_FILE"
    sed -i '/<!-- proxypool-global-menu -->/,/<!-- \/proxypool-global-menu -->/d' "$HEADER_FILE"
    sed -i '/updateProxyPoolStats/,/<\/script>/d' "$HEADER_FILE"
fi

echo "重启 uhttpd 服务..."
/etc/init.d/uhttpd restart 2>/dev/null

echo "全局菜单已卸载！"
echo "备份文件保留在: $BACKUP_FILE"
