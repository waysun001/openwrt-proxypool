#!/bin/sh
# ProxyPool 全局菜单卸载脚本
# 恢复 LuCI header.htm 到原始状态

HEADER_FILE="/usr/lib/lua/luci/view/cbi/header.htm"
BACKUP_FILE="${HEADER_FILE}.proxypool-backup"

# 检查备份是否存在
if [ ! -f "$BACKUP_FILE" ]; then
    echo "错误：找不到备份文件 $BACKUP_FILE"
    echo "可能未安装过全局菜单，或备份文件已被删除"
    exit 1
fi

echo "恢复原始 header.htm..."
cp "$BACKUP_FILE" "$HEADER_FILE"

echo "重启 uhttpd 服务..."
/etc/init.d/uhttpd restart

echo "全局菜单已卸载！"
echo "备份文件保留在: $BACKUP_FILE"
