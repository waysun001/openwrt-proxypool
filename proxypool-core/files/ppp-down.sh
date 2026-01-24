#!/bin/sh
# PPP 连接断开后的回调脚本
# 当 L2TP 连接断开后更新防火墙规则（阻止绑定的IP上网）

INTERFACE="$1"
TTY_DEVICE="$2"
SPEED="$3"
LOCAL_IP="$4"
REMOTE_IP="$5"
IPPARAM="$6"

LOG_FILE="/var/log/proxypool.log"

log_info() {
    echo "[$(date '+%Y-%m-%d %H:%M:%S')] [ppp-down] $*" >> "$LOG_FILE"
}

# 检查是否是 proxypool 的接口
if echo "$INTERFACE" | grep -q "^ppp-client_"; then
    client=$(echo "$INTERFACE" | sed 's/^ppp-//')

    log_info "PPP interface down: $INTERFACE (client: $client)"

    # 更新防火墙规则（会阻止绑定的IP上网）
    /usr/lib/proxypool/firewall.sh update_client "$client" &
fi

exit 0
