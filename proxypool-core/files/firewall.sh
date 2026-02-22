#!/bin/sh
# 智联盒子 - 防火墙管理脚本
# 使用 nftables 实现严格的网络隔离
# 所有规则通过 nft -f 原子化加载，杜绝重建时的安全空窗期

RUN_DIR="/var/run/proxypool"
LOG_FILE="/var/log/proxypool.log"
LOCK_DIR="/var/lock/proxypool-fw.lock"
# 锁超时（秒）：超过此时间的残留锁视为 stale 并强制清除
LOCK_TIMEOUT=15

log_info() {
    echo "[$(date '+%Y-%m-%d %H:%M:%S')] [firewall] $*" >> "$LOG_FILE"
}

log_error() {
    echo "[$(date '+%Y-%m-%d %H:%M:%S')] [firewall] [ERROR] $*" >> "$LOG_FILE"
}

get_config() {
    uci -q get "proxypool.$1.$2" || echo "$3"
}

get_clients() {
    uci show proxypool 2>/dev/null | grep "=client" | cut -d'.' -f2 | cut -d'=' -f1
}

get_client_port() {
    local client="$1"
    local ports_file="$RUN_DIR/redsocks/${client}.ports"
    if [ -f "$ports_file" ]; then
        cat "$ports_file" | cut -d: -f1
    else
        echo ""
    fi
}

# 从客户端 ID 中提取编号，用于路由表 ID
get_client_num() {
    local num=$(echo "$1" | sed 's/[^0-9]//g')
    [ -z "$num" ] && num=0
    echo "$num"
}

# 清理所有策略路由规则（路由表 100-199）
cleanup_policy_routing() {
    local i
    for i in $(seq 100 199); do
        while ip rule del table "$i" 2>/dev/null; do :; done
        ip route flush table "$i" 2>/dev/null || true
    done
}

# 为 L2TP 客户端设置策略路由
setup_l2tp_routing() {
    local client="$1"
    local num=$(get_client_num "$client")
    local table_id=$((100 + num))
    local ppp_iface="ppp-${client}"

    # 检查 PPP 接口是否存在
    if ! ip link show "$ppp_iface" >/dev/null 2>&1; then
        return 1
    fi

    # 添加默认路由到路由表
    ip route add default dev "$ppp_iface" table "$table_id" 2>/dev/null || true

    # 为每个绑定 IP 添加策略路由规则
    local bind_ips=$(uci -q get "proxypool.$client.bind_ip" 2>/dev/null || true)
    for ip in $bind_ips; do
        ip rule add from "$ip" table "$table_id" priority "$table_id" 2>/dev/null || true
        log_info "Policy route: $ip -> $ppp_iface (table $table_id)"
    done
}

is_client_online() {
    local client="$1"
    local type=$(get_config "$client" "type" "")

    case "$type" in
        socks5)
            local pid_file="$RUN_DIR/redsocks/${client}.pid"
            if [ -f "$pid_file" ]; then
                local pid=$(cat "$pid_file")
                if [ -n "$pid" ] && kill -0 "$pid" 2>/dev/null; then
                    return 0
                fi
            fi
            ;;
        l2tp)
            # 仅检查接口存在不够：PPP 握手完成前接口已存在但无 IP，此时流量无法通过隧道
            local ppp_iface="ppp-${client}"
            if ip link show "$ppp_iface" >/dev/null 2>&1; then
                local ppp_ip=$(ip -4 addr show "$ppp_iface" 2>/dev/null | grep -o 'inet [0-9.]*' | cut -d' ' -f2)
                if [ -n "$ppp_ip" ]; then
                    return 0
                fi
            fi
            ;;
        slp)
            # SLP: 需同时检查 slp-client 进程和 redsocks 进程
            local slp_pid_file="/var/run/proxypool/slp/${client}/slp.pid"
            if [ -f "$slp_pid_file" ]; then
                local slp_pid=$(cat "$slp_pid_file")
                if [ -n "$slp_pid" ] && kill -0 "$slp_pid" 2>/dev/null; then
                    local rs_pid_file="$RUN_DIR/redsocks/${client}.pid"
                    if [ -f "$rs_pid_file" ]; then
                        local rs_pid=$(cat "$rs_pid_file")
                        if [ -n "$rs_pid" ] && kill -0 "$rs_pid" 2>/dev/null; then
                            return 0
                        fi
                    fi
                fi
            fi
            ;;
    esac
    return 1
}

COUNTER_DIR="$RUN_DIR/counters"

