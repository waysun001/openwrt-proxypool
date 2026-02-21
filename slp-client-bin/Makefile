include $(TOPDIR)/rules.mk

PKG_NAME:=slp-client-bin
PKG_VERSION:=1.0.0
PKG_RELEASE:=1

PKG_MAINTAINER:=智联盒子
PKG_LICENSE:=MIT

# OpenWrt ARCH -> Go ARCH 映射
ifeq ($(ARCH),aarch64)
  SLP_ARCH:=arm64
else ifeq ($(ARCH),x86_64)
  SLP_ARCH:=amd64
else ifeq ($(ARCH),mipsel)
  SLP_ARCH:=mipsle
else ifeq ($(ARCH),mips)
  SLP_ARCH:=mips
else ifeq ($(ARCH),arm)
  SLP_ARCH:=arm64
else
  $(error slp-client-bin: unsupported architecture $(ARCH))
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

define Build/Prepare
	mkdir -p $(PKG_BUILD_DIR)
	$(CP) ./files/slp-client-linux-$(SLP_ARCH) $(PKG_BUILD_DIR)/slp-client
	chmod +x $(PKG_BUILD_DIR)/slp-client
endef

define Build/Compile
endef

define Package/slp-client-bin/install
	$(INSTALL_DIR) $(1)/usr/bin
	$(INSTALL_BIN) $(PKG_BUILD_DIR)/slp-client $(1)/usr/bin/slp-client
endef

$(eval $(call BuildPackage,slp-client-bin))
