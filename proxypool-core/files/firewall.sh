#!/bin/sh
# 智联盒子 - 防火墙管理脚本
# 使用 nftables 实现严格的网络隔离
# 所有规则通过 nft -f 原子化加载，杜绝重建时的安全空窗期
#
# 架构说明：
# - count_out/count_in 链：prerouting/postrouting 位置，捕获所有协议流量计数
# - forward 链：L2TP 放行全协议，SOCKS5/SLP 无 accept 规则（仅 TCP 经 NAT redirect 走代理）
# - proxypool_nat 表：SOCKS5/SLP TCP redirect 到 redsocks

RUN_DIR="/var/run/proxypool"
LOG_FILE="/var/log/proxypool.log"
LOCK_DIR="/var/lock/proxypool-fw.lock"
# 锁超时（秒）：超过此时间的残留锁视为 stale 并强制清除
LOCK_TIMEOUT=15
LEGACY_GATE="${PROXYPOOL_LEGACY_GATE:-/usr/lib/proxypool/legacy-gate.sh}"

legacy_quarantine() {
    /bin/sh "$LEGACY_GATE" mutation "$1" >/dev/null 2>&1 || true
    printf '%s\n' 'legacy_runtime_quarantined'
    return 125
}

# 获取防火墙锁（mkdir 原子锁，POSIX 兼容）
_acquire_lock() {
    local waited=0

    # 检测并清除 stale 锁（进程崩溃后残留）
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

    echo "$(date +%s)" > "$LOCK_DIR/ts" 2>/dev/null
}

# 释放防火墙锁
_release_lock() {
    rm -rf "$LOCK_DIR"
}

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

    # 正在停止的客户端视为离线（防止断开瞬间 IP 泄漏）
    [ -f "$RUN_DIR/stopping/$client" ] && return 1

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
# 从专用计数链 count_out / count_in 读取（捕获所有协议流量）
# 每个 IP 一个文件，格式：累加字节数
_save_counters() {
    mkdir -p "$COUNTER_DIR"

    # 从专用计数链读取（不再从 forward 链读取）
    local nft_out
    nft_out=$(nft list chain inet proxypool count_out 2>/dev/null) || true
    local nft_in
    nft_in=$(nft list chain inet proxypool count_in 2>/dev/null) || true
    [ -z "$nft_out" ] && [ -z "$nft_in" ] && return 0

    echo "$nft_out" | grep 'comment "out_' | while read -r line; do
        local ip=$(echo "$line" | grep -o 'comment "out_[^"]*"' | sed 's/comment "out_//;s/"//')
        local bytes=$(echo "$line" | grep -o 'bytes [0-9]*' | awk '{print $2}')
        [ -z "$ip" ] || [ -z "$bytes" ] && continue
        local old=$(cat "$COUNTER_DIR/${ip}.out" 2>/dev/null || echo 0)
        echo $((old + bytes)) > "$COUNTER_DIR/${ip}.out"
    done

    echo "$nft_in" | grep 'comment "in_' | while read -r line; do
        local ip=$(echo "$line" | grep -o 'comment "in_[^"]*"' | sed 's/comment "in_//;s/"//')
        local bytes=$(echo "$line" | grep -o 'bytes [0-9]*' | awk '{print $2}')
        [ -z "$ip" ] || [ -z "$bytes" ] && continue
        local old=$(cat "$COUNTER_DIR/${ip}.in" 2>/dev/null || echo 0)
        echo $((old + bytes)) > "$COUNTER_DIR/${ip}.in"
    done
}

