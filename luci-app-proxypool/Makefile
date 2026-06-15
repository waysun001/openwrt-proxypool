include $(TOPDIR)/rules.mk

PKG_NAME:=luci-app-proxypool
PKG_VERSION:=1.0.0
PKG_RELEASE:=1

PKG_LICENSE:=MIT
PKG_MAINTAINER:=智联盒子

LUCI_TITLE:=智联盒子管理界面
LUCI_DESCRIPTION:=多代理客户端管理，支持严格网络隔离
LUCI_DEPENDS:=+proxypool-core +luci-base +luci-lua-runtime +luci-compat +luci-lib-jsonc
LUCI_PKGARCH:=all

include $(TOPDIR)/feeds/luci/luci.mk

define Package/luci-app-proxypool/install
	$(INSTALL_DIR) $(1)/usr/lib/lua/luci/controller
	$(INSTALL_DIR) $(1)/usr/lib/lua/luci/view/proxypool
	$(INSTALL_DIR) $(1)/etc/uci-defaults
	$(INSTALL_DIR) $(1)/www/luci-static/resources

	$(INSTALL_DATA) ./luasrc/controller/proxypool.lua $(1)/usr/lib/lua/luci/controller/
	$(INSTALL_DATA) ./luasrc/view/proxypool/main.htm $(1)/usr/lib/lua/luci/view/proxypool/
	$(INSTALL_DATA) ./luasrc/view/proxypool/locked.htm $(1)/usr/lib/lua/luci/view/proxypool/
	$(INSTALL_DATA) ./luasrc/view/proxypool/lease.htm $(1)/usr/lib/lua/luci/view/proxypool/
	$(INSTALL_DATA) ./htdocs/luci-static/resources/proxypool-global.css $(1)/www/luci-static/resources/
	$(INSTALL_DATA) ./htdocs/luci-static/resources/proxypool-global.js $(1)/www/luci-static/resources/
	$(INSTALL_BIN) ./root/etc/uci-defaults/luci-proxypool $(1)/etc/uci-defaults/
endef

$(eval $(call BuildPackage,luci-app-proxypool))
