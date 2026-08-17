include $(TOPDIR)/rules.mk

PKG_NAME:=luci-app-proxypool
PKG_VERSION:=2.2.0
PKG_RELEASE:=1

PKG_LICENSE:=MIT
PKG_MAINTAINER:=智联盒子

LUCI_TITLE:=智联盒子管理界面
LUCI_DESCRIPTION:=多代理客户端管理，支持严格网络隔离
LUCI_DEPENDS:=+proxypool-core +luci-base +luci-lua-runtime +luci-compat +luci-lib-jsonc +luci-theme-proxypool
LUCI_PKGARCH:=all
PKG_FILE_MODES:=/etc/uci-defaults/luci-proxypool:root:root:0755 \
	/usr/lib/lua/luci/controller/proxypool.lua:root:root:0644 \
	/usr/lib/lua/luci/model/proxypool_rpc.lua:root:root:0644 \
	/usr/lib/lua/luci/view/proxypool/lease.htm:root:root:0644 \
	/usr/lib/lua/luci/view/proxypool/locked.htm:root:root:0644 \
	/usr/lib/lua/luci/view/proxypool/main.htm:root:root:0644 \
	/www/luci-static/resources/proxypool-v2.css:root:root:0644 \
	/www/luci-static/resources/proxypool-v2.js:root:root:0644

include $(TOPDIR)/feeds/luci/luci.mk

# call BuildPackage - OpenWrt buildroot signature
