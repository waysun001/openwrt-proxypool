#
# Copyright 2026 ProxyPool contributors
# Licensed to the public under the Apache License 2.0.
#

include $(TOPDIR)/rules.mk

PKG_NAME:=luci-theme-proxypool
PKG_VERSION:=2.2.1
PKG_RELEASE:=1

PKG_LICENSE:=Apache-2.0
PKG_MAINTAINER:=ProxyPool contributors

LUCI_TITLE:=ZeanLink global LuCI theme
LUCI_DESCRIPTION:=Bootstrap-based LuCI theme with a persistent ZeanLink management bar
LUCI_DEPENDS:=+luci-base +luci-theme-bootstrap
LUCI_PKGARCH:=all
PKG_FILE_MODES:=/etc/uci-defaults/30_luci-theme-proxypool:root:root:0755 \
	/usr/share/ucode/luci/template/themes/proxypool/header.ut:root:root:0644 \
	/usr/share/ucode/luci/template/themes/proxypool/footer.ut:root:root:0644 \
	/usr/share/ucode/luci/template/themes/proxypool/sysauth.ut:root:root:0644 \
	/www/luci-static/proxypool/proxypool-global.css:root:root:0644 \
	/www/luci-static/proxypool/proxypool-global.js:root:root:0644

define Package/luci-theme-proxypool/postrm
#!/bin/sh
[ -n "$${IPKG_INSTROOT}" ] || {
	active="$$(uci -q get luci.main.mediaurlbase 2>/dev/null || true)"
	[ "$$active" != '/luci-static/proxypool' ] || uci -q set luci.main.mediaurlbase='/luci-static/bootstrap'
	uci -q delete luci.themes.ProxyPool
	uci -q commit luci
	rm -f /tmp/luci-indexcache
	rm -rf /tmp/luci-modulecache
}
exit 0
endef

include $(TOPDIR)/feeds/luci/luci.mk

# call BuildPackage - OpenWrt buildroot signature
