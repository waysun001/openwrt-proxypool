include $(TOPDIR)/rules.mk

PKG_NAME:=proxypool-core
PKG_VERSION:=1.0.0
PKG_RELEASE:=1

PKG_MAINTAINER:=ProxyPool <proxypool@local>
PKG_LICENSE:=MIT

include $(INCLUDE_DIR)/package.mk

define Package/proxypool-core
  SECTION:=net
  CATEGORY:=Network
  TITLE:=ProxyPool Core Service
  DEPENDS:=+xl2tpd +ppp +ppp-mod-pppol2tp +redsocks +nftables +ip-full +bash +jq
  PKGARCH:=all
endef

define Package/proxypool-core/description
  Core service for ProxyPool - Multi-proxy client management system
  Supports L2TP and SOCKS5 protocols with strict network isolation
endef

define Package/proxypool-core/conffiles
/etc/config/proxypool
endef

define Build/Compile
endef

define Package/proxypool-core/install
	$(INSTALL_DIR) $(1)/etc/config
	$(INSTALL_CONF) ./files/proxypool.config $(1)/etc/config/proxypool

	$(INSTALL_DIR) $(1)/etc/init.d
	$(INSTALL_BIN) ./files/proxypool.init $(1)/etc/init.d/proxypool

	$(INSTALL_DIR) $(1)/usr/lib/proxypool
	$(INSTALL_BIN) ./files/proxypool.sh $(1)/usr/lib/proxypool/proxypool.sh
	$(INSTALL_BIN) ./files/l2tp-manager.sh $(1)/usr/lib/proxypool/l2tp-manager.sh
	$(INSTALL_BIN) ./files/socks5-manager.sh $(1)/usr/lib/proxypool/socks5-manager.sh
	$(INSTALL_BIN) ./files/firewall.sh $(1)/usr/lib/proxypool/firewall.sh
	$(INSTALL_BIN) ./files/status.sh $(1)/usr/lib/proxypool/status.sh
	$(INSTALL_BIN) ./files/backup.sh $(1)/usr/lib/proxypool/backup.sh

	$(INSTALL_DIR) $(1)/etc/uci-defaults
	$(INSTALL_BIN) ./files/99-proxypool $(1)/etc/uci-defaults/99-proxypool

	$(INSTALL_DIR) $(1)/usr/share/proxypool
	$(INSTALL_DATA) ./files/firewall.nft $(1)/usr/share/proxypool/firewall.nft

	$(INSTALL_DIR) $(1)/etc/ppp/ip-up.d
	$(INSTALL_DIR) $(1)/etc/ppp/ip-down.d
	$(INSTALL_BIN) ./files/ppp-up.sh $(1)/etc/ppp/ip-up.d/99-proxypool
	$(INSTALL_BIN) ./files/ppp-down.sh $(1)/etc/ppp/ip-down.d/99-proxypool
endef

$(eval $(call BuildPackage,proxypool-core))
