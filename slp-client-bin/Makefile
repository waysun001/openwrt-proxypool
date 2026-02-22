include $(TOPDIR)/rules.mk

PKG_NAME:=slp-client-bin
PKG_VERSION:=1.0.0
PKG_RELEASE:=1

PKG_MAINTAINER:=智联盒子
PKG_LICENSE:=MIT

# OpenWrt ARCH -> Go ARCH 映射
SLP_ARCH_MAP_aarch64:=arm64
SLP_ARCH_MAP_x86_64:=amd64
SLP_ARCH_MAP_mipsel:=mipsle
SLP_ARCH_MAP_mips:=mips
SLP_ARCH_MAP_arm:=arm64
SLP_ARCH:=$(SLP_ARCH_MAP_$(ARCH))

ifeq ($(SLP_ARCH),)
  $(error slp-client-bin: unsupported architecture: $(ARCH))
endif

include $(INCLUDE_DIR)/package.mk

define Package/slp-client-bin
  SECTION:=net
  CATEGORY:=Network
  TITLE:=SLP Client (pre-built binary)
endef

define Package/slp-client-bin/description
  SmartLink Protocol 客户端预编译二进制
endef

# 在多个可能的路径中搜索二进制文件（兼容 SDK 模式和完整编译模式）
define Build/Prepare
	mkdir -p $(PKG_BUILD_DIR)
	@BIN="slp-client-linux-$(SLP_ARCH)"; \
	FOUND=""; \
	for d in \
		$(TOPDIR)/package/proxypool/slp-client-bin/files \
		$(TOPDIR)/package/feeds/proxypool/slp-client-bin/files \
		$(TOPDIR)/feeds/proxypool/slp-client-bin/files; \
	do \
		if [ -f "$$d/$$BIN" ]; then \
			FOUND="$$d/$$BIN"; \
			echo "Found binary: $$FOUND"; \
			break; \
		fi; \
	done; \
	if [ -z "$$FOUND" ]; then \
		echo "ERROR: $$BIN not found in any known location"; \
		echo "Searched:"; \
		echo "  $(TOPDIR)/package/proxypool/slp-client-bin/files/"; \
		echo "  $(TOPDIR)/package/feeds/proxypool/slp-client-bin/files/"; \
		echo "  $(TOPDIR)/feeds/proxypool/slp-client-bin/files/"; \
		exit 1; \
	fi; \
	cp "$$FOUND" $(PKG_BUILD_DIR)/slp-client && \
	chmod +x $(PKG_BUILD_DIR)/slp-client
endef

define Build/Compile
endef

define Package/slp-client-bin/install
	$(INSTALL_DIR) $(1)/usr/bin
	$(INSTALL_BIN) $(PKG_BUILD_DIR)/slp-client $(1)/usr/bin/slp-client
endef

$(eval $(call BuildPackage,slp-client-bin))
