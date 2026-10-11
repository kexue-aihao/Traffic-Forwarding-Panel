# 转发连接沿用系统 TCP 拥塞算法

Agent 使用操作系统 TCP socket，不设置 `TCP_CONGESTION`，也不修改系统 sysctl。普通直连、安全直连、TLS/WS/WSS/HTTP 隧道及 Mux 的底层 TCP 连接沿用所在网络命名空间的默认拥塞算法；TLS 加密和隧道封装不会替换内核 TCP 算法。

如果机器已将 BBR 设置为默认算法，Agent 新建的 TCP 连接会使用 BBR，无需在面板额外启用。仅安装 BBR 模块或选择 `fq` 队列并不代表 BBR 已成为默认算法。入口和出口分别沿用各自机器的设置；容器需检查 Agent 所在网络命名空间，不能只检查宿主机。

在入口和出口机器分别检查：

```sh
sysctl net.ipv4.tcp_congestion_control
sysctl net.ipv4.tcp_available_congestion_control
sysctl net.core.default_qdisc
```

第一项应输出 `bbr`（或机器配置的 BBR 变体名称）。测速期间检查实际连接：

```sh
sudo ss -tinp
```

根据转发端口、对端地址和进程筛选 Agent 的连接，连接详情中应显示 `bbr`。系统默认值与现有连接使用的算法可能不同：修改默认值后，应重新建立转发连接；在默认值修改之前创建的监听 socket 及其接受的连接也可能保留旧值，必要时重启对应 Agent/出口服务。Mux 还会复用已有的底层承载连接。

BBR 控制该机器发送 TCP 数据时的拥塞行为。下载时需同时检查落地服务、出口和入口等实际发送端；它不能取消套餐限速、补足线路带宽或绕过应用层 Mux 窗口。UDP 和 QUIC 不使用内核 TCP BBR。

Linux 回归 `TestTCPInheritsSystemCongestionControl` 使用真实 IPv4/IPv6 socket 检查默认拨号、IPv6 优先拨号、监听及已接受连接的 `TCP_CONGESTION` 与内核默认值一致，不修改机器配置。该回归验证设置继承，不代表 BBR 公网性能验收。
