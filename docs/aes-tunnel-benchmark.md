# AES-GCM 与四种隧道承载的实测

2026-09-23，Windows/amd64，Intel Core i5-10400F（12 逻辑 CPU），Go 1.26.5，使用当前已优化的工作区代码。每个子基准按 500 ms 目标时间运行，顺序重复三次，以下报告中位数。MB/s 为十进制百万字节/秒，乘以 8 后是 Mbps。

## 加密算法与承载的关系

本项目的 TLS、WS、WSS、HTTP CONNECT 都包含一层 TLS 1.3 加密：

| 选项 | 数据封装次序（从内到外） |
|---|---|
| TLS | 隧道帧 → TLS → TCP |
| WS | 隧道帧 → TLS → WebSocket → TCP |
| WSS | 隧道帧 → WebSocket → TLS → TCP |
| HTTP | HTTP CONNECT 建连后，隧道帧 → TLS → TCP |

本次每条连接都通过证书验证并检查了实际协商结果：**TLS_AES_128_GCM_SHA256**。WebSocket 压缩未启用，HTTP CONNECT 不会为每个数据块重复发送 HTTP 请求。

AES 算法本身不会因为承载名称改变。承载影响的是记录和消息分片、掩码、复制、系统调用及调度成本。Go 的 TLS 1.3 不通过 `tls.Config.CipherSuites` 指定套件，本测试保留实际协商逻辑；下方 AES-256 的结果来自独立算法基准。

## AES-GCM 单线程加解密

使用 Go 标准库 AES-GCM，默认启用本机 AES-NI 等加速路径；包括 GCM 认证，解密包含标签校验。密钥生成和缓冲分配不计时，每次加密使用不同 nonce，复用输出缓冲。5 字节附加认证数据用于近似 TLS 的认证头部长度，并非完整 TLS 记录测试。

| 算法 | 数据块 | 加密 MB/s | 解密 MB/s |
|---|---|---:|---:|
| AES-128-GCM | 1 KiB | 3896.05 | 4393.86 |
| AES-128-GCM | 16 KiB | 5453.91 | 5895.10 |
| AES-128-GCM | 32 KiB | 5545.93 | 6033.43 |
| AES-256-GCM | 1 KiB | 3102.63 | 3457.79 |
| AES-256-GCM | 16 KiB | 4082.08 | 4324.95 |
| AES-256-GCM | 32 KiB | 4049.72 | 4373.08 |

32 KiB 数据块中，AES-128-GCM 加密约 5.91 μs，解密约 5.43 μs；AES-256-GCM 分别约 8.09 μs、7.49 μs。以上纯算法稳态循环均为 0 B/op、0 allocs/op。TLS 实际还要构造记录、维护序号和处理网络 I/O，因此不能将这些速率当作隧道吞吐。

## 四承载的实际加密传输

真实回环 TCP 连接，随机生成的 32 KiB 载荷，单逻辑流持续传输；测量前先传输 1 MiB，使 TLS 记录尺寸与缓冲进入稳态。上传、下载分别计时，收端读完全部载荷并发回确认后才停止计时，不以写入发送缓存完成为终点。

每个方向都包含发送端加密、接收端解密、隧道封装及本机 socket 开销。握手、Agent 计量落盘、配额/限速、控制面上报不计入；两端和目标均运行在同一台机器。

| 承载 | Mux | 上传 MB/s | 下载 MB/s |
|---|---|---:|---:|
| TLS | 关 | 887.38 | 625.12 |
| TLS | 开 | 752.21 | 491.09 |
| WS | 关 | 401.61 | 607.25 |
| WS | 开 | 335.98 | 476.36 |
| WSS | 关 | 454.05 | 632.87 |
| WSS | 开 | 412.45 | 490.28 |
| HTTP CONNECT | 关 | 979.18 | 640.23 |
| HTTP CONNECT | 开 | 724.07 | 446.25 |

该表是**包含 AES 加解密的端到端载荷吞吐**，不是给 AES 算法按协议分别测速。上传和下载中的 WebSocket 掩码方向、原始 TCP 读取分段与隧道帧生成位置不同，因此其速率差异不能直接解释为 AES 加密与解密的差异。

单流测试没有多连接复用的收益，开启 Mux 会增加帧和流控处理。此处的开/关比较，与之前优化前/后的比较属于不同问题。

本机回环有明显运行波动，例如 TLS 关闭 Mux 的上传三次为 690–1028 MB/s，HTTP 为 785–1092 MB/s。两者持续传输路径接近，不能依据两个中位数断言 HTTP 天生更快；WS/WSS 的结果也不能外推到所有带宽、RTT 和并发数。

## 软件实现对照

用 `-tags=purego` 单独构建基准，选择通用 Go AES/GCM 实现，仍在同一 CPU 上测量 32 KiB 数据块：

| 算法 | 加密 MB/s | 解密 MB/s |
|---|---:|---:|
| AES-128-GCM | 123.05 | 122.34 |
| AES-256-GCM | 103.86 | 104.76 |

本机 AES-128 加密的硬件路径约为通用实现的 45 倍。该对照说明硬件加速和实现选择的影响，但没有模拟低主频、虚拟机限额、其他架构或真实无 AES 指令 CPU 的完整性能。软件组只测试 AES-GCM 算法，没有将其用于四承载表；真实无 AES 加速环境可能协商 ChaCha20-Poly1305。

## 复现

基准代码：[`internal/tunnel/aes_benchmark_test.go`](../internal/tunnel/aes_benchmark_test.go)。

```sh
# 硬件默认路径：128/256 位，1/16/32 KiB，加密和解密
go test ./internal/tunnel -run '^$' -bench '^BenchmarkAESGCM$' -benchmem -benchtime=500ms -count=3

# 四承载，Mux 开/关，上传/下载；输出 AES-bits 验证实际协商
go test ./internal/tunnel -run '^$' -bench '^BenchmarkAESCarrierBulk$' -benchmem -benchtime=500ms -count=3

# 通用 Go 实现对照，仅测算法
go test -tags=purego ./internal/tunnel -run '^$' -bench '^BenchmarkAESGCM$/AES(128|256)/bytes=32768/' -benchmem -benchtime=500ms -count=3

# 正确性与竞态检查；这一轮的性能数字不用于报告
go test -race ./internal/tunnel -bench '^BenchmarkAES' -benchtime=2x -count=1
go vet ./internal/tunnel
```

若某台机器协商为非 AES-GCM，承载基准会明确跳过，不把其他算法的结果标为 AES。需要独立 CPU 测量时先运行算法基准，勿与承载测试或其他构建并行。

本机原始输出保存在 `.local/aes-gcm-hardware.txt`、`.local/aes-carriers-bulk.txt`、`.local/aes-gcm-purego.txt`。竞态检查、原有隧道回归和静态检查通过。测试没有测公网瓶颈、丢包或 Linux 双机容量。
