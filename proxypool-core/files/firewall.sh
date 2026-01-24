#!/bin/bash
# ProxyPool 防火墙管理脚本
# 实现严格的网络隔离：未绑定或客户端离线时无法上网

set -e

RUN_DIR="/var/run/proxypool"
NFT_RULES="/var/run/proxypool/firewall.nft"
LOG_FILE="/var/log/proxypool.log"

# 路由表基础编号
RT_TABLE_BASE=100

log_info() {
    echo "[$(date '+%Y-%m-%d %H:%M:%S')] [firewall] $*" >> "$LOG_FILE"
}

log_error() {
    echo "[$(date '+%Y-%m-%d %H:%M:%S')] [firewall] [ERROR] $*" >> "$LOG_FILE"
    echo "[ERROR] $*" >&2
}

# 读取UCI配置
get_config() {
    local section="$1"
    local option="$2"
    local default="$3"
    uci -q get "proxypool.$section.$option" || echo "$default"
}

# 获取所有客户端
get_clients() {
    uci show proxypool 2>/dev/null | grep "=client" | cut -d'.' -f2 | cut -d'=' -f1
}

# 获取客户端编号
get_client_num() {
    local client="$1"
    echo "$client" | sed 's/client_//' | sed 's/^0*//'
}

# 获取路由表编号
get_rt_table() {
    local client="$1"
    local num=$(get_client_num "$client")
    [ -z "$num" ] && num=0
    echo $((RT_TABLE_BASE + num))
}

# 初始化防火墙
init() {
    log_info "Initializing firewall..."

    # 创建 nftables 表
    nft add table inet proxypool 2>/dev/null || true

    # 创建链
    nft add chain inet proxypool prerouting { type filter hook prerouting priority -150 \; } 2>/dev/null || true
    nft add chain inet proxypool output { type filter hook output priority -150 \; } 2>/dev/null || true
    nft add chain inet proxypool forward { type filter hook forward priority -1 \; } 2>/dev/null || true
    nft add chain inet proxypool nat_prerouting { type nat hook prerouting priority -100 \; } 2>/dev/null || true

    # 创建 ipset 用于存储允许的IP
    nft add set inet proxypool allowed_ips { type ipv4_addr \; } 2>/dev/null || true

    # 默认规则：拒绝所有未绑定的内网流量
    # 保留本地流量
    nft add rule inet proxypool forward ip saddr 192.168.0.0/16 ip daddr 192.168.0.0/16 accept 2>/dev/null || true
    nft add rule inet proxypool forward ip saddr 10.0.0.0/8 ip daddr 10.0.0.0/8 accept 2>/dev/null || true

    # 允许已建立的连接
    nft add rule inet proxypool forward ct state established,related accept 2>/dev/null || true

    log_info "Firewall initialized"
}

# 清理防火墙
cleanup() {
    log_info "Cleaning up firewall..."

    # 删除表
    nft delete table inet proxypool 2>/dev/null || true

    # 清理路由表
    for i in $(seq 0 59); do
        local table=$((RT_TABLE_BASE + i))
        ip rule del table "$table" 2>/dev/null || true
        ip route flush table "$table" 2>/dev/null || true
    done

    log_info "Firewall cleaned up"
}

# 更新单个客户端的规则
update_client() {
    local client="$1"
    local type=$(get_config "$client" "type" "")
    local bind_ips=$(uci -q get "proxypool.$client.bind_ip" 2>/dev/null || true)

    log_info "Updating firewall for client: $client"

    local rt_table=$(get_rt_table "$client")
    local mark=$((rt_table))

    # 清理旧规则
    remove_client "$client"

    # 检查客户端是否在线
    local status=""
    case "$type" in
        l2tp)
            status=$(/usr/lib/proxypool/l2tp-manager.sh status "$client" | head -1)
            ;;
        socks5)
            status=$(/usr/lib/proxypool/socks5-manager.sh status "$client" | head -1)
            ;;
    esac

    if [ "$status" != "connected" ]; then
        log_info "Client $client is not connected, blocking bound IPs"
        # 客户端未连接，阻止绑定的IP上网
        for ip in $bind_ips; do
            nft add rule inet proxypool forward ip saddr "$ip" drop 2>/dev/null || true
        done
        return 0
    fi

    # 客户端已连接，配置路由和防火墙

    case "$type" in
        l2tp)
            setup_l2tp_routing "$client" "$bind_ips" "$rt_table" "$mark"
            ;;
        socks5)
            setup_socks5_routing "$client" "$bind_ips" "$rt_table" "$mark"
            ;;
    esac

    log_info "Firewall updated for client: $client"
}

