#!/bin/sh
# 超时检测脚本 - 每 3 分钟检测一次已连接的客户端
# crontab: */3 * * * * /usr/lib/proxypool/timeout-check.sh

RUN_DIR="/var/run/proxypool"
TIMEOUT_DIR="$RUN_DIR/timeout"
LOG_FILE="/var/log/proxypool.log"

mkdir -p "$TIMEOUT_DIR"

log_info() {
    echo "[$(date '+%Y-%m-%d %H:%M:%S')] [timeout-check] $*" >> "$LOG_FILE"
}

get_config() {
    uci -q get "proxypool.$1.$2" || echo "$3"
}

get_clients() {
    uci show proxypool 2>/dev/null | grep "=client" | cut -d'.' -f2 | cut -d'=' -f1
}

# 检测 SOCKS5 客户端
check_socks5() {
    local client="$1"
    local server=$(get_config "$client" "server" "" | tr -d ' 	
')
    local port=$(get_config "$client" "port" "1080" | tr -d ' 	
')
    local user=$(get_config "$client" "username" "" | tr -d ' 	
')
    local pass=$(get_config "$client" "password" "" | tr -d ' 	
')
    
    local curl_bin=$(command -v curl 2>/dev/null)
    
    if [ -z "$curl_bin" ]; then
        # 没有 curl，用 nc 简单测端口
        if nc -z -w 3 "$server" "$port" >/dev/null 2>&1; then
            return 0
        else
            return 1
        fi
    fi
    
    # 用 curl 真实检测
    if [ -n "$user" ] || [ -n "$pass" ]; then
        "$curl_bin" --socks5 "${server}:${port}" --proxy-user "${user}:${pass}" \
            --max-time 5 --silent --output /dev/null --head https://ip.sb
    else
        "$curl_bin" --socks5 "${server}:${port}" \
            --max-time 5 --silent --output /dev/null --head https://ip.sb
    fi
    
    return $?
}

# 检测 L2TP 客户端
check_l2tp() {
    local client="$1"
    local ppp_iface="ppp-${client}"

    # 检查接口是否存在且有 IP
    if ! ip link show "$ppp_iface" >/dev/null 2>&1; then
        return 1
    fi

    local ip=$(ip -4 addr show "$ppp_iface" 2>/dev/null | grep -o 'inet [0-9.]*' | cut -d' ' -f2)
    if [ -z "$ip" ]; then
        return 1
    fi

    # 尝试 ping 网关或外网（可选，更严格）
    # ping -c 1 -W 2 8.8.8.8 -I "$ppp_iface" >/dev/null 2>&1
    # return $?

    return 0
}

# 检测 SLP 客户端
check_slp() {
    local client="$1"
    local config_dir="/var/run/proxypool/slp/${client}"
    local port_file="$config_dir/socks5.port"

    # 读取本地 SOCKS5 端口
    if [ ! -f "$port_file" ]; then
        return 1
    fi

    local socks5_port=$(cat "$port_file" 2>/dev/null)
    if [ -z "$socks5_port" ]; then
        return 1
    fi

    local curl_bin=$(command -v curl 2>/dev/null)

    if [ -z "$curl_bin" ]; then
        # 没有 curl，用 nc 简单测端口
        if nc -z -w 3 127.0.0.1 "$socks5_port" >/dev/null 2>&1; then
            return 0
        else
            return 1
        fi
    fi

    # 用 curl 通过本地 SOCKS5 端口检测
    "$curl_bin" --socks5 "127.0.0.1:${socks5_port}" \
        --max-time 5 --silent --output /dev/null --head https://ip.sb

    return $?
}

# 主逻辑
for client in $(get_clients); do
    local enabled=$(get_config "$client" "enabled" "0")
    local type=$(get_config "$client" "type" "")
    local name=$(get_config "$client" "name" "$client")

    # 只检测已启用的
    [ "$enabled" != "1" ] && continue

    # 获取当前状态
    local status="disconnected"
    case "$type" in
        socks5)
            local result=$(/usr/lib/proxypool/socks5-manager.sh status "$client" 2>/dev/null | head -1)
            status="$result"
            ;;
        l2tp)
            local result=$(/usr/lib/proxypool/l2tp-manager.sh status "$client" 2>/dev/null | head -1)
            status="$result"
            ;;
        slp)
            local result=$(/usr/lib/proxypool/slp-manager.sh status "$client" 2>/dev/null | head -1)
            status="$result"
            ;;
    esac

    # 只检测状态为 connected 的客户端
    [ "$status" != "connected" ] && continue

    # 执行检测
    local check_result=1
    case "$type" in
        socks5)
            check_socks5 "$client"
            check_result=$?
            ;;
        l2tp)
            check_l2tp "$client"
            check_result=$?
            ;;
        slp)
            check_slp "$client"
            check_result=$?
            ;;
    esac

    # 检测失败，计数 +1
    if [ $check_result -ne 0 ]; then
        local cur=$(cat "$TIMEOUT_DIR/${client}.today" 2>/dev/null || echo 0)
        echo $((cur + 1)) > "$TIMEOUT_DIR/${client}.today"
        log_info "Timeout detected: $name ($client) - count: $((cur + 1))"
    fi
done
