# Traffic-Forwarding-Panel

Go 控制面与独立 Agent，Vue 管理员后台 `/admin` 和用户前台 `/`。支持 SQLite、PostgreSQL、MySQL；提供规则配置、节点探针、钱包与套餐，以及 TLS、WS、WSS、HTTP 四种加密承载。

当前版本为 `v0.1.0-beta.2` 预发布版。安装包、更新范围及构建方式见 [发布说明](docs/releases/v0.1.0-beta.2.md)。已实现内容、实测记录和未完成项见 [实施状态](docs/implementation-status.md)；完整范围见 [实施计划](docs/implementation-plan.md)。Cyber 按用户要求跳过；租约续期连接连续性、容量/性能工作及 Linux/公网/真实支付验收等待后续指示。

## 本机启动

需要 Go 1.26；仓库已提交前端嵌入产物，构建 Go 程序无需 Node 或外网字体。目标部署平台是 Linux amd64/arm64，Windows 可用于开发验证。

```sh
go build -trimpath -o bin/panel ./cmd/panel
go build -trimpath -o bin/agent ./cmd/agent
./bin/panel -init-admin admin
```

初始化命令从标准输入读取一行至少 12、最多 72 字节的密码，完成后退出；已有数据不会被覆盖。不提供默认密码。再启动：

```sh
./bin/panel -addr 127.0.0.1:8080
```

访问 `http://127.0.0.1:8080/admin`。Windows 构建可使用 `-o bin/panel.exe`，执行 `./bin/panel.exe`。默认数据库 `data/panel.db`，面板进程有目录写权限即可。设备组和接入凭据由管理员创建；普通用户可由管理员创建，也可在站点设置启用开放或邀请注册（默认关闭）。用户规则需要有效套餐和有限配额。

账号恢复：在部署机器执行 `./bin/panel -reset-password 用户名`，从标准输入读取新密码；旧会话和 API Token 一并失效。无邮件找回功能。

## 部署与数据库

三库目前都使用单控制面实例。SQLite 使用本地 WAL；不要把数据库放在共享网络文件系统。服务端数据库通过 `TFP_DATABASE`、`TFP_DSN` 配置，启动自动迁移；例如 Linux：

```sh
export TFP_DATABASE=postgres
export TFP_DSN='postgres://panel:替换密码@127.0.0.1:5432/panel?sslmode=verify-full'
./bin/panel -origin https://panel.example.com
```

MySQL 使用 `TFP_DATABASE=mysql`，DSN 如 `panel:替换密码@tcp(127.0.0.1:3306)/panel?parseTime=true&tls=true`；数据库和权限由部署者先准备。部署配置使用受保护的服务环境文件。`-dsn` 可用于本机 SQLite 文件，含服务端凭据时优先环境变量。

HTTPS 反向代理应设置真实公开地址 `-origin https://panel.example.com` 并保留 Host；面板据此校验 Origin、设置 Secure Cookie。SSE `/api/v1/probes/events` 关闭代理缓冲并允许长连接。健康检查 `GET /api/v1/health` 会检查数据库。

### Docker 一键部署 / 1Panel

服务器已有 Docker 和 Docker Compose v2 时执行：

```sh
curl -fsSL https://github.com/kexue-aihao/Traffic-Forwarding-Panel/releases/download/v0.1.0-beta.2/install-docker.sh -o install-docker.sh && sudo bash install-docker.sh
```

输入 HTTPS 域名和管理员密码即可。脚本自动识别 amd64/arm64，下载并校验 Docker 镜像包，配置数据持久化、管理员、健康检查和容器重启。默认目录 `/opt/traffic-forwarding-panel`，仅监听宿主机 `127.0.0.1:18080`。

在 1Panel 新建反向代理网站，填写同一域名、代理地址 `http://127.0.0.1:18080`，申请证书并开启 HTTPS；启用 WebSocket，关闭代理缓存。管理员入口 `https://你的域名/admin`。详见 [Docker 与 1Panel 部署](docs/docker-deployment.md)，其中包括 OpenResty 使用桥接网络时的配置、离线安装、支付配置和备份方式。

