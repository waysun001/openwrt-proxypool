#!/bin/bash
# ProxyPool 备份/恢复脚本

set -e

BACKUP_DIR="/tmp/proxypool_backup"
CONFIG_FILE="/etc/config/proxypool"
LOG_FILE="/var/log/proxypool.log"

log_info() {
    echo "[$(date '+%Y-%m-%d %H:%M:%S')] [backup] $*" >> "$LOG_FILE"
}

log_error() {
    echo "[$(date '+%Y-%m-%d %H:%M:%S')] [backup] [ERROR] $*" >> "$LOG_FILE"
    echo "[ERROR] $*" >&2
}

# 创建备份
create_backup() {
    local output_file="${1:-/tmp/proxypool_backup.tar.gz}"

    log_info "Creating backup to $output_file"

    mkdir -p "$BACKUP_DIR"

    # 复制配置文件
    cp "$CONFIG_FILE" "$BACKUP_DIR/proxypool"

    # 保存版本信息
    cat > "$BACKUP_DIR/version.json" << EOF
{
    "version": "1.0.0",
    "created": "$(date '+%Y-%m-%d %H:%M:%S')",
    "timestamp": $(date +%s)
}
EOF

    # 导出UCI配置为可读格式
    uci export proxypool > "$BACKUP_DIR/proxypool.uci" 2>/dev/null || true

    # 打包
    cd /tmp
    tar -czf "$output_file" -C "$BACKUP_DIR" .

    # 清理
    rm -rf "$BACKUP_DIR"

    log_info "Backup created: $output_file"
    echo "$output_file"
}

# 恢复备份
restore_backup() {
    local input_file="$1"

    if [ ! -f "$input_file" ]; then
        log_error "Backup file not found: $input_file"
        return 1
    fi

    log_info "Restoring backup from $input_file"

    mkdir -p "$BACKUP_DIR"

    # 解压
    tar -xzf "$input_file" -C "$BACKUP_DIR"

    # 检查版本
    if [ -f "$BACKUP_DIR/version.json" ]; then
        local version=$(grep -oP '(?<="version": ")[^"]+' "$BACKUP_DIR/version.json")
        log_info "Backup version: $version"
    fi

    # 停止服务
    /etc/init.d/proxypool stop 2>/dev/null || true

    # 恢复配置
    if [ -f "$BACKUP_DIR/proxypool" ]; then
        cp "$BACKUP_DIR/proxypool" "$CONFIG_FILE"
        log_info "Configuration restored"
    else
        log_error "No configuration found in backup"
        rm -rf "$BACKUP_DIR"
        return 1
    fi

    # 清理
    rm -rf "$BACKUP_DIR"

    # 重启服务
    /etc/init.d/proxypool start

    log_info "Backup restored successfully"
}

# 验证备份文件
verify_backup() {
    local input_file="$1"

    if [ ! -f "$input_file" ]; then
        echo "error: File not found"
        return 1
    fi

    mkdir -p "$BACKUP_DIR"

    # 尝试解压
    if ! tar -xzf "$input_file" -C "$BACKUP_DIR" 2>/dev/null; then
        echo "error: Invalid archive"
        rm -rf "$BACKUP_DIR"
        return 1
    fi

    # 检查必要文件
    if [ ! -f "$BACKUP_DIR/proxypool" ]; then
        echo "error: Missing configuration"
        rm -rf "$BACKUP_DIR"
        return 1
    fi

    # 读取版本信息
    local version="unknown"
    local created="unknown"
    if [ -f "$BACKUP_DIR/version.json" ]; then
        version=$(grep -oP '(?<="version": ")[^"]+' "$BACKUP_DIR/version.json")
        created=$(grep -oP '(?<="created": ")[^"]+' "$BACKUP_DIR/version.json")
    fi

    # 统计客户端数量
    local client_count=$(grep -c "config client" "$BACKUP_DIR/proxypool" 2>/dev/null || echo 0)

    # 清理
    rm -rf "$BACKUP_DIR"

    cat << EOF
{
    "valid": true,
    "version": "$version",
    "created": "$created",
    "clients": $client_count
}
EOF
}

# 导出为CSV格式（便于查看）
export_csv() {
    local output_file="${1:-/tmp/proxypool_clients.csv}"

    echo "ID,Name,Type,Server,Port,Enabled,Bind IPs" > "$output_file"

    local clients=$(uci show proxypool 2>/dev/null | grep "=client" | cut -d'.' -f2 | cut -d'=' -f1)

    for client in $clients; do
        local name=$(uci -q get "proxypool.$client.name" || echo "")
        local type=$(uci -q get "proxypool.$client.type" || echo "")
        local server=$(uci -q get "proxypool.$client.server" || echo "")
        local port=$(uci -q get "proxypool.$client.port" || echo "")
        local enabled=$(uci -q get "proxypool.$client.enabled" || echo "0")
        local bind_ips=$(uci -q get "proxypool.$client.bind_ip" 2>/dev/null | tr ' ' ';')

        echo "$client,$name,$type,$server,$port,$enabled,$bind_ips" >> "$output_file"
    done

    log_info "Exported to CSV: $output_file"
    echo "$output_file"
}

# 从CSV导入
import_csv() {
    local input_file="$1"

    if [ ! -f "$input_file" ]; then
        log_error "CSV file not found: $input_file"
        return 1
    fi

    log_info "Importing from CSV: $input_file"

    # 跳过标题行
    tail -n +2 "$input_file" | while IFS=, read -r id name type server port enabled bind_ips; do
        [ -z "$id" ] && continue

        # 创建或更新客户端
        uci set "proxypool.$id=client"
        [ -n "$name" ] && uci set "proxypool.$id.name=$name"
        [ -n "$type" ] && uci set "proxypool.$id.type=$type"
        [ -n "$server" ] && uci set "proxypool.$id.server=$server"
        [ -n "$port" ] && uci set "proxypool.$id.port=$port"
        [ -n "$enabled" ] && uci set "proxypool.$id.enabled=$enabled"

        # 处理绑定IP（分号分隔）
        if [ -n "$bind_ips" ]; then
            uci -q delete "proxypool.$id.bind_ip" 2>/dev/null || true
            echo "$bind_ips" | tr ';' '\n' | while read -r ip; do
                [ -n "$ip" ] && uci add_list "proxypool.$id.bind_ip=$ip"
            done
        fi

        log_info "Imported client: $id"
    done

    uci commit proxypool
    log_info "CSV import completed"
}

# 主入口
case "$1" in
    create)
        create_backup "$2"
        ;;
    restore)
        restore_backup "$2"
        ;;
    verify)
        verify_backup "$2"
        ;;
    export_csv)
        export_csv "$2"
        ;;
    import_csv)
        import_csv "$2"
        ;;
    *)
        echo "Usage: $0 {create|restore|verify|export_csv|import_csv} [file]"
        exit 1
        ;;
esac