# 配置 L2TP 路由
setup_l2tp_routing() {
    local client="$1"
    local bind_ips="$2"
    local rt_table="$3"
    local mark="$4"

    local ppp_iface="ppp-${client}"

    # 检查接口是否存在
    if ! ip link show "$ppp_iface" &>/dev/null; then
        log_error "PPP interface $ppp_iface not found"
        return 1
    fi

    # 获取PPP接口的网关
    local gateway=$(ip route show dev "$ppp_iface" | grep -oP '(?<=via\s)\d+(\.\d+){3}' | head -1)
    if [ -z "$gateway" ]; then
        # 点对点链路，使用对端地址
        gateway=$(ip -4 addr show "$ppp_iface" | grep -oP '(?<=peer\s)\d+(\.\d+){3}' | head -1)
    fi

    # 配置路由表
    ip route add default dev "$ppp_iface" table "$rt_table" 2>/dev/null || true

    # 为每个绑定IP配置策略路由
    for ip in $bind_ips; do
        # 添加到允许列表
        nft add element inet proxypool allowed_ips { "$ip" } 2>/dev/null || true

        # fwmark 规则
        ip rule add from "$ip" table "$rt_table" priority "$rt_table" 2>/dev/null || true

        # 允许转发
        nft add rule inet proxypool forward ip saddr "$ip" oifname "$ppp_iface" accept 2>/dev/null || true

        log_info "Bound IP $ip to L2TP client $client via $ppp_iface"
    done

    # SNAT/MASQUERADE
    nft add rule inet proxypool nat_prerouting oifname "$ppp_iface" masquerade 2>/dev/null || true
}

# 配置 SOCKS5 路由 (透明代理)
setup_socks5_routing() {
    local client="$1"
    local bind_ips="$2"
    local rt_table="$3"
    local mark="$4"

    local tcp_port=$(/usr/lib/proxypool/socks5-manager.sh port "$client" tcp)
    local udp_port=$(/usr/lib/proxypool/socks5-manager.sh port "$client" udp)

    if [ -z "$tcp_port" ]; then
        log_error "Cannot get local port for SOCKS5 client $client"
        return 1
    fi

    # 为每个绑定IP配置透明代理
    for ip in $bind_ips; do
        # 添加到允许列表
        nft add element inet proxypool allowed_ips { "$ip" } 2>/dev/null || true

        # TCP 透明代理 (REDIRECT 到 redsocks)
        nft add rule inet proxypool nat_prerouting ip saddr "$ip" tcp dport != 22 redirect to :"$tcp_port" 2>/dev/null || true

        # UDP 透明代理 (TPROXY)
        nft add rule inet proxypool prerouting ip saddr "$ip" udp dport != 53 tproxy to :"$udp_port" mark set "$mark" 2>/dev/null || true

        # 允许转发
        nft add rule inet proxypool forward ip saddr "$ip" accept 2>/dev/null || true

        log_info "Bound IP $ip to SOCKS5 client $client (TCP:$tcp_port, UDP:$udp_port)"
    done

    # 配置 TPROXY 路由
    ip rule add fwmark "$mark" table "$rt_table" priority "$rt_table" 2>/dev/null || true
    ip route add local 0.0.0.0/0 dev lo table "$rt_table" 2>/dev/null || true
}

# 移除客户端规则
remove_client() {
    local client="$1"
    local rt_table=$(get_rt_table "$client")
    local bind_ips=$(uci -q get "proxypool.$client.bind_ip" 2>/dev/null || true)

    log_info "Removing firewall rules for client: $client"

    # 清理路由规则
    for ip in $bind_ips; do
        ip rule del from "$ip" table "$rt_table" 2>/dev/null || true
        nft delete element inet proxypool allowed_ips { "$ip" } 2>/dev/null || true
    done

    ip rule del fwmark "$rt_table" table "$rt_table" 2>/dev/null || true
    ip route flush table "$rt_table" 2>/dev/null || true

    # 注意：nftables 规则清理依赖 rebuild
}

# 重建所有规则
rebuild() {
    log_info "Rebuilding all firewall rules..."

    # 清理并重新初始化
    cleanup
    init

    # 重建每个客户端的规则
    local clients=$(get_clients)
    for client in $clients; do
        local enabled=$(get_config "$client" "enabled" "0")
        if [ "$enabled" = "1" ]; then
            update_client "$client"
        fi
    done

    # 添加默认拒绝规则（放在最后）
    # 阻止所有未明确允许的内网IP访问外网
    nft add rule inet proxypool forward ip saddr 192.168.0.0/16 ip daddr != 192.168.0.0/16 drop 2>/dev/null || true
    nft add rule inet proxypool forward ip saddr 10.0.0.0/8 ip daddr != 10.0.0.0/8 drop 2>/dev/null || true

    log_info "Firewall rules rebuilt"
}

# 显示当前规则
show() {
    echo "=== nftables rules ==="
    nft list table inet proxypool 2>/dev/null || echo "Table not found"

    echo ""
    echo "=== IP rules ==="
    ip rule show | grep -E "^($RT_TABLE_BASE|$((RT_TABLE_BASE+1))|$((RT_TABLE_BASE+2)))" || true

    echo ""
    echo "=== Allowed IPs ==="
    nft list set inet proxypool allowed_ips 2>/dev/null || echo "Set not found"
}

# 主入口
case "$1" in
    init)
        init
        ;;
    cleanup)
        cleanup
        ;;
    update_client)
        update_client "$2"
        ;;
    remove_client)
        remove_client "$2"
        ;;
    rebuild)
        rebuild
        ;;
    show)
        show
        ;;
    *)
        echo "Usage: $0 {init|cleanup|update_client|remove_client|rebuild|show} [client_id]"
        exit 1
        ;;
esac
