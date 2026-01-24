include $(TOPDIR)/rules.mk

PKG_NAME:=luci-app-proxypool
PKG_VERSION:=1.0.0
PKG_RELEASE:=1

PKG_LICENSE:=MIT
PKG_MAINTAINER:=ProxyPool

LUCI_TITLE:=LuCI support for ProxyPool
LUCI_DESCRIPTION:=Multi-proxy client management with strict network isolation
LUCI_DEPENDS:=+proxypool-core +luci-base +luci-lua-runtime +luci-compat +luci-lib-ipkg
LUCI_PKGARCH:=all

include $(TOPDIR)/feeds/luci/luci.mk

define Package/luci-app-proxypool/install
	$(INSTALL_DIR) $(1)/usr/lib/lua/luci/controller
	$(INSTALL_DIR) $(1)/usr/lib/lua/luci/model/cbi/proxypool
	$(INSTALL_DIR) $(1)/usr/lib/lua/luci/view/proxypool
	$(INSTALL_DIR) $(1)/etc/uci-defaults

	$(INSTALL_DATA) ./luasrc/controller/proxypool.lua $(1)/usr/lib/lua/luci/controller/
	$(INSTALL_DATA) ./luasrc/model/cbi/proxypool/*.lua $(1)/usr/lib/lua/luci/model/cbi/proxypool/
	$(INSTALL_DATA) ./luasrc/view/proxypool/*.htm $(1)/usr/lib/lua/luci/view/proxypool/
	$(INSTALL_BIN) ./root/etc/uci-defaults/luci-proxypool $(1)/etc/uci-defaults/
endef

$(eval $(call BuildPackage,luci-app-proxypool))