# 保存当前 nftables 计数器到持久化文件
# 每个 IP 一个文件，格式：out_bytes in_bytes（累加值）
_save_counters() {
    mkdir -p "$COUNTER_DIR"
    local nft_output=$(nft list chain inet proxypool forward 2>/dev/null) || return 0
    [ -z "$nft_output" ] && return 0

    # 解析每条带 comment 的规则，提取 IP 和 bytes
    echo "$nft_output" | grep 'comment "out_' | while read -r line; do
        local ip=$(echo "$line" | grep -o 'comment "out_[^"]*"' | sed 's/comment "out_//;s/"//')
        local bytes=$(echo "$line" | grep -o 'bytes [0-9]*' | awk '{print $2}')
        [ -z "$ip" ] || [ -z "$bytes" ] && continue
        local old=$(cat "$COUNTER_DIR/${ip}.out" 2>/dev/null || echo 0)
        echo $((old + bytes)) > "$COUNTER_DIR/${ip}.out"
    done

    echo "$nft_output" | grep 'comment "in_' | while read -r line; do
        local ip=$(echo "$line" | grep -o 'comment "in_[^"]*"' | sed 's/comment "in_//;s/"//')
        local bytes=$(echo "$line" | grep -o 'bytes [0-9]*' | awk '{print $2}')
        [ -z "$ip" ] || [ -z "$bytes" ] && continue
        local old=$(cat "$COUNTER_DIR/${ip}.in" 2>/dev/null || echo 0)
        echo $((old + bytes)) > "$COUNTER_DIR/${ip}.in"
    done
}

# 构建 nftables 规则文件（原子化加载）
build_nft_ruleset() {
    local nft_file="$1"
    local clients=$(get_clients)

    # 收集允许的 IP 和 NAT 规则
    local allowed_ips=""
    local socks5_rules=""

    for client in $clients; do
        local enabled=$(get_config "$client" "enabled" "0")
        [ "$enabled" != "1" ] && continue
        is_client_online "$client" || continue

        local type=$(get_config "$client" "type" "")
        local bind_ips=$(uci -q get "proxypool.$client.bind_ip" 2>/dev/null || true)
        [ -z "$bind_ips" ] && continue

        for ip in $bind_ips; do
            case "$type" in
                socks5|slp)
                    local tcp_port=$(get_client_port "$client")
                    if [ -n "$tcp_port" ]; then
                        allowed_ips="$allowed_ips $ip"
                        # 排除内网目标，只重定向外网流量（保留对路由器和内网设备的直接访问）
                        socks5_rules="$socks5_rules
        ip saddr $ip ip daddr != { 192.168.0.0/16, 10.0.0.0/8, 172.16.0.0/12 } tcp dport != 22 redirect to :$tcp_port"
                    fi
                    ;;
                l2tp)
                    allowed_ips="$allowed_ips $ip"
                    ;;
            esac
        done
    done

    # 构建允许 IP 的 nft 规则行（带 counter 用于流量统计）
    local allow_lines=""
    for ip in $allowed_ips; do
        allow_lines="$allow_lines
        ip saddr $ip counter accept comment \"out_$ip\"
        ip daddr $ip counter accept comment \"in_$ip\""
        log_info "Allowing IP: $ip"
    done

    # 写入原子化 nft 规则文件
    cat > "$nft_file" << NFTEOF
table inet proxypool;
delete table inet proxypool;
table ip proxypool_nat;
delete table ip proxypool_nat;

table inet proxypool {
    chain forward {
        type filter hook forward priority -1; policy accept;

        # 允许内网互访
        ip saddr 192.168.0.0/16 ip daddr 192.168.0.0/16 accept
        ip saddr 10.0.0.0/8 ip daddr 10.0.0.0/8 accept
        ip saddr 172.16.0.0/12 ip daddr 172.16.0.0/12 accept

        # 允许绑定了在线客户端的 IP（不使用 ct state，确保断开后立即阻断）
${allow_lines}

        # 阻止所有其他内网到外网的流量（包括已建立的连接）
        ip saddr 192.168.0.0/16 ip daddr != { 192.168.0.0/16, 10.0.0.0/8, 172.16.0.0/12 } drop
        ip saddr 10.0.0.0/8 ip daddr != { 192.168.0.0/16, 10.0.0.0/8, 172.16.0.0/12 } drop
    }
}

table ip proxypool_nat {
    chain prerouting {
        type nat hook prerouting priority -100; policy accept;
${socks5_rules}
    }
    chain postrouting {
        type nat hook postrouting priority 100; policy accept;
        oifname "ppp-*" masquerade
    }
}
NFTEOF
}