# 构建 nftables 规则文件（原子化加载）
#
# 新架构：
# - count_out (prerouting -200)：所有绑定 IP 出站流量计数（TCP/UDP/ICMP 全捕获）
# - count_in (postrouting 200)：所有绑定 IP 入站流量计数（conntrack 已反转 NAT）
# - forward：L2TP IP 放行全协议；SOCKS5/SLP IP 无 accept → UDP/ICMP 被末尾 drop 规则阻断
# - proxypool_nat：SOCKS5/SLP TCP redirect 到 redsocks
build_nft_ruleset() {
    local nft_file="$1"
    local clients=$(get_clients)

    # 分类收集规则
    local count_out_lines=""
    local count_in_lines=""
    local l2tp_allow_lines=""
    local socks5_rules=""

    for client in $clients; do
        local enabled=$(get_config "$client" "enabled" "0")
        [ "$enabled" != "1" ] && continue
        is_client_online "$client" || continue

        local type=$(get_config "$client" "type" "")
        local bind_ips=$(uci -q get "proxypool.$client.bind_ip" 2>/dev/null || true)
        [ -z "$bind_ips" ] && continue

        for ip in $bind_ips; do
            # 所有在线绑定 IP 都生成计数规则（全协议）
            count_out_lines="$count_out_lines
        ip saddr $ip ip daddr != { 192.168.0.0/16, 10.0.0.0/8, 172.16.0.0/12 } counter comment \"out_$ip\""
            count_in_lines="$count_in_lines
        ip daddr $ip ip saddr != { 192.168.0.0/16, 10.0.0.0/8, 172.16.0.0/12 } counter comment \"in_$ip\""

            case "$type" in
                socks5|slp)
                    local tcp_port=$(get_client_port "$client")
                    if [ -n "$tcp_port" ]; then
                        # NAT redirect：仅 TCP，排除 SSH
                        socks5_rules="$socks5_rules
        ip saddr $ip ip daddr != { 192.168.0.0/16, 10.0.0.0/8, 172.16.0.0/12 } tcp dport != 22 redirect to :$tcp_port"
                    fi
                    # SOCKS5/SLP 不在 forward 链中添加 accept 规则
                    # → TCP 被 NAT redirect 到 redsocks，不经过 forward
                    # → UDP/ICMP 等非 TCP 流量落到末尾 drop 规则 → 被阻断
                    # → 浏览器 HTTP/3 (QUIC/UDP:443) 被 drop → 自动降级 TCP → 走代理
                    # → DNS (UDP:53) 到路由器 LAN IP 是内网流量 → 被内网互访规则放行
                    log_info "SOCKS5/SLP IP: $ip (TCP only, UDP/ICMP blocked)"
                    ;;
                l2tp)
                    # L2TP：策略路由处理全协议，forward 链放行
                    l2tp_allow_lines="$l2tp_allow_lines
        ip saddr $ip accept
        ip daddr $ip accept"
                    log_info "L2TP IP: $ip (all protocols via policy routing)"
                    ;;
            esac
        done
    done

    # 写入原子化 nft 规则文件
    cat > "$nft_file" << NFTEOF
table inet proxypool;
delete table inet proxypool;
table ip proxypool_nat;
delete table ip proxypool_nat;

