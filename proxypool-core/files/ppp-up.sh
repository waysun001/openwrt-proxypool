#!/bin/sh
# PPP 连接建立后的回调脚本
# 当 L2TP 连接建立后更新防火墙规则

INTERFACE="$1"
TTY_DEVICE="$2"
SPEED="$3"
LOCAL_IP="$4"
REMOTE_IP="$5"
IPPARAM="$6"

LOG_FILE="/var/log/proxypool.log"

log_info() {
    echo "[$(date '+%Y-%m-%d %H:%M:%S')] [ppp-up] $*" >> "$LOG_FILE"
}

# 检查是否是 proxypool 的接口
if echo "$INTERFACE" | grep -q "^ppp-client_"; then
    client=$(echo "$INTERFACE" | sed 's/^ppp-//')

    log_info "PPP interface up: $INTERFACE (client: $client, IP: $LOCAL_IP)"

    # 更新防火墙规则
    /usr/lib/proxypool/firewall.sh update_client "$client" &
fi

exit 0
