#!/bin/sh
# 智联盒子 - PPP 连接建立回调脚本

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

# 检查是否是智联盒子的接口
if echo "$INTERFACE" | grep -q "^ppp-"; then
    client=$(echo "$INTERFACE" | sed 's/^ppp-//')

    log_info "PPP interface up: $INTERFACE (client: $client, IP: $LOCAL_IP)"

    # 同步更新防火墙规则（PPP 回调本身已在后台执行，无需再异步；
    # 异步调用会导致竞态：防火墙规则未就绪时流量已开始转发）
    /usr/lib/proxypool/firewall.sh rebuild
fi

exit 0
