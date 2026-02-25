#!/bin/sh
# 智联盒子 - 状态监控脚本

RUN_DIR="/var/run/proxypool"
LOG_FILE="/var/log/proxypool.log"

get_config() {
    uci -q get "proxypool.$1.$2" || echo "$3"
}

get_clients() {
    uci show proxypool 2>/dev/null | grep "=client" | cut -d'.' -f2 | cut -d'=' -f1
}

format_bytes() {
    local bytes="$1"
    if [ "$bytes" -ge 1073741824 ] 2>/dev/null; then
        awk "BEGIN {printf \"%.2f GB\", $bytes/1073741824}"
    elif [ "$bytes" -ge 1048576 ] 2>/dev/null; then
        awk "BEGIN {printf \"%.2f MB\", $bytes/1048576}"
    elif [ "$bytes" -ge 1024 ] 2>/dev/null; then
        awk "BEGIN {printf \"%.2f KB\", $bytes/1024}"
    else
        echo "${bytes} B"
    fi
}

get_client_status() {
    local client="$1"
    local nft_out_cache="$2"
    local nft_in_cache="$3"
    local type=$(get_config "$client" "type" "")
    local name=$(get_config "$client" "name" "$client")
    local server=$(get_config "$client" "server" "")
    local port=$(get_config "$client" "port" "")
    local username=$(get_config "$client" "username" "")
    local password=$(get_config "$client" "password" "")
    local expiry=$(get_config "$client" "expiry" "")
    local remark=$(get_config "$client" "remark" "")
    
    # IP归属地查询（使用内置脚本，开箱即用，带缓存）
    local location=""
    if [ -n "$server" ]; then
        local cache_file="$RUN_DIR/location_cache/${server}.txt"
        if [ -f "$cache_file" ]; then
            # 使用缓存（5分钟有效期）
            local cache_age=$(($(date +%s) - $(stat -c %Y "$cache_file" 2>/dev/null || echo 0)))
            if [ $cache_age -lt 300 ]; then
                location=$(cat "$cache_file" 2>/dev/null)
            fi
        fi
        
        # 缓存未命中或过期，重新查询
        if [ -z "$location" ]; then
            location=$(/usr/lib/proxypool/iplocation.sh "$server" 2>/dev/null)
            if [ -n "$location" ]; then
                mkdir -p "$RUN_DIR/location_cache"
                echo "$location" > "$cache_file"
            fi
        fi
    fi
    local enabled=$(get_config "$client" "enabled" "0")
    local bind_ips=$(uci -q get "proxypool.$client.bind_ip" 2>/dev/null | tr ' ' ',')
    local status="offline"
    local rx=0
    local tx=0
    local ip_addr=""

    if [ "$enabled" = "1" ]; then
        case "$type" in
            l2tp)
                local result=$(/usr/lib/proxypool/l2tp-manager.sh status "$client" 2>/dev/null || echo "disconnected")
                status=$(echo "$result" | head -1)
                if [ "$status" = "connected" ]; then
                    ip_addr=$(echo "$result" | sed -n '2p')
                fi
                ;;
            socks5)
                local result=$(/usr/lib/proxypool/socks5-manager.sh status "$client" 2>/dev/null || echo "disconnected")
                status=$(echo "$result" | head -1)
                ;;
            slp)
                local result=$(/usr/lib/proxypool/slp-manager.sh status "$client" 2>/dev/null || echo "disconnected")
                status=$(echo "$result" | head -1)
                ;;
            *)
                status="disconnected"
                ;;
        esac
    else
        status="disabled"
    fi

    # 读取流量统计：持久化累加值 + 当前 nftables counter（适用于所有客户端类型）
    # 从专用计数链 count_out / count_in 读取（捕获 TCP/UDP/ICMP 全协议）
    local counter_dir="$RUN_DIR/counters"
    local bind_ip_list=$(echo "$bind_ips" | tr ',' ' ')
    for bip in $bind_ip_list; do
        # 持久化累加值（rebuild 前保存的历史流量）
        local saved_out=$(cat "$counter_dir/${bip}.out" 2>/dev/null || echo 0)
        local saved_in=$(cat "$counter_dir/${bip}.in" 2>/dev/null || echo 0)
        # 当前 nftables counter（本次 rebuild 后的增量，从专用计数链读取）
        local cur_out=$(echo "$nft_out_cache" | grep "comment \"out_$bip\"" | grep -o 'bytes [0-9]*' | awk '{print $2}')
        local cur_in=$(echo "$nft_in_cache" | grep "comment \"in_$bip\"" | grep -o 'bytes [0-9]*' | awk '{print $2}')
        tx=$((tx + saved_out + ${cur_out:-0}))
        rx=$((rx + saved_in + ${cur_in:-0}))
    done

    local bind_ips_json="[]"
    if [ -n "$bind_ips" ]; then
        bind_ips_json="[\"$(echo "$bind_ips" | sed 's/,/","/g')\"]"
    fi

    # 超时计数
    local timeout_dir="$RUN_DIR/timeout"
    local timeout_today=$(cat "$timeout_dir/${client}.today" 2>/dev/null || echo 0)
    local timeout_yesterday=$(cat "$timeout_dir/${client}.yesterday" 2>/dev/null || echo 0)

    cat << EOF
{
  "id": "$client",
  "name": "$name",
  "type": "$type",
  "server": "$server",
  "port": "$port",
  "username": "$username",
  "password": "$password",
  "expiry": "$expiry",
  "remark": "$remark",
  "location": "$location",
  "enabled": $enabled,
  "status": "$status",
  "ip_addr": "$ip_addr",
  "bind_ips": $bind_ips_json,
  "rx_bytes": $rx,
  "tx_bytes": $tx,
  "rx_human": "$(format_bytes $rx)",
  "tx_human": "$(format_bytes $tx)",
  "timeout_today": $timeout_today,
  "timeout_yesterday": $timeout_yesterday
}
EOF
}

