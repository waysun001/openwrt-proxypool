# SmartLink Protocol (SLP) — 完整架构文档

## 一、总体架构

```
┌─────────────────────────────────────────────────────────────────┐
│                        局域网设备                                │
│  手机/电脑/TV  →  连接路由器WiFi  →  自动按IP分流                 │
└──────────────────────────┬──────────────────────────────────────┘
                           │
                    ┌──────▼──────┐
                    │   OpenWrt    │
                    │  路由器      │
                    │             │
                    │ ┌─────────┐ │
                    │ │proxypool│ │  IP分流规则（已有）
                    │ └────┬────┘ │
                    │      │      │
                    │ ┌────▼────┐ │
                    │ │SLP Client│ │  多条隧道，每条一个SOCKS5端口
                    │ │:1081    │ │  ← 隧道1（美国）
                    │ │:1082    │ │  ← 隧道2（日本）
                    │ │:1083    │ │  ← 隧道3（香港）
                    │ └────┬────┘ │
                    └──────┼──────┘
                           │ QUIC/WebSocket/KCP（加密隧道）
                    ┌──────▼──────────────────────────┐
                    │          互联网（加密传输）        │
                    └──┬──────────┬──────────┬────────┘
                       │          │          │
                 ┌─────▼───┐┌────▼────┐┌────▼────┐
                 │ SLP 服务端││ SLP 服务端││ SLP 服务端│
                 │ 美国 VPS ││ 日本 VPS ││ 香港 VPS │
                 │ 出口IP-A ││ 出口IP-B ││ 出口IP-C │
                 └─────────┘└─────────┘└─────────┘
```

## 二、协议设计

### 2.1 传输层（三种模式）

```
模式1: QUIC（默认，推荐）
  - 基于 UDP:443
  - 内建 TLS 1.3 加密
  - 0-RTT 快速重连
  - 多路复用、抗丢包
  - 外部看起来像 HTTP/3 流量

模式2: WebSocket + TLS（防墙备用）
  - TCP:443，标准 HTTPS
  - 可过 CDN（Cloudflare/CloudFront）
  - 外部看起来像普通网页访问
  - 适合 UDP 被封的网络

模式3: KCP + FEC（极端弱网）
  - UDP 自定义端口
  - 前向纠错（20%冗余抗20%丢包）
  - 以带宽换延迟
  - 适合移动网络/高丢包环境
```

### 2.2 协议帧格式

```
所有数据在加密层之上，外部无法识别内容

认证帧（连接建立后第一帧）:
┌──────────┬──────────┬──────────┬──────────────┐
│ Version  │ AuthType │ TokenLen │ Token        │
│ 1 byte   │ 1 byte   │ 2 bytes  │ 变长         │
│ 0x01     │ 0x01=tkn │          │ UTF-8 字符串  │
└──────────┴──────────┴──────────┴──────────────┘

数据帧:
┌──────────┬──────────┬──────────┬──────────┬──────────┬─────────┐
│ FrameType│ AddrType │ AddrLen  │ Address  │ Port     │ Payload │
│ 1 byte   │ 1 byte   │ 1 byte   │ 变长     │ 2 bytes  │ 变长    │
│ 0x01=TCP │ 0x01=IPv4│          │          │ BigEnd   │         │
│ 0x02=UDP │ 0x03=域名│          │          │          │         │
└──────────┴──────────┴──────────┴──────────┴──────────┴─────────┘

控制帧:
┌──────────┬──────────┐
│ FrameType│ Padding  │
│ 0xFE=心跳│ 随机填充  │  ← 伪装成正常数据，防流量分析
│ 0xFF=关闭│          │
└──────────┴──────────┘
```

### 2.3 加密方案

```
握手:
  QUIC 模式  → TLS 1.3（内建）
  WS 模式    → TLS 1.3（外层）+ ChaCha20（内层）
  KCP 模式   → ChaCha20-Poly1305（手动加密每个包）

数据加密:
  算法: ChaCha20-Poly1305（AEAD）
  密钥派生: HKDF-SHA256(token, random_salt)
  每连接独立密钥，前向安全

TLS指纹伪装（utls库）:
  模拟 Chrome 120 的 ClientHello
  随机化扩展顺序
  正常的 ALPN: h2, http/1.1
```

