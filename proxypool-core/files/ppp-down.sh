#!/bin/sh
# 智联盒子 - PPP 连接断开回调脚本

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

# 检查是否是智联盒子的接口
if echo "$INTERFACE" | grep -q "^ppp-"; then
    client=$(echo "$INTERFACE" | sed 's/^ppp-//')

    log_info "PPP interface down: $INTERFACE (client: $client)"

    # 同步重建防火墙规则（立即阻止绑定的IP上网，防止泄露到本地网络）
    /usr/lib/proxypool/firewall.sh rebuild
fi

exit 0
