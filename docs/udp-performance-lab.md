# UDP 隔离验收操作手册

本手册执行 [计划书](udp-performance-plan.md) 的测量方法，当前没有跨机实测结果。使用独立负载机 L、入口 I、出口 E、目标 T，或预先验证不会争用 CPU/网络的隔离拓扑。只对已明确授权的实验机器/服务执行；不要把示例目标改成生产流量。

## 环境冻结

记录每台机器的角色、CPU 型号/配额/亲和性、内存、OS/内核、Go 1.26.9、quic-go 0.63.0、commit 或工作区 diff 哈希、NIC/速率/MTU、socket/sysctl 实际值、存储和同步延迟。记录各链路 RTT、CPU 背景占用、NUMA、IRQ、虚拟化与加密硬件支持。压测时其他自动测试停止运行。

每轮启动前保存 `uname -a`、`lscpu`、`ip -d link`、`sysctl net.core.rmem_max net.core.wmem_max`、`nstat -az`、`ss -u -m`、`ethtool -S <NIC>`、磁盘和进程资源数据。没有权限时标明缺项。使用已有监测工具采集进程 CPU/RSS/FD/线程/GC、磁盘同步和网卡/socket 丢弃，基线与产品使用相同配额。Agent 的诊断计数用来解释丢弃，不能替代进程 profiler 或接收端统计。

## 构建

```sh
go build -trimpath -o udpbench ./cmd/udpbench
go build -trimpath -o tfp-agent ./cmd/agent
go build -trimpath -o tfp-panel ./cmd/panel
```

保留二进制 SHA-256 和源代码快照。V37 从 v0.1.37 的独立 checkout 编译，复用本次 udpbench 负载机/目标；禁止拿关闭计量的新 Agent 代替 V37。

以下地址仅为说明，执行前用实验机地址替换。示例：L=192.0.2.10、I=192.0.2.20、E=192.0.2.30、T=192.0.2.40。服务器 foreground 运行，通过 Ctrl-C 停止；各测量轮次避免并行互相干扰。

## B0 / B1

目标 T：

```sh
./udpbench -mode echo -listen 0.0.0.0:9001
```

B0 负载机直接测目标，先确认链路和生成器容量：

```sh
./udpbench -mode load -target 192.0.2.40:9001 -direction echo \
  -size 1200 -sessions 32 -pps 10000 -warmup 30s -duration 120s -output B0.json
```

直连 B1 在 I 运行无计量普通 UDP 代理：

```sh
./udpbench -mode relay -listen 0.0.0.0:9000 -target 192.0.2.40:9001
```

单出口裸 UDP B1 在 E 增加一个 relay 到 T；I 的 relay 指向 E 的 relay。负载机始终向 I 测量；与产品保持同跳数/路由。

## B2

B2 使用相同 QUIC 库、加密与业务帧格式的独立代理，不调用产品计量/规则策略。因此它能分离产品额外开销，但共享协议库的缺陷仍可能影响 B2 与产品，必须同时公布裸 UDP B1 的比值。入口基线建连发生在预热期，不把建连吞吐当稳定容量。

出口 E 使用实验 CA 签发且 SAN 包含 `exit.lab.example` 的证书，token 通过环境注入：

```sh
export TFP_EXIT_TOKEN='<实验专用的16至128字符token>'
./udpbench -mode quic-relay -listen 0.0.0.0:9443 -cert exit.crt -key exit.key
```

入口 I：

```sh
export TFP_EXIT_TOKEN='<同一个实验token>'
./udpbench -mode quic-entry -listen 0.0.0.0:9000 -target 192.0.2.30:9443 \
  -server-name exit.lab.example -ca lab-ca.crt -forward 192.0.2.40:9001
```

负载机 L 向 I 测量。不得关闭证书验证；抓包确认 I→E 使用 UDP、业务数据为 QUIC DATAGRAM，可靠 stream 只承载会话控制。

## 产品与三种方向

隔离面板配置真实有限套餐，并让 I/E 使用本次 Agent；按 [实现说明](udp-performance-implementation.md) 配置直连/单出口，目标仍为 T 的 `udpbench -mode echo`。产品监听端口设为 9000，避免同时启动同端口基线。每方向使用相同参数测 B1/B2/产品/V37。

