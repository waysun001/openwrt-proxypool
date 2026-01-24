#!/bin/bash
# 本地编译脚本（需要 Docker）

set -e

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
PROJECT_DIR="$(dirname "$SCRIPT_DIR")"

OPENWRT_VERSION="${1:-23.05.3}"
DEVICE="gl-mt6000"

echo "=== OpenWrt ProxyPool 本地编译 ==="
echo "OpenWrt 版本: $OPENWRT_VERSION"
echo "目标设备: $DEVICE"
echo ""

# 检查 Docker
if ! command -v docker &> /dev/null; then
    echo "错误: 需要安装 Docker"
    exit 1
fi

# 创建编译目录
BUILD_DIR="$PROJECT_DIR/build"
mkdir -p "$BUILD_DIR"

# 使用 Docker 编译
docker run --rm -it \
    -v "$PROJECT_DIR:/src" \
    -v "$BUILD_DIR:/build" \
    -e OPENWRT_VERSION="$OPENWRT_VERSION" \
    openwrt/sdk:mediatek-filogic-$OPENWRT_VERSION \
    /bin/bash -c '
        set -e

        echo "=== 准备编译环境 ==="

        # 复制源码
        cp -r /src/proxypool-core /build/package/
        cp -r /src/luci-app-proxypool /build/package/

        cd /build

        # 更新 feeds
        ./scripts/feeds update -a
        ./scripts/feeds install -a

        # 安装软件包
        ./scripts/feeds install proxypool-core luci-app-proxypool

        # 配置
        cp /src/config/gl-mt6000.config .config
        make defconfig

        # 下载依赖
        make download -j8

        # 编译
        make package/proxypool-core/compile V=s
        make package/luci-app-proxypool/compile V=s

        echo "=== 编译完成 ==="
        ls -la bin/packages/*/proxypool/
    '

echo ""
echo "编译完成！输出文件在 $BUILD_DIR/bin/"
