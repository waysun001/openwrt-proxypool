#!/bin/bash
# 智联盒子 - 备份/恢复脚本

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

create_backup() {
    local output_file="${1:-/tmp/proxypool_backup.tar.gz}"

    log_info "Creating backup to $output_file"

    mkdir -p "$BACKUP_DIR"

    cp "$CONFIG_FILE" "$BACKUP_DIR/proxypool"

    cat > "$BACKUP_DIR/version.json" << EOF
{
    "version": "1.0.0",
    "created": "$(date '+%Y-%m-%d %H:%M:%S')",
    "timestamp": $(date +%s)
}
EOF

    uci export proxypool > "$BACKUP_DIR/proxypool.uci" 2>/dev/null || true

    cd /tmp
    tar -czf "$output_file" -C "$BACKUP_DIR" .

    rm -rf "$BACKUP_DIR"

    log_info "Backup created: $output_file"
    echo "$output_file"
}

restore_backup() {
    local input_file="$1"

    if [ ! -f "$input_file" ]; then
        log_error "Backup file not found: $input_file"
        return 1
    fi

    log_info "Restoring backup from $input_file"

    mkdir -p "$BACKUP_DIR"

    tar -xzf "$input_file" -C "$BACKUP_DIR"

    if [ -f "$BACKUP_DIR/version.json" ]; then
        local version=$(grep '"version"' "$BACKUP_DIR/version.json" | cut -d'"' -f4)
        log_info "Backup version: $version"
    fi

    /etc/init.d/proxypool stop 2>/dev/null || true

    if [ -f "$BACKUP_DIR/proxypool" ]; then
        cp "$BACKUP_DIR/proxypool" "$CONFIG_FILE"
        log_info "Configuration restored"
    else
        log_error "No configuration found in backup"
        rm -rf "$BACKUP_DIR"
        return 1
    fi

    rm -rf "$BACKUP_DIR"

    /etc/init.d/proxypool start

    log_info "Backup restored successfully"
}

verify_backup() {
    local input_file="$1"

    if [ ! -f "$input_file" ]; then
        echo '{"valid": false, "error": "File not found"}'
        return 1
    fi

    mkdir -p "$BACKUP_DIR"

    if ! tar -xzf "$input_file" -C "$BACKUP_DIR" 2>/dev/null; then
        echo '{"valid": false, "error": "Invalid archive"}'
        rm -rf "$BACKUP_DIR"
        return 1
    fi

    if [ ! -f "$BACKUP_DIR/proxypool" ]; then
        echo '{"valid": false, "error": "Missing configuration"}'
        rm -rf "$BACKUP_DIR"
        return 1
    fi

    local version="unknown"
    local created="unknown"
    if [ -f "$BACKUP_DIR/version.json" ]; then
        version=$(grep '"version"' "$BACKUP_DIR/version.json" | cut -d'"' -f4)
        created=$(grep '"created"' "$BACKUP_DIR/version.json" | cut -d'"' -f4)
    fi

    local client_count=$(grep -c "config client" "$BACKUP_DIR/proxypool" 2>/dev/null || echo 0)

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
    *)
        echo "Usage: $0 {create|restore|verify} [file]"
        exit 1
        ;;
esac
