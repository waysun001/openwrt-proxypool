include $(TOPDIR)/rules.mk

PKG_NAME:=proxypool-core
PKG_VERSION:=2.0.0
PKG_RELEASE:=1

PKG_MAINTAINER:=ProxyPool maintainers
PKG_LICENSE:=MIT
PKG_BUILD_DEPENDS:=golang/host
PKG_BUILD_PARALLEL:=1

GO_PKG:=proxypoold
GO_PKG_BUILD_PKG:=$(GO_PKG)/cmd/proxypoold $(GO_PKG)/cmd/proxypoolctl
GO_PKG_LDFLAGS_X:=$(GO_PKG)/internal/buildinfo.Version=$(PKG_VERSION)

include $(INCLUDE_DIR)/package.mk
include $(TOPDIR)/feeds/packages/lang/golang/golang-package.mk

define Package/proxypool-core
  SECTION:=net
  CATEGORY:=Network
  TITLE:=ProxyPool control plane and legacy runtime
  DEPENDS:=$(GO_ARCH_DEPENDS) +xl2tpd +ppp +ppp-mod-pppol2tp +redsocks +nftables +ip-full +conntrack +curl
endef

define Package/proxypool-core/description
  ProxyPool V2 shadow control plane with the complete V1 runtime retained for rollback.
endef

define Package/proxypool-core/conffiles
/etc/config/proxypool
endef

define Build/Compile/Ip2Region
	$(TARGET_CC) $(TARGET_CFLAGS) $(TARGET_LDFLAGS) \
		-o $(PKG_BUILD_DIR)/ip2region_searcher \
		$(PKG_BUILD_DIR)/csrc/ip2region_searcher.c
endef

Hooks/Compile/Post+=Build/Compile/Ip2Region

define Build/Prepare
	$(call Build/Prepare/Default)
	$(CP) ./src/proxypoold/. $(PKG_BUILD_DIR)/
	$(INSTALL_DIR) $(PKG_BUILD_DIR)/csrc
	$(CP) ./src/ip2region_searcher.c $(PKG_BUILD_DIR)/csrc/
endef

define Package/proxypool-core/install
	$(INSTALL_DIR) $(1)/etc/config
	$(INSTALL_CONF) ./files/proxypool.config $(1)/etc/config/proxypool

	$(INSTALL_DIR) $(1)/etc/init.d
	$(INSTALL_BIN) ./files/proxypool.init $(1)/etc/init.d/proxypool

	$(INSTALL_DIR) $(1)/lib/upgrade/keep.d
	$(INSTALL_DATA) ./files/proxypool.keep $(1)/lib/upgrade/keep.d/proxypool

	$(INSTALL_DIR) $(1)/usr/lib/proxypool
	$(INSTALL_BIN) ./files/proxypool.sh $(1)/usr/lib/proxypool/proxypool.sh
	$(INSTALL_BIN) ./files/l2tp-manager.sh $(1)/usr/lib/proxypool/l2tp-manager.sh
	$(INSTALL_BIN) ./files/socks5-manager.sh $(1)/usr/lib/proxypool/socks5-manager.sh
	$(INSTALL_BIN) ./files/slp-manager.sh $(1)/usr/lib/proxypool/slp-manager.sh
	$(INSTALL_BIN) ./files/firewall.sh $(1)/usr/lib/proxypool/firewall.sh
	$(INSTALL_BIN) ./files/status.sh $(1)/usr/lib/proxypool/status.sh
	$(INSTALL_BIN) ./files/backup.sh $(1)/usr/lib/proxypool/backup.sh
	$(INSTALL_BIN) ./files/watchdog.sh $(1)/usr/lib/proxypool/watchdog.sh
	$(INSTALL_BIN) ./files/lease.sh $(1)/usr/lib/proxypool/lease.sh
	$(INSTALL_BIN) ./files/iplocation.sh $(1)/usr/lib/proxypool/iplocation.sh
	$(INSTALL_BIN) ./files/timeout-check.sh $(1)/usr/lib/proxypool/timeout-check.sh
	$(INSTALL_BIN) ./files/timeout-rotate.sh $(1)/usr/lib/proxypool/timeout-rotate.sh
	$(INSTALL_BIN) ./files/update-ipdb.sh $(1)/usr/lib/proxypool/update-ipdb.sh
	$(INSTALL_BIN) ./files/install-global-menu.sh $(1)/usr/lib/proxypool/install-global-menu.sh
	$(INSTALL_BIN) ./files/uninstall-global-menu.sh $(1)/usr/lib/proxypool/uninstall-global-menu.sh
	$(INSTALL_BIN) ./files/dns-manager.sh $(1)/usr/lib/proxypool/dns-manager.sh
	$(INSTALL_BIN) $(PKG_BUILD_DIR)/ip2region_searcher $(1)/usr/lib/proxypool/ip2region_searcher
	$(INSTALL_DATA) ./files/ip2region.xdb $(1)/usr/lib/proxypool/ip2region.xdb

	$(INSTALL_DIR) $(1)/usr/sbin
	$(INSTALL_BIN) $(GO_PKG_BUILD_BIN_DIR)/proxypoold $(1)/usr/sbin/proxypoold

	$(INSTALL_DIR) $(1)/usr/bin
	$(INSTALL_BIN) $(GO_PKG_BUILD_BIN_DIR)/proxypoolctl $(1)/usr/bin/proxypoolctl
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
