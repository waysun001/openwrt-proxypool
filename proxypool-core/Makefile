include $(TOPDIR)/rules.mk

PKG_NAME:=proxypool-core
PKG_VERSION:=2.0.0
PKG_RELEASE:=2

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
  DEPENDS:=$(GO_ARCH_DEPENDS) +xl2tpd +ppp +ppp-mod-pppol2tp +redsocks +firewall4 +kmod-nft-bridge +uci +ucode +ucode-mod-ubus +ubus +jshn +nftables +ip-full +ip-bridge +coreutils-stat +coreutils-timeout +conntrack +curl
endef

define Package/proxypool-core/description
  ProxyPool V2 live control plane with the quarantined V1 runtime retained for migration diagnostics.
endef

define Package/proxypool-core/conffiles
/etc/config/proxypool
endef

define Package/proxypool-core/postinst
#!/bin/sh
"$${IPKG_INSTROOT}/usr/lib/proxypool/proxypool-postinst"
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
	$(INSTALL_BIN) ./files/proxypool-guard.init $(1)/etc/init.d/proxypool-guard

	$(INSTALL_DIR) $(1)/etc/hotplug.d/net $(1)/etc/hotplug.d/iface
	$(INSTALL_BIN) ./files/proxypool-lan-isolation.hotplug $(1)/etc/hotplug.d/net/99-proxypool-lan-isolation
	$(INSTALL_BIN) ./files/proxypool-lan-isolation.hotplug $(1)/etc/hotplug.d/iface/99-proxypool-lan-isolation
	$(INSTALL_BIN) ./files/proxypool-netifd-event $(1)/etc/hotplug.d/iface/98-proxypool-v2-event

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
	$(INSTALL_BIN) ./files/guard-resync.sh $(1)/usr/lib/proxypool/guard-resync.sh
	$(INSTALL_BIN) ./files/legacy-gate.sh $(1)/usr/lib/proxypool/legacy-gate.sh
	$(INSTALL_BIN) ./files/lan-isolation.sh $(1)/usr/lib/proxypool/lan-isolation.sh
	$(INSTALL_BIN) ./files/lan-isolation-worker.sh $(1)/usr/lib/proxypool/lan-isolation-worker.sh
	$(INSTALL_BIN) ./files/proxypool-firewall-defaults $(1)/usr/lib/proxypool/proxypool-firewall-defaults
	$(INSTALL_BIN) ./files/proxypool-firewall-transaction $(1)/usr/lib/proxypool/proxypool-firewall-transaction
	$(INSTALL_BIN) ./files/proxypool-fw4-activate $(1)/usr/lib/proxypool/proxypool-fw4-activate
	$(INSTALL_BIN) ./files/proxypool-fw4-check-staged $(1)/usr/lib/proxypool/proxypool-fw4-check-staged
	$(INSTALL_BIN) ./files/proxypool-postinst $(1)/usr/lib/proxypool/proxypool-postinst
	$(INSTALL_BIN) ./files/proxypool-safety-uci-default $(1)/usr/lib/proxypool/proxypool-safety-uci-default
	$(INSTALL_DATA) ./files/proxypool-guard.nft $(1)/usr/lib/proxypool/proxypool-guard.nft
	$(INSTALL_DATA) ./files/proxypool-fw4-input-gate.nft $(1)/usr/lib/proxypool/proxypool-fw4-input-gate.nft
	$(INSTALL_DATA) ./files/proxypool-fw4-forward-gate.nft $(1)/usr/lib/proxypool/proxypool-fw4-forward-gate.nft
	$(INSTALL_DATA) ./files/proxypool-uci-staged.uc $(1)/usr/lib/proxypool/proxypool-uci-staged.uc
	$(INSTALL_BIN) ./files/proxypool-ubus-call-stdin.uc $(1)/usr/lib/proxypool/ubus-call-stdin.uc
	$(INSTALL_BIN) $(PKG_BUILD_DIR)/ip2region_searcher $(1)/usr/lib/proxypool/ip2region_searcher
	$(INSTALL_DATA) ./files/ip2region.xdb $(1)/usr/lib/proxypool/ip2region.xdb

	$(INSTALL_DIR) $(1)/usr/sbin
	$(INSTALL_BIN) $(GO_PKG_BUILD_BIN_DIR)/proxypoold $(1)/usr/sbin/proxypoold

	$(INSTALL_DIR) $(1)/usr/bin
	$(INSTALL_BIN) $(GO_PKG_BUILD_BIN_DIR)/proxypoolctl $(1)/usr/bin/proxypoolctl
	$(INSTALL_BIN) ./files/slp-client $(1)/usr/bin/slp-client
endef

$(eval $(call BuildPackage,proxypool-core))