### 2.4 防检测策略

```
1. 流量伪装
   - TLS 指纹模拟浏览器（utls）
   - 随机 padding 消除包长度特征
   - 心跳包伪装成正常数据包

2. 协议无特征
   - 无固定魔数
   - 无固定头部模式
   - 加密后无法区分数据类型

3. CDN 中转（可选）
   - WebSocket 模式可过 Cloudflare
   - SNI 显示正常域名
   - 流量混在 CDN 大流量中
```

## 三、服务端设计

### 3.1 目录结构

```
slp-server/
├── cmd/
│   └── slp-server/
│       └── main.go              # 入口
├── internal/
│   ├── server/
│   │   ├── server.go            # 主服务器
│   │   ├── quic.go              # QUIC 监听器
│   │   ├── websocket.go         # WebSocket 监听器
│   │   └── kcp.go               # KCP 监听器
│   ├── auth/
│   │   ├── auth.go              # 认证接口
│   │   └── token.go             # Token 认证
│   ├── proxy/
│   │   ├── proxy.go             # 代理转发核心
│   │   ├── tcp.go               # TCP 转发
│   │   └── udp.go               # UDP 转发
│   ├── crypto/
│   │   ├── cipher.go            # ChaCha20-Poly1305
│   │   └── tls.go               # TLS 配置
│   └── stats/
│       └── stats.go             # 流量统计
├── config.yaml                  # 配置文件
├── Dockerfile
├── docker-compose.yml
├── Makefile
├── go.mod
└── go.sum
```

### 3.2 配置文件

```yaml
# config.yaml
server:
  name: "us-west-1"

listen:
  quic:
    enabled: true
    addr: ":443"
  websocket:
    enabled: true
    addr: ":8443"
    path: "/ws"           # 伪装成普通WebSocket路径
  kcp:
    enabled: false
    addr: ":4000"
    fec_data: 10          # FEC: 10个数据包
    fec_parity: 3         # FEC: 3个校验包

tls:
  cert: "/etc/slp/fullchain.pem"
  key: "/etc/slp/privkey.pem"
  sni: "cdn.example.com"

auth:
  tokens:
    - name: "router-01"
      token: "xxxx-xxxx-xxxx"
      bandwidth: 0        # 0=无限 (Mbps)
    - name: "router-02"
      token: "yyyy-yyyy-yyyy"
      bandwidth: 100

log:
  level: "info"           # debug/info/warn/error
  file: "/var/log/slp-server.log"

stats:
  enabled: true
  api_addr: "127.0.0.1:9090"   # 内部统计API
```

### 3.3 核心流程

```
1. 启动监听（QUIC + WebSocket + KCP）
2. 接受连接 → TLS 握手
3. 读取认证帧 → 验证 Token
4. 循环读取数据帧:
   a. 解析目标地址和端口
   b. 建立到目标的连接（连接池复用）
   c. 双向转发数据
5. 心跳检测 → 超时断开
6. 记录流量统计
```

## 四、客户端设计

### 4.1 目录结构

```
slp-client/
├── cmd/
│   └── slp-client/
│       └── main.go              # 入口
├── internal/
│   ├── client/
│   │   ├── client.go            # 主客户端
│   │   ├── quic.go              # QUIC 连接
│   │   ├── websocket.go         # WebSocket 连接
│   │   └── kcp.go               # KCP 连接
│   ├── tunnel/
│   │   ├── tunnel.go            # 隧道管理
│   │   ├── pool.go              # 连接池
│   │   └── reconnect.go         # 自动重连
│   ├── socks5/
│   │   └── socks5.go            # 本地SOCKS5服务
│   ├── crypto/
│   │   └── cipher.go            # 加密（同服务端）
│   └── health/
│       └── heartbeat.go         # 心跳保活
├── Makefile                     # 交叉编译 OpenWrt
├── go.mod
└── go.sum
```

### 4.2 OpenWrt 编译

