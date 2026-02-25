include $(TOPDIR)/rules.mk

PKG_NAME:=proxypool-core
PKG_VERSION:=1.0.0
PKG_RELEASE:=1

PKG_MAINTAINER:=智联盒子
PKG_LICENSE:=MIT

include $(INCLUDE_DIR)/package.mk

define Package/proxypool-core
  SECTION:=net
  CATEGORY:=Network
  TITLE:=智联盒子核心服务
  DEPENDS:=+xl2tpd +ppp +ppp-mod-pppol2tp +redsocks +nftables +ip-full +conntrack +curl
endef

define Package/proxypool-core/description
  智联盒子核心服务 - 多代理客户端管理系统
  支持 L2TP、SOCKS5 和 SLP 协议，严格网络隔离
endef

define Package/proxypool-core/conffiles
/etc/config/proxypool
endef

define Build/Compile
	$(TARGET_CC) $(TARGET_CFLAGS) $(TARGET_LDFLAGS) \
		-o $(PKG_BUILD_DIR)/ip2region_searcher \
		$(PKG_BUILD_DIR)/src/ip2region_searcher.c
endef

define Build/Prepare
	$(call Build/Prepare/Default)
	$(INSTALL_DIR) $(PKG_BUILD_DIR)/src
	$(CP) ./src/ip2region_searcher.c $(PKG_BUILD_DIR)/src/
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
	$(INSTALL_BIN) ./files/slp-manager.sh $(1)/usr/lib/proxypool/slp-manager.sh
	$(INSTALL_BIN) ./files/firewall.sh $(1)/usr/lib/proxypool/firewall.sh
	$(INSTALL_BIN) ./files/status.sh $(1)/usr/lib/proxypool/status.sh
	$(INSTALL_BIN) ./files/backup.sh $(1)/usr/lib/proxypool/backup.sh
	$(INSTALL_BIN) ./files/watchdog.sh $(1)/usr/lib/proxypool/watchdog.sh
	$(INSTALL_BIN) ./files/iplocation.sh $(1)/usr/lib/proxypool/iplocation.sh
	$(INSTALL_BIN) ./files/timeout-check.sh $(1)/usr/lib/proxypool/timeout-check.sh
	$(INSTALL_BIN) ./files/timeout-rotate.sh $(1)/usr/lib/proxypool/timeout-rotate.sh
	$(INSTALL_BIN) ./files/update-ipdb.sh $(1)/usr/lib/proxypool/update-ipdb.sh
	$(INSTALL_BIN) ./files/install-global-menu.sh $(1)/usr/lib/proxypool/install-global-menu.sh
	$(INSTALL_BIN) ./files/uninstall-global-menu.sh $(1)/usr/lib/proxypool/uninstall-global-menu.sh
	$(INSTALL_BIN) ./files/dns-manager.sh $(1)/usr/lib/proxypool/dns-manager.sh
	$(INSTALL_BIN) $(PKG_BUILD_DIR)/ip2region_searcher $(1)/usr/lib/proxypool/ip2region_searcher
	$(INSTALL_DATA) ./files/ip2region.xdb $(1)/usr/lib/proxypool/ip2region.xdb

	$(INSTALL_DIR) $(1)/usr/bin
	$(INSTALL_BIN) ./files/slp-client $(1)/usr/bin/slp-client

	$(INSTALL_DIR) $(1)/etc/ppp
	$(INSTALL_BIN) ./files/ip-up $(1)/etc/ppp/ip-up
	$(INSTALL_BIN) ./files/ip-down $(1)/etc/ppp/ip-down

	$(INSTALL_DIR) $(1)/etc/ppp/ip-up.d
	$(INSTALL_DIR) $(1)/etc/ppp/ip-down.d
	$(INSTALL_BIN) ./files/ppp-up.sh $(1)/etc/ppp/ip-up.d/99-proxypool
	$(INSTALL_BIN) ./files/ppp-down.sh $(1)/etc/ppp/ip-down.d/99-proxypool
endef

$(eval $(call BuildPackage,proxypool-core))
