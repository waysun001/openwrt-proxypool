include $(TOPDIR)/rules.mk

PKG_NAME:=luci-app-proxypool
PKG_VERSION:=2.0.1
PKG_RELEASE:=1

PKG_LICENSE:=MIT
PKG_MAINTAINER:=智联盒子

LUCI_TITLE:=智联盒子管理界面
LUCI_DESCRIPTION:=多代理客户端管理，支持严格网络隔离
LUCI_DEPENDS:=+proxypool-core +luci-base +luci-lua-runtime +luci-compat +luci-lib-jsonc
LUCI_PKGARCH:=all
PKG_FILE_MODES:=/etc/uci-defaults/luci-proxypool:root:root:0755

include $(TOPDIR)/feeds/luci/luci.mk

# call BuildPackage - OpenWrt buildroot signature
