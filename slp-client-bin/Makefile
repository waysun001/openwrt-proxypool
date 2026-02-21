include $(TOPDIR)/rules.mk

PKG_NAME:=slp-client-bin
PKG_VERSION:=1.0.0
PKG_RELEASE:=1

PKG_MAINTAINER:=智联盒子
PKG_LICENSE:=MIT

# OpenWrt ARCH -> Go ARCH 映射（用变量映射，避免 else ifeq 兼容问题）
SLP_ARCH_MAP_aarch64:=arm64
SLP_ARCH_MAP_x86_64:=amd64
SLP_ARCH_MAP_mipsel:=mipsle
SLP_ARCH_MAP_mips:=mips
SLP_ARCH_MAP_arm:=arm64
SLP_ARCH:=$(SLP_ARCH_MAP_$(ARCH))

ifeq ($(SLP_ARCH),)
  $(error slp-client-bin: unsupported architecture: $(ARCH))
endif

# 用 TOPDIR 构建绝对路径（TOPDIR = SDK 根目录，由 rules.mk 设置）
SLP_BIN_FILE:=$(TOPDIR)/package/proxypool/slp-client-bin/files/slp-client-linux-$(SLP_ARCH)

include $(INCLUDE_DIR)/package.mk

define Package/slp-client-bin
  SECTION:=net
  CATEGORY:=Network
  TITLE:=SLP Client (pre-built binary)
endef

define Package/slp-client-bin/description
  SmartLink Protocol 客户端预编译二进制
endef

define Build/Prepare
	mkdir -p $(PKG_BUILD_DIR)
	$(CP) $(SLP_BIN_FILE) $(PKG_BUILD_DIR)/slp-client
	chmod +x $(PKG_BUILD_DIR)/slp-client
endef

define Build/Compile
endef

define Package/slp-client-bin/install
	$(INSTALL_DIR) $(1)/usr/bin
	$(INSTALL_BIN) $(PKG_BUILD_DIR)/slp-client $(1)/usr/bin/slp-client
endef

$(eval $(call BuildPackage,slp-client-bin))