```makefile
# Makefile — OpenWrt 交叉编译

# GL-MT6000 = MediaTek MT7986 = ARM64
GOOS=linux
GOARCH=arm64

# 编译优化（减小体积）
LDFLAGS=-s -w

build:
	CGO_ENABLED=0 GOOS=$(GOOS) GOARCH=$(GOARCH) \
	go build -ldflags "$(LDFLAGS)" -o slp-client ./cmd/slp-client/

# UPX 压缩（可选，约减小 60%）
compress: build
	upx --best slp-client

# 预计体积：~5-8MB（压缩后 ~2-3MB）
```

### 4.3 UCI 配置格式

```
# /etc/config/slp

config slp 'global'
    option enabled '1'
    option log_level 'info'

config tunnel 'us1'
    option enabled '1'
    option name '美国节点1'
    option server '1.2.3.4'
    option port '443'
    option transport 'quic'
    option token 'xxxx-xxxx-xxxx'
    option socks5_port '1081'
    option keepalive '15'
    option pool_size '2'
    option fec '0'

config tunnel 'jp1'
    option enabled '1'
    option name '日本节点1'
    option server '5.6.7.8'
    option port '443'
    option transport 'websocket'
    option ws_path '/ws'
    option token 'yyyy-yyyy-yyyy'
    option socks5_port '1082'
    option keepalive '15'
    option pool_size '2'

config tunnel 'hk1'
    option enabled '1'
    option name '香港节点1'
    option server '9.10.11.12'
    option port '4000'
    option transport 'kcp'
    option token 'zzzz-zzzz-zzzz'
    option socks5_port '1083'
    option keepalive '10'
    option fec '20'
```

### 4.4 客户端核心流程

```
1. 读取 UCI 配置
2. 为每个 tunnel 启动:
   a. 连接服务端（QUIC/WS/KCP）
   b. 发送认证帧
   c. 启动本地 SOCKS5 监听（:1081, :1082...）
   d. 启动心跳 goroutine
   e. 启动连接池维护
3. 接受本地 SOCKS5 请求:
   a. 解析目标地址
   b. 通过隧道发送数据帧
   c. 双向转发
4. 异常处理:
   a. 连接断开 → 自动重连（指数退避）
   b. 多次失败 → 切换传输模式
   c. 所有模式失败 → 告警
```

## 五、OpenWrt 集成

### 5.1 新增文件

```
openwrt-proxypool/
├── slp-client/                       # 新增：SLP客户端包
│   ├── Makefile                      # OpenWrt 包定义
│   └── files/
│       ├── slp-client                # 预编译二进制
│       ├── slp.config                # 默认 UCI 配置
│       ├── slp.init                  # init.d 启动脚本
│       └── slp-manager.sh           # 隧道管理（类似 l2tp-manager.sh）
│
├── luci-app-proxypool/
│   └── luasrc/
│       └── view/proxypool/
│           └── main.htm              # 修改：新增 SLP 类型
│
└── proxypool-core/
    └── files/
        └── proxypool.sh             # 修改：支持 SLP 类型分流
```

### 5.2 proxypool 集成

```
现有类型: l2tp, socks5
新增类型: slp

SLP 在 proxypool 中的表现:
- 每条隧道 = 一个本地 SOCKS5 端口
- 和现有 SOCKS5 类型完全兼容
- 防火墙规则不用改（基于 SOCKS5 端口分流）
```

### 5.3 LuCI 界面新增

```
客户端类型下拉:
  [L2TP ▼]  →  [L2TP | SOCKS5 | SLP ▼]

SLP 专属字段:
  ┌────────────────────────────────────────┐
  │ 类型: [SLP ▼]                          │
  │ 服务器: [1.2.3.4        ]              │
  │ 端口:   [443            ]              │
  │ 传输:   [QUIC ▼]  (QUIC/WebSocket/KCP) │
  │ Token:  [xxxx-xxxx-xxxx ]              │
  │ 本地端口:[1081          ]  (自动分配)   │
  │ FEC:    [0   ] %  (仅KCP模式)          │
  │ 连接池: [2   ]                         │
  └────────────────────────────────────────┘
```

## 六、开发顺序

