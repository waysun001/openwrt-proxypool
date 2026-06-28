#!/bin/sh
# 智联盒子 - PPP 连接建立回调脚本

INTERFACE="$1"
TTY_DEVICE="$2"
SPEED="$3"
LOCAL_IP="$4"
REMOTE_IP="$5"
IPPARAM="$6"

RUN_DIR="/var/run/proxypool"
LOG_FILE="/var/log/proxypool.log"

# 防火墙 rebuild 合并：多条 L2TP 隧道几乎同时拨通时，会各自触发一次完整 rebuild。
# rebuild 很重且会先 cleanup_policy_routing 清空所有策略路由再重建，
# 并发执行时一个 rebuild 会清掉另一个刚加好的路由，导致部分线路连不上
# （表现为批量导入后需手动"停止再连接"才正常）。
# 这里把一段时间内的多次拨通合并成一次 rebuild：第一个回调成为执行者，
# 后续回调只置标记；执行者等待一个沉降窗口直到没有新请求，再跑一次 rebuild。
REQ_FILE="$RUN_DIR/fw-rebuild.req"
RUNNER_DIR="$RUN_DIR/fw-rebuild.runner"
SETTLE=3

log_info() {
    echo "[$(date '+%Y-%m-%d %H:%M:%S')] [ppp-up] $*" >> "$LOG_FILE"
}

# 检查是否是智联盒子的接口
if echo "$INTERFACE" | grep -q "^ppp-"; then
    client=$(echo "$INTERFACE" | sed 's/^ppp-//')

    log_info "PPP interface up: $INTERFACE (client: $client, IP: $LOCAL_IP)"

    mkdir -p "$RUN_DIR"
    # 置位 rebuild 请求标记（每个拨通回调都会置位）
    touch "$REQ_FILE"

    # 抢占执行者：mkdir 原子操作，只有第一个回调成为执行者
    if mkdir "$RUNNER_DIR" 2>/dev/null; then
        (
            # 外层循环：rebuild 跑完后若期间又有新隧道拨通，再补一次
            while [ -f "$REQ_FILE" ]; do
                # 沉降窗口：只要还有新请求就继续等，把整批拨通合并成一次
                while [ -f "$REQ_FILE" ]; do
                    rm -f "$REQ_FILE"
                    sleep "$SETTLE"
                done
                log_info "Coalesced firewall rebuild after L2TP tunnels settled"
                /usr/lib/proxypool/firewall.sh rebuild
            done
            rmdir "$RUNNER_DIR" 2>/dev/null
        ) &
    fi
fi

exit 0
