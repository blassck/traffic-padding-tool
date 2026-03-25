# Traffic Padding Tool (Optimized)

高性能网络流量填充工具，支持 WebSocket、令牌桶限流、热更新、Prometheus 监控等功能。

## 架构

```
┌─────────────┐      WebSocket      ┌─────────────┐
│   Client    │  ⟷  (双向长连接)   │   Server    │
│  (发送端)   │                   │  (监控/控制) │
│  令牌桶限流  │  ← 控制指令        │  流量采样    │
│  Buffer池   │  → 填充数据        │  Prometheus │
└─────────────┘                   └─────────────┘
```

## 主要优化

### 1. WebSocket 长连接
- 替代 HTTP POST，避免重复握手
- 双向通信：服务端推送控制指令，客户端发送填充数据
- 支持 mTLS 双向认证

### 2. 令牌桶限流 (Token Bucket)
- 平滑流量发送，避免突发
- 100Hz 高频采样，接近真实流量模式
- 支持 ±10% 抖动，更自然

### 3. Buffer 池 (sync.Pool)
- 复用 32KB 缓冲区
- 减少 GC 压力
- 提升高并发性能

### 4. 配置热更新
- 文件变更自动重载
- 无需重启服务
- 使用 fsnotify 监控

### 5. Prometheus 监控
```
# 指标列表
padding_bytes_total       # 总填充字节数（按客户端）
control_messages_total    # 控制消息数
clients_connected         # 当前连接客户端数
current_mode              # 当前模式 (0=idle, 1=proxy)
target_rate_bytes_per_second  # 目标填充速率
```

### 6. 跨平台支持
- Linux: /proc/net/dev
- Windows/macOS: 降级到 DummyCollector（可扩展）

### 7. 安全性
- mTLS 双向证书认证
- TLS 1.2+ 强制

## 构建

```bash
# Windows
build.bat

# Linux/macOS
cd server && GOOS=linux GOARCH=amd64 go build -o ../dist/pad-server .
cd client && GOOS=linux GOARCH=amd64 go build -o ../dist/pad-client .
```

## 配置 (config.toml)

```toml
[server]
port = 8443
uri = "/pad"
metrics = true          # 启用 Prometheus /metrics
log_level = "info"      # debug, info, warn, error

[network]
iface = "eth0"
sample_interval = 1.0   # 采样间隔（秒）
smooth_window = 1.5     # EWMA 平滑窗口

[proxy]
threshold = 100.0       # 流量阈值 KB/s
exit_time = 10          # 低于阈值后退出 proxy 模式的延迟
ratio = 2.0             # 目标出站/入站比例
maxspeed = 10000.0      # 最大填充速率 KB/s
minspeed = 100.0        # 最小填充速率 KB/s（可选）

[idle]
smin = 50               # 空闲模式最小速率 KB/s
smax = 200              # 空闲模式最大速率 KB/s
tmin = 5                # 发送阶段最小持续时间（秒）
tmax = 30               # 发送阶段最大持续时间（秒）
pmin = 10               # 暂停阶段最小持续时间（秒）
pmax = 60               # 暂停阶段最大持续时间（秒）

[auth]
enabled = false
# client_cert = "/path/to/ca.crt"  # mTLS CA 证书路径
```

## 使用

### 服务端

```bash
./pad-server -c config.toml
```

接口：
- `GET /` - 健康检查
- `GET /healthz` - 健康状态
- `GET /metrics` - Prometheus 指标
- `WS  /pad` - WebSocket 控制通道

### 客户端

```bash
# 基础用法
./pad-client -addr SERVER_IP:8443 -uri /pad -reconnect-min 5 -reconnect-max 10

# 带 mTLS
./pad-client -addr SERVER_IP:8443 -uri /pad -reconnect-min 5 -reconnect-max 10 -mtls
```

参数：
- `-addr` - 服务端地址
- `-uri` - WebSocket 路径
- `-reconnect-min/max` - 重连间隔范围（秒）
- `-mtls` - 启用双向 TLS（需 client.crt/client.key）

## 生成客户端证书 (mTLS)

```bash
# 生成 CA
openssl req -x509 -newkey rsa:4096 -keyout ca.key -out ca.crt -days 3650 -nodes

# 生成客户端证书
openssl req -newkey rsa:2048 -keyout client.key -out client.csr -nodes
openssl x509 -req -in client.csr -CA ca.crt -CAkey ca.key -CAcreateserial -out client.crt -days 365
```

## 监控

```bash
# 查看指标
curl -k https://localhost:8443/metrics

# 查看连接状态
curl -k https://localhost:8443/healthz
```

## 对比原版本

| 特性 | 原版 | 优化版 |
|-----|-----|-------|
| 协议 | HTTP POST | WebSocket |
| 流量平滑 | 100ms 突发 | 令牌桶 100Hz |
| 内存分配 | 每 tick 新分配 | sync.Pool 复用 |
| 配置更新 | 需重启 | 热重载 |
| 监控 | 日志 | Prometheus |
| 认证 | 无 | mTLS |
| 跨平台 | 仅 Linux | 接口化设计 |
