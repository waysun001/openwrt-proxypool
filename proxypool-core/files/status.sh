#!/bin/sh
# 智联盒子 - 状态监控脚本

RUN_DIR="/var/run/proxypool"
LOG_FILE="/var/log/proxypool.log"

# JSON 字符串转义：处理双引号、反斜杠、换行符、制表符等特殊字符
json_escape() {
    printf '%s' "$1" | sed -e 's/\\/\\\\/g' -e 's/"/\\"/g' -e 's/	/\\t/g' | tr -d '\n\r'
}

get_config() {
    local val
    val=$(uci -q get "proxypool.$1.$2")
    if [ -z "$val" ]; then
        echo "$3"
    else
        echo "$val"
    fi
}

get_clients() {
    uci show proxypool 2>/dev/null | grep "=client" | cut -d'.' -f2 | cut -d'=' -f1
}

format_bytes() {
    local bytes="${1:-0}"
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

    # 确保数值变量非空（空值会导致 JSON 断裂）
    enabled="${enabled:-0}"
    rx="${rx:-0}"
    tx="${tx:-0}"
    timeout_today="${timeout_today:-0}"
    timeout_yesterday="${timeout_yesterday:-0}"

    # 转义所有字符串字段，防止特殊字符破坏 JSON
    local j_name=$(json_escape "$name")
    local j_server=$(json_escape "$server")
    local j_port=$(json_escape "$port")
    local j_username=$(json_escape "$username")
    local j_password=$(json_escape "$password")
    local j_expiry=$(json_escape "$expiry")
    local j_remark=$(json_escape "$remark")
    local j_location=$(json_escape "$location")
    local j_ip_addr=$(json_escape "$ip_addr")

    cat << EOF
{
  "id": "$client",
  "name": "$j_name",
  "type": "$type",
  "server": "$j_server",
  "port": "$j_port",
  "username": "$j_username",
  "password": "$j_password",
  "expiry": "$j_expiry",
  "remark": "$j_remark",
  "location": "$j_location",
  "enabled": $enabled,
  "status": "$status",
  "ip_addr": "$j_ip_addr",
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

    # 单次 ip neigh show 缓存全表（替代每个 IP 单独查询，212 次 → 1 次）
    local NEIGH_CACHE=$(ip neigh show 2>/dev/null)

    for client in $clients; do
        local bind_ips=$(uci -q get "proxypool.$client.bind_ip" 2>/dev/null || true)
        local client_name=$(get_config "$client" "name" "$client")

        for ip in $bind_ips; do
            local neigh_entry=$(echo "$NEIGH_CACHE" | grep "^$ip " | head -1)
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
            local j_client_name=$(json_escape "$client_name")
            devices="$devices{\"ip\":\"$ip\",\"mac\":\"$mac\",\"online\":$online,\"client\":\"$client\",\"client_name\":\"$j_client_name\"}"
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
    [ "$disconnected" -lt 0 ] 2>/dev/null && disconnected=0
    global_enabled="${global_enabled:-1}"

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
        # 捕获输出，确保即使脚本内部出错也返回有效 JSON
        _output=$(get_full_status 2>>"$LOG_FILE")
        if [ -z "$_output" ]; then
            echo '{"timestamp":0,"datetime":"","global_enabled":1,"summary":{"total":0,"enabled":0,"connected":0,"disconnected":0},"clients":[],"devices":[],"error":"get_full_status returned empty"}'
            echo "[$(date '+%Y-%m-%d %H:%M:%S')] status.sh: get_full_status 返回空" >> "$LOG_FILE"
        else
            echo "$_output"
        fi
        # 本次查询完成后，后台并发探测所有客户端连通性
        # 结果写入缓存，供下次 status 查询使用（不阻塞当前响应）
        # 用子 shell 隔离：防止后台进程继承 popen pipe fd 导致 read("*a") 阻塞
        (nohup /usr/lib/proxypool/proxypool.sh probe_all >/dev/null 2>&1 &)
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