镜像包含 `/panel` 与 `/agent`，默认以 UID/GID 65532 运行。此安装仅部署面板；用于承载转发流量的 Agent 仍部署到相应入口/出口节点。

## Agent、支付和接口

- [Agent 安装、出口白名单与四承载示例](examples/agent-README.md)：入口 Agent 注册后拉取配置，出口显式提供证书和凭据；WS/HTTP 内层同样使用 TLS，禁止证书验证降级。
- [支付配置示例](examples/payments.example.json)：复制到仓库外的受保护文件，填写商户资料，以 `-payments /path/payments.json -origin https://panel.example.com` 启动。示例占位值不能直接付款。
- [支付协议与固定版本](docs/payment/protocol-sources.md)、[支付实现边界](docs/payment/implementation-status.md)：已接入的渠道仍需分别验证真实商户；Cyber 已跳过。
- [API 契约](docs/api-contract.md)：浏览器使用 Cookie；自动化使用可撤销、到期的独立 API Token，目前权限为所有者资源。

钱包以人民币整数分记账；充值后再余额购买套餐。续费立即重置周期和配额，从购买成功时间增加上海自然月并夹紧月末。不会沿用旧到期时间，也不会在每月 1 日另送配额。

支持查单的已配置渠道会在后台自动核对待付款订单，失败按持久退避重试，面板重启继续；原版EPUSDT没有查单能力，依赖有效回调。前端所有日期固定上海时间。

## 备份恢复

```sh
./bin/panel -backup /secure/panel-backup.jsonl
./bin/panel -dsn data/restored.db -restore /secure/panel-backup.jsonl
```

备份是数据库一致性快照，目标文件必须不存在；恢复目标必须是空的已初始化数据库，工具会自动初始化表。恢复前停止面板，失败导入回滚，不覆盖已有业务记录。快照包含身份和账务数据，按数据库密钥同级保管。支付配置、隧道证书及每个 Agent 的状态/WAL 需另外备份。

恢复旧快照后，上线前核对支付已到账记录、Agent 未确认用量和配置版本；数据库快照本身不保证跨外部支付和节点状态的时间点一致性。RPO/RTO 与升级失败演练仍待完整验收。

## 开发与测试

```sh
go test ./...
go test -race ./...
go vet ./...
cd web
npm ci
npm run typecheck
npm run build
npx playwright install
npm test
npm run test:live
```

前端需 Node >=22.12，推荐 Node 24；运行时仅 Vue、vue-router 和本地 Inter。标准、构建资产要求与浏览器测试说明见 [前端开发](web/README.md)。live 测试编译独立面板并使用随机 SQLite 库，模拟 Agent 上报；真实转发由 Go 集成测试单独验证。

三库测试使用 `TFP_TEST_DRIVER=postgres|mysql` 和 `TFP_TEST_DSN` 后执行 `go test ./internal/commerce ./internal/platform ./internal/storage -count=1`。测试身份需要创建和删除数据库权限；每个测试创建随机隔离库，不清空指定 DSN 的现有数据。默认测试使用临时 SQLite。

500 节点/1 万用户/10 万规则热路径冒烟基准：`go test ./internal/platform -run '^$' -bench '^BenchmarkUsageCapacity$' -benchtime=500x -count=1`。这不是 2 小时/24 小时混合负载或网络吞吐的容量结论。

当前接口的机器可读文档：运行后访问 `/api/v1/openapi.json`，源文件见 [OpenAPI 3.1](internal/openapi/openapi.json)。套餐限制、状态告警、故障转移、反向隧道、Mux、TLS 共享、终端和升级的边界见 [实施状态](docs/implementation-status.md)。Cyber支付按用户要求跳过；Linux 跨机、公网和真实商户仍需外部验收。