```
Phase 1: 基础通信（3天）
├── Day 1: 项目骨架 + QUIC 服务端监听 + 认证
├── Day 2: QUIC 客户端连接 + 数据帧收发 + TCP代理转发
└── Day 3: 本地 SOCKS5 接口 + 端到端测试（curl --socks5 测试）

Phase 2: 稳定性（3天）
├── Day 4: 心跳保活 + 自动重连 + 连接池
├── Day 5: WebSocket 传输模式
└── Day 6: KCP + FEC 传输模式

Phase 3: 防检测（2天）
├── Day 7: TLS 指纹伪装(utls) + 随机 padding
└── Day 8: 流量混淆 + CDN 支持测试

Phase 4: OpenWrt 集成（3天）
├── Day 9:  交叉编译 + OpenWrt 包 + UCI 配置
├── Day 10: slp-manager.sh + init.d 脚本
└── Day 11: LuCI 界面集成 + proxypool 分流支持

Phase 5: 测试上线（2天）
├── Day 12: VPS 部署服务端 + 路由器刷机测试
└── Day 13: 弱网模拟测试 + 性能调优 + 文档
```

## 七、Go 依赖库

```go
// go.mod 主要依赖
require (
    github.com/quic-go/quic-go    v0.42.0   // QUIC 传输
    github.com/gorilla/websocket   v1.5.1    // WebSocket 传输
    github.com/xtaci/kcp-go/v5     v5.6.8    // KCP 传输
    github.com/refraction-networking/utls v1.6.3  // TLS 指纹伪装
    github.com/things-go/go-socks5 v0.0.5    // SOCKS5 服务端
    golang.org/x/crypto            v0.21.0   // ChaCha20-Poly1305
    gopkg.in/yaml.v3               v3.0.1    // 配置解析
)
```

## 八、部署清单

### 服务端（每台VPS）
```bash
# 1. 安装
wget https://github.com/yourrepo/slp-server/releases/latest/slp-server-linux-amd64
chmod +x slp-server-linux-amd64
mv slp-server-linux-amd64 /usr/local/bin/slp-server

# 2. 申请证书（用于TLS）
apt install certbot
certbot certonly --standalone -d proxy.example.com

# 3. 配置
mkdir -p /etc/slp
cat > /etc/slp/config.yaml << 'EOF'
server:
  name: "us-west-1"
listen:
  quic:
    enabled: true
    addr: ":443"
auth:
  tokens:
    - name: "router-01"
      token: "your-secret-token"
tls:
  cert: "/etc/letsencrypt/live/proxy.example.com/fullchain.pem"
  key: "/etc/letsencrypt/live/proxy.example.com/privkey.pem"
EOF

# 4. systemd 服务
cat > /etc/systemd/system/slp-server.service << 'EOF'
[Unit]
Description=SmartLink Protocol Server
After=network.target

[Service]
ExecStart=/usr/local/bin/slp-server -c /etc/slp/config.yaml
Restart=always
RestartSec=5

[Install]
WantedBy=multi-user.target
EOF

systemctl enable --now slp-server
```

### 客户端（OpenWrt路由器）
```bash
# 刷入包含 slp-client 的固件后自动可用
# 或手动安装 ipk:
opkg install slp-client_1.0.0_aarch64.ipk

# UCI 配置
uci set slp.global.enabled='1'
uci set slp.us1=tunnel
uci set slp.us1.server='1.2.3.4'
uci set slp.us1.port='443'
uci set slp.us1.transport='quic'
uci set slp.us1.token='your-secret-token'
uci set slp.us1.socks5_port='1081'
uci commit slp

/etc/init.d/slp restart
```

## 九、性能目标

```
指标                目标值
─────────────────────────────
吞吐量（好网）      > 90% 线路带宽
吞吐量（5%丢包）    > 80% 线路带宽
延迟增加            < 5ms
重连时间            < 2s（QUIC 0-RTT）
内存占用            < 15MB（路由器端）
二进制体积          < 5MB（UPX压缩后）
CPU（路由器）       < 10%（MT7986 跑满带宽时）
```

---

准备好了就从 Phase 1 Day 1 开始！