get_bound_devices() {
    local clients=$(get_clients)
    local devices="["
    local first=1

    for client in $clients; do
        local bind_ips=$(uci -q get "proxypool.$client.bind_ip" 2>/dev/null || true)
        local client_name=$(get_config "$client" "name" "$client")

        for ip in $bind_ips; do
            local neigh_entry=$(ip neigh show "$ip" 2>/dev/null | head -1)
            local mac=$(echo "$neigh_entry" | awk '{print $5}')
            local state=$(echo "$neigh_entry" | awk '{print $NF}')
            local online="false"
            # 只有 REACHABLE 和 DELAY 状态才算在线
            # STALE=缓存过期 FAILED=不可达 INCOMPLETE=未完成 都不算在线
            if [ -n "$mac" ] && [ "$mac" != "FAILED" ]; then
                case "$state" in
                    REACHABLE|DELAY|PROBE) online="true" ;;
                esac
            fi

            if [ $first -eq 0 ]; then
                devices="$devices,"
            fi
            first=0
            devices="$devices{\"ip\":\"$ip\",\"mac\":\"$mac\",\"online\":$online,\"client\":\"$client\",\"client_name\":\"$client_name\"}"
        done
    done

    devices="$devices]"
    echo "$devices"
}

get_full_status() {
    mkdir -p "$RUN_DIR"

    local clients=$(get_clients)
    local global_enabled=$(get_config "global" "enabled" "1")
    local total=0
    local connected=0
    local enabled_count=0
    local clients_json="["
    local first=1

    # 循环前查询一次 nft，缓存结果避免 N 次系统调用
    # 从专用计数链读取（count_out/count_in 捕获全协议流量）
    local NFT_OUT_CACHE=$(nft list chain inet proxypool count_out 2>/dev/null)
    local NFT_IN_CACHE=$(nft list chain inet proxypool count_in 2>/dev/null)

    for client in $clients; do
        local status_json=$(get_client_status "$client" "$NFT_OUT_CACHE" "$NFT_IN_CACHE")

        if [ $first -eq 0 ]; then
            clients_json="$clients_json,"
        fi
        first=0
        clients_json="$clients_json$status_json"

        total=$((total + 1))

        local enabled=$(get_config "$client" "enabled" "0")
        if [ "$enabled" = "1" ]; then
            enabled_count=$((enabled_count + 1))
        fi

        local client_status=$(echo "$status_json" | grep '"status"' | cut -d'"' -f4)
        if [ "$client_status" = "connected" ]; then
            connected=$((connected + 1))
        fi
    done

    clients_json="$clients_json]"
    local devices=$(get_bound_devices)
    local disconnected=$((enabled_count - connected))

    cat << EOF
{
  "timestamp": $(date +%s),
  "datetime": "$(date '+%Y-%m-%d %H:%M:%S')",
  "global_enabled": $global_enabled,
  "summary": {
    "total": $total,
    "enabled": $enabled_count,
    "connected": $connected,
    "disconnected": $disconnected
  },
  "clients": $clients_json,
  "devices": $devices
}
EOF
}

case "$1" in
    get)
        get_full_status
        ;;
    client)
        get_client_status "$2" "$(nft list chain inet proxypool count_out 2>/dev/null)" "$(nft list chain inet proxypool count_in 2>/dev/null)"
        ;;
    devices)
        get_bound_devices
        ;;
    *)
        echo "Usage: $0 {get|client|devices} [client_id]"
        exit 1
        ;;
esac
