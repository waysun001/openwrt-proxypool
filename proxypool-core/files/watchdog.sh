#!/bin/sh
# 智联盒子 - Watchdog 健康检查脚本
# 由 cron 每5分钟调用一次，检测并自动恢复异常客户端
# 兼容 busybox ash 环境

LOG_FILE="/var/log/proxypool.log"
RUN_DIR="/var/run/proxypool"
WD_DIR="$RUN_DIR/watchdog"
COOLDOWN=300  # 每个客户端最少间隔5分钟才能再次重启

log_info() {
    echo "[$(date '+%Y-%m-%d %H:%M:%S')] [watchdog] $*" >> "$LOG_FILE"
}

log_warn() {
    echo "[$(date '+%Y-%m-%d %H:%M:%S')] [watchdog] [WARN] $*" >> "$LOG_FILE"
}

get_config() {
    uci -q get "proxypool.$1.$2" || echo "$3"
}

get_clients() {
    uci show proxypool 2>/dev/null | grep "=client" | cut -d'.' -f2 | cut -d'=' -f1
}

# 检查频率限制：距上次重启是否已超过 COOLDOWN 秒
check_cooldown() {
    local client="$1"
    local ts_file="$WD_DIR/${client}.ts"
    local now=$(date +%s)

    if [ -f "$ts_file" ]; then
        local last=$(cat "$ts_file" 2>/dev/null || echo 0)
        local diff=$((now - last))
        if [ "$diff" -lt "$COOLDOWN" ]; then
            return 1  # 冷却中，不允许重启
        fi
    fi
    return 0  # 允许重启
}

# 记录重启时间戳
record_restart() {
    local client="$1"
    mkdir -p "$WD_DIR"
    date +%s > "$WD_DIR/${client}.ts"
}

# 执行客户端重启
do_restart() {
    local client="$1"
    local reason="$2"

    if ! check_cooldown "$client"; then
        log_info "Skip restart $client (cooldown): $reason"
        return 0
    fi

    log_warn "Restarting $client: $reason"
    record_restart "$client"
    /usr/lib/proxypool/proxypool.sh restart_client "$client" 2>/dev/null
}

# 检查 L2TP 客户端健康状态
check_l2tp() {
    local client="$1"
    local ppp_iface="ppp-${client}"

    # 情况1：PPP 接口存在
    if ip link show "$ppp_iface" >/dev/null 2>&1; then
        local ip=$(ip -4 addr show "$ppp_iface" 2>/dev/null | grep -o 'inet [0-9.]*' | cut -d' ' -f2)
        if [ -n "$ip" ]; then
            # 有 IP，ping 网关验证隧道是否假死
            local gw=$(ip route show dev "$ppp_iface" 2>/dev/null | awk '/via/{print $3}' | head -1)
            # 无网关时尝试 ping 对端 IP（PPP 点对点链路）
            if [ -z "$gw" ]; then
                gw=$(ip -4 addr show "$ppp_iface" 2>/dev/null | grep -o 'peer [0-9.]*' | cut -d' ' -f2)
            fi
            if [ -n "$gw" ]; then
                if ! ping -c 3 -W 3 "$gw" >/dev/null 2>&1; then
                    do_restart "$client" "L2TP tunnel dead (ping $gw failed)"
                    return
                fi
            fi
            # ping 成功或无网关可 ping → 视为正常
            return
        fi
        # 有接口但无 IP → 可能正在协商，跳过
        return
    fi

    # 情况2：无 PPP 接口
    local pid_file="$RUN_DIR/l2tp/${client}/xl2tpd.pid"
    if [ -f "$pid_file" ]; then
        local pid=$(cat "$pid_file" 2>/dev/null)
        if [ -n "$pid" ] && kill -0 "$pid" 2>/dev/null; then
            # xl2tpd 进程在运行但无 PPP 接口 → 正在连接中，跳过
            return
        fi
    fi

    # 无 PPP 接口 + 无 xl2tpd 进程 → max_redials 耗尽或进程崩溃
    do_restart "$client" "L2TP no ppp interface and no xl2tpd process"
}

# 检查 SOCKS5 客户端健康状态
check_socks5() {
    local client="$1"
    local pid_file="$RUN_DIR/redsocks/${client}.pid"

    if [ ! -f "$pid_file" ]; then
        do_restart "$client" "SOCKS5 pid file missing"
        return
    fi

    local pid=$(cat "$pid_file" 2>/dev/null)
    if [ -z "$pid" ] || ! kill -0 "$pid" 2>/dev/null; then
        do_restart "$client" "SOCKS5 process not running (pid=$pid)"
        return
    fi
}

# 主逻辑
run() {
    mkdir -p "$WD_DIR"

    local clients=$(get_clients)
    for client in $clients; do
        local enabled=$(get_config "$client" "enabled" "0")
        [ "$enabled" != "1" ] && continue

        local type=$(get_config "$client" "type" "")
        case "$type" in
            l2tp)
                check_l2tp "$client"
                ;;
            socks5)
                check_socks5 "$client"
                ;;
        esac
    done
}

case "$1" in
    run)
        run
        ;;
    *)
        echo "Usage: $0 run"
        exit 1
        ;;
esac
