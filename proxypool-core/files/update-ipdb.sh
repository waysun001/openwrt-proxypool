#!/bin/sh
# 下载并安装 ip2region v2.0 数据库和查询工具

LIB_DIR="/usr/lib/proxypool"
XDB_FILE="$LIB_DIR/ip2region.xdb"
SEARCHER="$LIB_DIR/ip2region_searcher"
TEMP_DIR="/tmp/ip2region_install"

echo "=== ip2region 安装脚本 ==="
echo "正在下载 ip2region v2.0 数据库和查询工具..."

mkdir -p "$TEMP_DIR"
cd "$TEMP_DIR" || exit 1

# 1. 下载 ip2region.xdb 数据库（~11MB）
echo "[1/3] 下载 ip2region.xdb 数据库..."
wget -O ip2region.xdb "https://github.com/lionsoul2014/ip2region/raw/master/data/ip2region.xdb" 2>&1 | grep -E "saved|failed|error"

if [ ! -f ip2region.xdb ] || [ ! -s ip2region.xdb ]; then
    echo "错误: 数据库下载失败"
    rm -rf "$TEMP_DIR"
    exit 1
fi

# 2. 下载预编译的查询工具（C语言版本）
echo "[2/3] 下载查询工具..."

# 尝试下载预编译版本（如果有）
# 注意：ip2region 官方没有直接提供预编译版本，需要自己编译
# 这里我们下载源码并尝试简单编译

wget -O searcher.c "https://github.com/lionsoul2014/ip2region/raw/master/binding/c/xdb_searcher.c" 2>/dev/null
wget -O searcher.h "https://github.com/lionsoul2014/ip2region/raw/master/binding/c/xdb_searcher.h" 2>/dev/null
wget -O test.c "https://github.com/lionsoul2014/ip2region/raw/master/binding/c/test_xdb_searcher.c" 2>/dev/null

# 如果系统有 gcc，尝试编译
if command -v gcc >/dev/null 2>&1; then
    echo "检测到 gcc，正在编译查询工具..."
    gcc -O2 -o ip2region_searcher test.c searcher.c 2>/dev/null
    
    if [ -f ip2region_searcher ]; then
        echo "编译成功"
    else
        echo "编译失败，将使用脚本查询方式"
    fi
else
    echo "未检测到 gcc，将使用脚本查询方式"
fi

# 3. 安装文件
echo "[3/3] 安装文件..."
mkdir -p "$LIB_DIR"
mv ip2region.xdb "$XDB_FILE"
[ -f ip2region_searcher ] && mv ip2region_searcher "$SEARCHER" && chmod +x "$SEARCHER"

rm -rf "$TEMP_DIR"

echo ""
echo "=== 安装完成 ==="
echo "数据库: $XDB_FILE ($(du -h "$XDB_FILE" 2>/dev/null | cut -f1))"
if [ -f "$SEARCHER" ]; then
    echo "查询工具: $SEARCHER"
else
    echo "查询工具: 未安装（将使用回退方案）"
fi
echo ""
echo "测试查询: /usr/lib/proxypool/iplocation.sh 8.8.8.8"
/usr/lib/proxypool/iplocation.sh 8.8.8.8