table inet proxypool {
    # 流量计数（prerouting，优先级 -200，在 NAT 之前）
    # 捕获所有协议（TCP/UDP/ICMP）的出站流量
    chain count_out {
        type filter hook prerouting priority -200; policy accept;
${count_out_lines}
    }

    # 流量计数（postrouting，优先级 200，在 NAT 之后）
    # conntrack 已反转 NAT，正确捕获所有回包
    chain count_in {
        type filter hook postrouting priority 200; policy accept;
${count_in_lines}
    }

    # 访问控制
    chain forward {
        type filter hook forward priority -1; policy accept;

        # 允许内网互访（包括 DNS 到路由器 LAN IP）
        ip saddr 192.168.0.0/16 ip daddr 192.168.0.0/16 accept
        ip saddr 10.0.0.0/8 ip daddr 10.0.0.0/8 accept
        ip saddr 172.16.0.0/12 ip daddr 172.16.0.0/12 accept

        # L2TP 绑定 IP：放行全协议（策略路由走 PPP 接口）
${l2tp_allow_lines}

        # SOCKS5/SLP 绑定 IP：此处无 accept 规则
        # TCP 已在 NAT prerouting 被 REDIRECT → 不经过 forward
        # UDP/ICMP/其他协议 → 落到下面的 drop 规则 → 阻断泄漏

        # 阻止所有未授权的 LAN→WAN 流量（包括已建立的连接）
        ip saddr 192.168.0.0/16 ip daddr != { 192.168.0.0/16, 10.0.0.0/8, 172.16.0.0/12 } drop
        ip saddr 10.0.0.0/8 ip daddr != { 192.168.0.0/16, 10.0.0.0/8, 172.16.0.0/12 } drop

        # 阻止所有 IPv6 外出流量（防止 IPv6 绕过代理规则）
        # 允许 IPv6 链路本地 (fe80::/10) 和组播 (ff00::/8) 互访
        ip6 saddr fe80::/10 accept
        ip6 daddr ff00::/8 accept
        ip6 saddr != ::1 ip6 daddr != { ::1, fe80::/10, ff00::/8 } drop
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
    chain count_out {
        type filter hook prerouting priority -200; policy accept;
    }
    chain count_in {
        type filter hook postrouting priority 200; policy accept;
    }
    chain forward {
        type filter hook forward priority -1; policy accept;
        ip saddr 192.168.0.0/16 ip daddr 192.168.0.0/16 accept
        ip saddr 10.0.0.0/8 ip daddr 10.0.0.0/8 accept
        ip saddr 172.16.0.0/12 ip daddr 172.16.0.0/12 accept
        ip saddr 192.168.0.0/16 ip daddr != { 192.168.0.0/16, 10.0.0.0/8, 172.16.0.0/12 } drop
        ip saddr 10.0.0.0/8 ip daddr != { 192.168.0.0/16, 10.0.0.0/8, 172.16.0.0/12 } drop
        ip6 saddr fe80::/10 accept
        ip6 daddr ff00::/8 accept
        ip6 saddr != ::1 ip6 daddr != { ::1, fe80::/10, ff00::/8 } drop
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

    log_info "Firewall initialized - all LAN devices blocked by default (IPv4+IPv6)"
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
rebuild() {
    _acquire_lock
    _rebuild_locked
    _release_lock
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
        log_error "Failed to apply nft rules (file kept: $nft_file), falling back to block-all"
        # nft -f 非原子：前几行可能已删除旧表，若新规则创建失败则无任何规则
        # 回退到 init 的 block-all 规则，确保绝不出现无防火墙的空窗期
        init
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

# ============================================================
# 增量操作：仅修改单个客户端的规则（替代全量 rebuild）
# remove_client: ~4 次 nft 调用 + grep/awk（毫秒级）
# add_client:    ~2 次 nft 调用（毫秒级）
# 对比 rebuild:  ~500 次 fork（50 客户端时 5-10 秒）
# ============================================================

# 增量移除单个客户端的所有防火墙规则
_remove_client_locked() {
    local client="$1"
    local bind_ips=$(uci -q get "proxypool.$client.bind_ip" 2>/dev/null || true)
    [ -z "$bind_ips" ] && return 0

    log_info "Incremental remove: $client (IPs: $bind_ips)"

    # 读取各链规则（带 handle），4 次 nft 调用
    local chain_cout=$(nft -a list chain inet proxypool count_out 2>/dev/null) || true
    local chain_cin=$(nft -a list chain inet proxypool count_in 2>/dev/null) || true
    local chain_fwd=$(nft -a list chain inet proxypool forward 2>/dev/null) || true
    local chain_nat=$(nft -a list chain ip proxypool_nat prerouting 2>/dev/null) || true

    mkdir -p "$COUNTER_DIR"
    local nft_file=$(mktemp /tmp/proxypool-nft-rm.XXXXXX) || return 1
    > "$nft_file"

    for ip in $bind_ips; do
        # 保存计数器（删除前持久化，防止流量归零）
        local bytes_out=$(echo "$chain_cout" | grep "\"out_${ip}\"" | grep -o 'bytes [0-9]*' | awk '{print $2}')
        if [ -n "$bytes_out" ] && [ "$bytes_out" -gt 0 ] 2>/dev/null; then
            local old=$(cat "$COUNTER_DIR/${ip}.out" 2>/dev/null || echo 0)
            echo $((old + bytes_out)) > "$COUNTER_DIR/${ip}.out"
        fi
        local bytes_in=$(echo "$chain_cin" | grep "\"in_${ip}\"" | grep -o 'bytes [0-9]*' | awk '{print $2}')
        if [ -n "$bytes_in" ] && [ "$bytes_in" -gt 0 ] 2>/dev/null; then
            local old=$(cat "$COUNTER_DIR/${ip}.in" 2>/dev/null || echo 0)
            echo $((old + bytes_in)) > "$COUNTER_DIR/${ip}.in"
        fi

        # 删除 count_out 规则（按 comment 匹配，精确到 IP）
        echo "$chain_cout" | grep "\"out_${ip}\"" | grep -o '# handle [0-9]*' | awk '{print $3}' | while read -r h; do
            echo "delete rule inet proxypool count_out handle $h" >> "$nft_file"
        done

        # 删除 count_in 规则
        echo "$chain_cin" | grep "\"in_${ip}\"" | grep -o '# handle [0-9]*' | awk '{print $3}' | while read -r h; do
            echo "delete rule inet proxypool count_in handle $h" >> "$nft_file"
        done

        # 删除 forward 链 L2TP accept 规则（按 IP 精确匹配，IP 后跟空格防止前缀误匹配）
        echo "$chain_fwd" | grep -E "ip (saddr|daddr) ${ip} accept" | grep -o '# handle [0-9]*' | awk '{print $3}' | while read -r h; do
            echo "delete rule inet proxypool forward handle $h" >> "$nft_file"
        done

        # 删除 NAT prerouting redirect 规则
        echo "$chain_nat" | grep "ip saddr ${ip} " | grep -o '# handle [0-9]*' | awk '{print $3}' | while read -r h; do
            echo "delete rule ip proxypool_nat prerouting handle $h" >> "$nft_file"
        done
    done

    # 批量执行删除
    if [ -s "$nft_file" ]; then
        if nft -f "$nft_file" 2>> "$LOG_FILE"; then
            log_info "Removed $(wc -l < "$nft_file") rules for $client"
        else
            log_error "Incremental remove failed for $client, falling back to full rebuild"
            rm -f "$nft_file"
            _rebuild_locked
            return
        fi
    else
        log_info "No rules found for $client, nothing to remove"
    fi
    rm -f "$nft_file"

    # 清理 L2TP 策略路由
    local type=$(get_config "$client" "type" "")
    if [ "$type" = "l2tp" ]; then
        local num=$(get_client_num "$client")
        local table_id=$((100 + num))
        while ip rule del table "$table_id" 2>/dev/null; do :; done
        ip route flush table "$table_id" 2>/dev/null || true
    fi
}

# 增量添加单个客户端的防火墙规则
_add_client_locked() {
    local client="$1"
    local enabled=$(get_config "$client" "enabled" "0")
    [ "$enabled" != "1" ] && { log_info "Client $client not enabled, skip add"; return 0; }
    is_client_online "$client" || { log_info "Client $client not online, skip add"; return 0; }

    local type=$(get_config "$client" "type" "")
    local bind_ips=$(uci -q get "proxypool.$client.bind_ip" 2>/dev/null || true)
    [ -z "$bind_ips" ] && return 0

    log_info "Incremental add: $client ($type, IPs: $bind_ips)"

    # 检查 nft 表是否存在（服务未启动时表不存在）
    if ! nft list table inet proxypool >/dev/null 2>&1; then
        log_error "Table inet proxypool not found, falling back to full rebuild"
        _rebuild_locked
        return
    fi

    local nft_file=$(mktemp /tmp/proxypool-nft-add.XXXXXX) || return 1
    > "$nft_file"

    for ip in $bind_ips; do
        # 流量计数规则（全协议）
        echo "add rule inet proxypool count_out ip saddr $ip ip daddr != { 192.168.0.0/16, 10.0.0.0/8, 172.16.0.0/12 } counter comment \"out_$ip\"" >> "$nft_file"
        echo "add rule inet proxypool count_in ip daddr $ip ip saddr != { 192.168.0.0/16, 10.0.0.0/8, 172.16.0.0/12 } counter comment \"in_$ip\"" >> "$nft_file"

        case "$type" in
            socks5|slp)
                local tcp_port=$(get_client_port "$client")
                if [ -n "$tcp_port" ]; then
                    echo "add rule ip proxypool_nat prerouting ip saddr $ip ip daddr != { 192.168.0.0/16, 10.0.0.0/8, 172.16.0.0/12 } tcp dport != 22 redirect to :$tcp_port" >> "$nft_file"
                fi
                ;;
        esac
    done

    # L2TP: forward 链 accept 规则必须插入在 drop 规则之前
    if [ "$type" = "l2tp" ]; then
        local drop_handle=$(nft -a list chain inet proxypool forward 2>/dev/null | grep 'drop' | head -1 | grep -o '# handle [0-9]*' | awk '{print $3}')
        for ip in $bind_ips; do
            if [ -n "$drop_handle" ]; then
                echo "insert rule inet proxypool forward position $drop_handle ip saddr $ip accept" >> "$nft_file"
                echo "insert rule inet proxypool forward position $drop_handle ip daddr $ip accept" >> "$nft_file"
            else
                echo "add rule inet proxypool forward ip saddr $ip accept" >> "$nft_file"
                echo "add rule inet proxypool forward ip daddr $ip accept" >> "$nft_file"
            fi
        done
    fi

    # 批量执行添加
    if [ -s "$nft_file" ]; then
        if nft -f "$nft_file" 2>> "$LOG_FILE"; then
            log_info "Added $(wc -l < "$nft_file") rules for $client"
        else
            log_error "Incremental add failed for $client, falling back to full rebuild"
            rm -f "$nft_file"
            _rebuild_locked
            return
        fi
    fi
    rm -f "$nft_file"

    # L2TP 策略路由
    if [ "$type" = "l2tp" ]; then
        setup_l2tp_routing "$client"
    fi
}

# 增量移除（公开接口，带锁）
remove_client() {
    local client="$1"
    log_info "Remove client rules: $client"
    _acquire_lock
    _remove_client_locked "$client"
    _release_lock
}

# 增量添加（公开接口，带锁）
add_client() {
    local client="$1"
    log_info "Add client rules: $client"
    _acquire_lock
    _add_client_locked "$client"
    _release_lock
}

# 更新单个客户端的规则（移除旧规则 + 添加新规则）
update_client() {
    local client="$1"
    log_info "Updating rules for client: $client"
    _acquire_lock
    _remove_client_locked "$client"
    _add_client_locked "$client"
    _release_lock
}

# 显示当前规则
show() {
    echo "=== 智联盒子 nftables 规则 ==="
    nft list table inet proxypool 2>/dev/null || echo "Table inet proxypool not found"
    echo ""
    nft list table ip proxypool_nat 2>/dev/null || echo "Table ip proxypool_nat not found"
}

case "$1" in
    show)
        ;;
    init|cleanup|rebuild|remove_client|add_client|update_client)
        legacy_quarantine "firewall:$1"
        exit $?
        ;;
    *)
        echo "Usage: $0 {init|cleanup|rebuild|remove_client|add_client|update_client|show} [client_id]"
        exit 1
        ;;
esac

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
    remove_client)
        remove_client "$2"
        ;;
    add_client)
        add_client "$2"
        ;;
    update_client)
        update_client "$2"
        ;;
    show)
        show
        ;;
    *) exit 1 ;;
esac
