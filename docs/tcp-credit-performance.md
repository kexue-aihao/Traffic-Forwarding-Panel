# TCP 批量额度与测速测试包

本次优化让 TCP 转发在已持久化的额度窗口内直接扣减内存用量，后台批量写账本。普通直连、安全直连、入口到出口、多级出口和共享 TLS 入站选中规则后的 TCP 计量使用同一条路径。入口 Agent 自动启用，无需修改规则或升级面板。

原流程是每读取最多 32 KiB 业务数据，先提交用量记录并等待 WAL 的 `fsync`，成功后才发送。单流无法充分利用并发组提交；每块数据若等待 3 ms，单计量环节就可能把吞吐限制到约 87 Mbps。该换算解释瓶颈量级，不代表已经测得用户服务器的磁盘延迟。

现在的流程如下：

1. 首次需要转发时，按规则、账户、租约和上传/下载方向申请窗口；WAL 同步成功后才发布可用额度。
2. 每窗口最多 256 KiB，每方向最多两个窗口。窗口内的 TCP 扣量和状态变化通知查询都不获取磁盘状态锁。
3. 剩余可用额度低于 256 KiB 时异步补充；TCP 额度不足时暂停发送并等待补充、确认账本空间或新租约，不丢弃已经读取的数据。等待可以取消，沿用 30 秒续租等待上限；队列满时每 100 ms 重试。
4. writer 在补充窗口和每 100 ms 的定时刷新中批量结算进度，向面板同步前也会刷新。临时离线时沿用最多 4096 个待确认记录与恢复窗口的总空间限制。
5. TCP 不在加载配置时提前预占额度；空闲约一秒的窗口在 writer 下次处理时结算并释放，避免空闲规则占满同账户的预留空间。

每规则未确认的原始字节责任最多 1 MiB，每账户每 Agent 最多 8 MiB，与原 UDP 窗口一致。窗口实际扣量由内存锁保护，同规则多连接和两个方向不能重复使用同一额度。套餐速率、连接数、IP 数、流量倍率和租约约束继续生效。

正常关闭、退租、规则撤销及目的地址/资源限制变化时，冻结相关窗口、结算已经准许转发的用量并释放未用额度。进程崩溃时，无法证明未用的窗口余量按原租约、规则、方向和时间区间生成独立 `recovery` 用量，避免重启后重复消费预算；同一窗口的恢复记录不会重复记账。因此异常退出可能保守多计，单次恢复受上述责任上限约束；反复异常退出的多计可以累积。计量仍在发送前发生，网络写入失败后的计费语义沿用原实现。

WAL、checkpoint 及 UsageRecord 保持 v0.1.41 的格式 2，不增加面板契约字段。UDP 仍按原配置选择信用窗口或同步计量，额度不足时保留丢包语义。

## 验证结果

2026-10-10，Windows amd64、Intel i5-10400F、Go 1.26.9。本机真实回环持续单流下载，安全直连使用 TLS 和 `random-padding`，32 KiB 业务块，512 次、两轮；预热 16 块。

| 同一诊断用例 | 改动前两轮 | 改动后两轮 |
| --- | --- | --- |
| 安全直连 + Runtime/Store 计量，MB/s | 43.33 / 52.37 | 201.12 / 177.62 |
| 每 32 KiB 的 WAL 同步次数 | 1 / 1 | 0.2520 / 0.2520 |
| 每次 fsync 平均耗时，µs | 650.6 / 531.6 | 572.1 / 664.1 |

两轮均值约提升 3.96 倍。此次未计量的安全直连对照为 392.65 / 360.44 MB/s，说明 TLS/混淆及窗口补充仍有开销。本例运行实际 Runtime 和 Store，但没有运行与远程面板持续同步的 Agent 控制循环；结果不能作为公网带宽或 Linux 性能保证。

通过 `go test ./... -count=1 -timeout=5m`、`go vet ./...`，以及 Agent、tunnel、integration 的 race 检查。覆盖窗口内扣量不等待 writer、未落盘额度拒绝发送、磁盘失败、取消、64 个双向并发扣量、账户责任上限、空闲回收、10 字节小预算方向再分配、正常关闭、崩溃恢复幂等、租约切换和共享 TLS 规则隔离。测试中的账务断言先刷新窗口再核对精确业务用量。

## 更新入口 Agent 测速

本地测试包位于 `.local/tcp-credit-test-20261010/`，版本标识为 `0.1.41-tcp-credit-test`。提供 Linux amd64、arm64 包和 `SHA256SUMS`。二进制以 `CGO_ENABLED=0` 构建；已核对 ELF 架构和 Go 构建信息，尚未在用户 Linux 节点运行。

先根据节点架构选择包：`x86_64` 使用 amd64，`aarch64` 使用 arm64。把包及 `SHA256SUMS` 传至入口机器，在该目录核验所选包，例如：

```sh
sha256sum tfp-agent_0.1.41-tcp-credit-test_linux_amd64.tar.gz
# 与 SHA256SUMS 中对应包的摘要比较。
tar -xzf tfp-agent_0.1.41-tcp-credit-test_linux_amd64.tar.gz
cd tfp-agent_0.1.41-tcp-credit-test_linux_amd64
./tfp-agent -version
# 应输出 0.1.41-tcp-credit-test
```

使用仓库标准 systemd 安装路径的入口节点，可用以下命令替换。保留原状态文件和服务参数，入口重新连接后继续使用原规则。

```sh
sudo systemctl stop tfp-agent
sudo cp -p -n /usr/local/bin/tfp-agent /usr/local/bin/tfp-agent.before-tcp-credit
sudo install -m 0755 ./tfp-agent /usr/local/bin/tfp-agent.next
sudo mv -f /usr/local/bin/tfp-agent.next /usr/local/bin/tfp-agent
sudo systemctl start tfp-agent
/usr/local/bin/tfp-agent -version
sudo systemctl is-active tfp-agent
sudo journalctl -u tfp-agent -n 30 --no-pager
```

先升级负责入口计量的注册 Agent 即可测试本次优化，安全直连出口保持原协议也能对接。若使用自定义安装路径，以 `systemctl cat tfp-agent` 的实际 `ExecStart` 为准。

用原测速客户端、相同规则和测速目标，分别测单连接、多连接与直接访问目标，保留上下行速率、入口 CPU 占用及 Agent 日志。第一次转发仍需一次持久化预留，测速应持续至少 30 秒并重复三轮。明确记录当前混淆策略；若开启 `timing-perturb`，其主动等待仍会影响速度，不能仅凭本次优化判断链路带宽。

需要退回原 v0.1.41 时，正常停止服务后用已备份二进制原子替换并重新启动；两版共享 WAL 2 格式，可继续使用当前状态。不要恢复已经被继续消费的旧账本或删除状态文件。

```sh
sudo systemctl stop tfp-agent
sudo install -m 0755 /usr/local/bin/tfp-agent.before-tcp-credit /usr/local/bin/tfp-agent.next
sudo mv -f /usr/local/bin/tfp-agent.next /usr/local/bin/tfp-agent
sudo systemctl start tfp-agent
```