# 初始化防火墙表（启动时调用，阻止所有内网外出流量）
init() {
    log_info "Initializing firewall..."

    local nft_file=$(mktemp /tmp/proxypool-nft.XXXXXX) || { log_error "Failed to create temp file"; return 1; }

    cat > "$nft_file" << 'NFTEOF'
table inet proxypool;
delete table inet proxypool;
table ip proxypool_nat;
delete table ip proxypool_nat;

table inet proxypool {
    chain forward {
        type filter hook forward priority -1; policy accept;
        ip saddr 192.168.0.0/16 ip daddr 192.168.0.0/16 accept
        ip saddr 10.0.0.0/8 ip daddr 10.0.0.0/8 accept
        ip saddr 172.16.0.0/12 ip daddr 172.16.0.0/12 accept
        ip saddr 192.168.0.0/16 ip daddr != { 192.168.0.0/16, 10.0.0.0/8, 172.16.0.0/12 } drop
        ip saddr 10.0.0.0/8 ip daddr != { 192.168.0.0/16, 10.0.0.0/8, 172.16.0.0/12 } drop
    }
}

table ip proxypool_nat {
    chain prerouting {
        type nat hook prerouting priority -100; policy accept;
    }
    chain postrouting {
        type nat hook postrouting priority 100; policy accept;
        oifname "ppp-*" masquerade
    }
}
NFTEOF

    nft -f "$nft_file"
    rm -f "$nft_file"

    log_info "Firewall initialized - all LAN devices blocked by default"
}

# 清理防火墙
cleanup() {
    log_info "Cleaning up firewall..."
    nft delete table inet proxypool 2>/dev/null || true
    nft delete table ip proxypool_nat 2>/dev/null || true
    cleanup_policy_routing
    log_info "Firewall cleaned up"
}

# 重建所有规则（原子化，无安全空窗期）
# 使用 mkdir 原子锁防止并发 rebuild 竞态（POSIX 兼容，busybox ash 可用）
rebuild() {
    local waited=0

    # 检测并清除 stale 锁（进程崩溃后残留）
    # 使用 lock 目录内的时间戳文件（兼容 busybox，不依赖 date -r / stat）
    if [ -d "$LOCK_DIR" ]; then
        local lock_ts=$(cat "$LOCK_DIR/ts" 2>/dev/null || echo 0)
        local now_ts=$(date +%s)
        local lock_age=$((now_ts - lock_ts))
        if [ "$lock_age" -gt "$LOCK_TIMEOUT" ]; then
            log_info "Removing stale firewall lock (age: ${lock_age}s)"
            rm -rf "$LOCK_DIR"
        fi
    fi

    # 自旋等待获取锁
    while ! mkdir "$LOCK_DIR" 2>/dev/null; do
        waited=$((waited + 1))
        if [ $waited -ge $LOCK_TIMEOUT ]; then
            log_error "Failed to acquire firewall lock after ${LOCK_TIMEOUT}s, forcing"
            rm -rf "$LOCK_DIR"
            mkdir "$LOCK_DIR" 2>/dev/null || true
            break
        fi
        sleep 1
    done

    # 写入时间戳用于 stale 检测
    echo "$(date +%s)" > "$LOCK_DIR/ts" 2>/dev/null

    _rebuild_locked

    rm -rf "$LOCK_DIR"
}

_rebuild_locked() {
    log_info "Rebuilding firewall rules..."

    # 保存当前 nftables 计数器到持久化文件（防止 delete table 后归零）
    _save_counters

    # 清理旧的策略路由
    cleanup_policy_routing

    # 构建并原子化加载 nft 规则
    local nft_file=$(mktemp /tmp/proxypool-nft.XXXXXX) || { log_error "Failed to create temp file"; return 1; }
    build_nft_ruleset "$nft_file"

    if ! nft -f "$nft_file" 2>> "$LOG_FILE"; then
        log_error "Failed to apply nft rules, check $nft_file"
    else
        rm -f "$nft_file"
    fi

    # 设置 L2TP 策略路由（将绑定 IP 的流量路由到对应 PPP 接口）
    local all_clients=$(get_clients)
    for client in $all_clients; do
        local c_enabled=$(get_config "$client" "enabled" "0")
        local c_type=$(get_config "$client" "type" "")
        if [ "$c_enabled" = "1" ] && [ "$c_type" = "l2tp" ] && is_client_online "$client"; then
            setup_l2tp_routing "$client"
        fi
    done

    # 注意：不再执行 conntrack -F 全量清除
    # nftables forward 链未使用 ct state，规则移除即立即阻断，无需清 conntrack
    # 全量清除会导致其他在线客户端的 NAT 会话中断

    log_info "Firewall rules rebuilt"
}

# 更新单个客户端的规则
update_client() {
    local client="$1"
    log_info "Updating rules for client: $client"
    rebuild
}

# 显示当前规则
show() {
    echo "=== 智联盒子 nftables 规则 ==="
    nft list table inet proxypool 2>/dev/null || echo "Table inet proxypool not found"
    echo ""
    nft list table ip proxypool_nat 2>/dev/null || echo "Table ip proxypool_nat not found"
}

case "$1" in
    init)
        init
        ;;
    cleanup)
        cleanup
        ;;
    rebuild)
        rebuild
        ;;
    update_client)
        update_client "$2"
        ;;
    show)
        show
        ;;
    *)
        echo "Usage: $0 {init|cleanup|rebuild|update_client|show} [client_id]"
        exit 1
        ;;
esac
