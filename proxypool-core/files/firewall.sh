#!/bin/bash
# 智联盒子 - 防火墙管理脚本
# 使用 nftables 实现严格的网络隔离

RUN_DIR="/var/run/proxypool"
LOG_FILE="/var/log/proxypool.log"

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
    if ! ip link show "$ppp_iface" &>/dev/null; then
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
            local ppp_iface="ppp-${client}"
            if ip link show "$ppp_iface" &>/dev/null; then
                return 0
            fi
            ;;
    esac
    return 1
}

# 收集所有需要允许的 IP 和对应的端口
collect_allowed_ips() {
    local clients=$(get_clients)
    local allowed_ips=""
    local nat_rules=""

    for client in $clients; do
        local enabled=$(get_config "$client" "enabled" "0")
        if [ "$enabled" != "1" ]; then
            continue
        fi

        if ! is_client_online "$client"; then
            continue
        fi

        local type=$(get_config "$client" "type" "")
        local bind_ips=$(uci -q get "proxypool.$client.bind_ip" 2>/dev/null || true)

        if [ -z "$bind_ips" ]; then
            continue
        fi

        for ip in $bind_ips; do
            case "$type" in
                socks5)
                    local tcp_port=$(get_client_port "$client")
                    if [ -n "$tcp_port" ]; then
                        allowed_ips="$allowed_ips $ip"
                        nat_rules="$nat_rules|$ip:$tcp_port"
                    fi
                    ;;
                l2tp)
                    local ppp_iface="ppp-${client}"
                    allowed_ips="$allowed_ips $ip"
                    # L2TP 使用策略路由
                    ;;
            esac
        done
    done

    echo "$allowed_ips|$nat_rules"
}

# 初始化防火墙表
init() {
    log_info "Initializing firewall..."

    # 删除旧表
    nft delete table inet proxypool 2>/dev/null || true
    nft delete table ip proxypool_nat 2>/dev/null || true

    # 创建新表
    nft add table inet proxypool
    nft add table ip proxypool_nat

    # 创建 forward 链 - 优先级 -1，在 fw4 之前处理
    nft add chain inet proxypool forward { type filter hook forward priority -1 \; }

    # 创建 NAT prerouting 链（SOCKS5 重定向）
    nft add chain ip proxypool_nat prerouting { type nat hook prerouting priority -100 \; }

    # 创建 NAT postrouting 链（L2TP masquerade）
    nft add chain ip proxypool_nat postrouting { type nat hook postrouting priority 100 \; }
    nft add rule ip proxypool_nat postrouting oifname "ppp-*" masquerade

    # 基础规则：允许内网互访
    nft add rule inet proxypool forward ip saddr 192.168.0.0/16 ip daddr 192.168.0.0/16 accept
    nft add rule inet proxypool forward ip saddr 10.0.0.0/8 ip daddr 10.0.0.0/8 accept
    nft add rule inet proxypool forward ip saddr 172.16.0.0/12 ip daddr 172.16.0.0/12 accept

    # 允许已建立的连接
    nft add rule inet proxypool forward ct state established,related accept

    # 默认阻止所有内网到外网的新连接
    nft add rule inet proxypool forward ip saddr 192.168.0.0/16 ip daddr != { 192.168.0.0/16, 10.0.0.0/8, 172.16.0.0/12 } drop
    nft add rule inet proxypool forward ip saddr 10.0.0.0/8 ip daddr != { 192.168.0.0/16, 10.0.0.0/8, 172.16.0.0/12 } drop

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

# 重建所有规则
rebuild() {
    log_info "Rebuilding firewall rules..."

    # 删除旧表
    nft delete table inet proxypool 2>/dev/null || true
    nft delete table ip proxypool_nat 2>/dev/null || true

    # 创建新表
    nft add table inet proxypool
    nft add table ip proxypool_nat

    # 创建链
    nft add chain inet proxypool forward { type filter hook forward priority -1 \; }
    nft add chain ip proxypool_nat prerouting { type nat hook prerouting priority -100 \; }
    nft add chain ip proxypool_nat postrouting { type nat hook postrouting priority 100 \; }

    # L2TP PPP 接口流量 masquerade（将 LAN IP 伪装为 PPP 分配的 IP）
    nft add rule ip proxypool_nat postrouting oifname "ppp-*" masquerade

    # 清理旧的策略路由
    cleanup_policy_routing

    # 收集所有允许的 IP 和 NAT 规则
    local data=$(collect_allowed_ips)
    local allowed_ips=$(echo "$data" | cut -d'|' -f1)
    local nat_data=$(echo "$data" | cut -d'|' -f2-)

    # 基础规则
    nft add rule inet proxypool forward ip saddr 192.168.0.0/16 ip daddr 192.168.0.0/16 accept
    nft add rule inet proxypool forward ip saddr 10.0.0.0/8 ip daddr 10.0.0.0/8 accept
    nft add rule inet proxypool forward ip saddr 172.16.0.0/12 ip daddr 172.16.0.0/12 accept
    nft add rule inet proxypool forward ct state established,related accept

    # 添加允许的 IP
    for ip in $allowed_ips; do
        log_info "Allowing IP: $ip"
        nft add rule inet proxypool forward ip saddr $ip accept
    done

    # 添加 NAT 规则
    echo "$nat_data" | tr '|' '\n' | while read rule; do
        [ -z "$rule" ] && continue
        local ip=$(echo "$rule" | cut -d: -f1)
        local port=$(echo "$rule" | cut -d: -f2)
        if [ -n "$ip" ] && [ -n "$port" ]; then
            log_info "Adding NAT rule: $ip -> port $port"
            nft add rule ip proxypool_nat prerouting ip saddr $ip tcp dport != 22 redirect to :$port
        fi
    done

    # 默认阻止规则
    nft add rule inet proxypool forward ip saddr 192.168.0.0/16 ip daddr != { 192.168.0.0/16, 10.0.0.0/8, 172.16.0.0/12 } drop
    nft add rule inet proxypool forward ip saddr 10.0.0.0/8 ip daddr != { 192.168.0.0/16, 10.0.0.0/8, 172.16.0.0/12 } drop

    # 设置 L2TP 策略路由（将绑定 IP 的流量路由到对应 PPP 接口）
    local all_clients=$(get_clients)
    for client in $all_clients; do
        local c_enabled=$(get_config "$client" "enabled" "0")
        local c_type=$(get_config "$client" "type" "")
        if [ "$c_enabled" = "1" ] && [ "$c_type" = "l2tp" ] && is_client_online "$client"; then
            setup_l2tp_routing "$client"
        fi
    done

    # 清除 conntrack 缓存（确保旧连接使用新路由）
    conntrack -F 2>/dev/null || true

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