```sh
# upload：1200 B 上传，目标校验后回 32 B ACK
./udpbench -mode load -target 192.0.2.20:9000 -direction upload \
  -size 1200 -sessions 32 -pps 10000 -output product-upload.json
# download：32 B 请求，目标回 1200 B 并带校验；目标须用本次 udpbench
./udpbench -mode load -target 192.0.2.20:9000 -direction download \
  -size 1200 -sessions 32 -pps 10000 -output product-download.json
# echo：1200 B 回显，相关的对称双向负载
./udpbench -mode load -target 192.0.2.20:9000 -direction echo \
  -size 1200 -sessions 32 -pps 10000 -output product-echo.json
```

方向中的少量控制/ACK 字节仍真实经过产品并计量，但 bulk Mbps 不把它们当目标方向有效载荷。每个 RTT 都是请求/应答，禁止称为单向延迟。echo 的 `verified_payload_mbps` 是每方向的业务速率；不能将这个数与上下行合计计量混用。

0 B 包另做边界测试，工具测量元数据包含在完整业务载荷长度中，负载模式长度至少 32 B。报文近上限可用 `-size 60000`，只验证边界/重组；IPv6 用括号地址并测试真实 IPv6 链路。

先每个方向/载荷/会话组合逐步增加 `-pps`，找到连续采样应用丢包 ≤0.1%、资源不超限的最大稳定接收速率。发送器必须达到目标 offered PPS；达不到则先排查负载机，不能把发送器上限当代理容量。负载生成器采用 Go socket 和 pacing，高 PPS 也必须验证生成器 CPU/丢包。默认 30 s 预热、120 s 采样、2 s drain，重复 5 次报告中位数及范围。

矩阵：64/256/512/1200 B、1/32/256/1024 会话、1/32/128 规则、IPv4/IPv6、两架构、上传/下载/双向。多规则可将总负载均分到多个监听端口并运行分开的生成器进程，最后按时间区间对齐聚合；为每个进程记录负载机 CPU，避免把多进程竞争混入结论。

低/中负载以比较路径中较慢者最大稳定速率的 10%/70% 测附加 P99 RTT。受控 RTT 1/20/80 ms、丢包 0/0.1%/1%、乱序、MTU 收缩、UDP 封锁和出口重启在隔离网络分别测试，故障场景不与无损性能门槛混算。

## 持续租约、计量与长稳

至少一轮用足够大但有限的真实套餐连续跑多次租约补充、用量上报和退租，保留面板结算/审计事实、Agent 状态与诊断快照。全路径的实际计量还包含失败发送尝试、ACK/请求和正常双向业务字节，应该按这些定义对账，不能只拿负载工具单方向 Mbps 计算账单。

正常退出验证最终放行计数和未用预留释放；异常终止后验证恢复记录独立分类、仍属原权益周期、重复上报不重复扣费，单规则/账号责任不超过约定上限。还需慢磁盘、满磁盘、ACK 丢失、配置撤销、新周期购买和有限控制面失联场景。

同条件 24 h 可用 `-duration 24h`，以预热后固定间隔记录内存/FD/goroutine/GC/队列、活动会话和计量；不要只比较最后一次 heap 数。工具序号去重窗口为每会话 65536 包，极端迟到计 late/lost；RTT 直方图精度 10 μs，超过 1 s 的值计溢出并截在 1 s，溢出较多时不能用 P99 截断值声称满足延迟门槛。

PostgreSQL/MySQL 在实验 DB 实例上用仓库 `TFP_TEST_DRIVER`、`TFP_TEST_DSN` 运行同一 suite；测试身份需允许创建/删除随机实验数据库，避免将生产数据库提供给测试。Linux amd64/arm64 分别实际执行 suite/race 和数据面矩阵；交叉编译不代替运行。

## 报告模板

| 场景/方向/载荷/会话 | B0 Mbps/PPS | B1 Mbps/PPS | B2 Mbps/PPS | V37 Mbps/PPS | 产品 Mbps/PPS | 产品/B1 | 产品/B2 | 丢包/重复/乱序/损坏 | P50/P95/P99 RTT | CPU/RSS/FD/GC/同步/丢弃 | 门槛结果 |
|---|---|---|---|---|---|---|---|---|---|---|---|
| 待测 | — | — | — | — | — | — | — | — | — | — | 待测 |

报告附原始 JSON、环境快照、五轮范围、资源采样、抓包承载证据、计量对账、故障结果及 24 h 趋势图。没有数据填“待测”，不填估算值或 Windows 回环推算。验收结果逐项对照计划 3.2，任一强制项不通过就定位并重测。
