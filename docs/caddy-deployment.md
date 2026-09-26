# YAML 配置与 Caddy 建站

面板支持用 YAML 配置启动，再由 Caddy 提供域名、HTTPS 和反向代理。配置方式参考 [Nyanpass 安装面板文档](https://nyanpass.pages.dev/intro/install_backend/)，路由按本项目适配：除 `/api/*` 外，还必须转发 `/online/*` 和 `/download/*`。管理员入口是 `/admin`，用户入口是 `/`。

本功能需要从当前源码构建；已发布的旧版 v0.1.2 二进制不包含新增的 `-config`、`-check-config`、`-export-html` 参数。v0.1.3 起的发布包包含这些参数。下面的 Docker 示例也会构建当前源码。

## 方式一：Docker Compose 同时启动面板和 Caddy

需要 Linux、Docker 与 Compose v2，域名 A/AAAA 记录指向服务器。TCP 80/443 要可达；443/UDP 用于可选 HTTP/3。若服务器已有 1Panel/OpenResty 占用 80/443，应使用现有代理，或先调整其监听端口后再启动 Caddy。

在源码仓库执行：

```sh
cd deploy/caddy
cp .env.example .env
# 编辑 .env，将 PANEL_DOMAIN 改成你的实际域名（不带 https://）。
docker compose build panel
docker compose run --rm panel -check-config
docker compose run --rm -T panel -init-admin admin
# 上一条命令从标准输入读取一行 12–72 字节的密码，输入后回车。
# 仅首次安装执行 init-admin；已有管理员直接使用原账号。
docker compose up -d --wait
docker compose logs --tail=100 caddy panel
```

访问 `https://你的域名/admin`。Caddy 自动申请并续期公开受信任证书。面板容器只在私有容器网络监听 `18888`，不向宿主机发布后端端口；Caddy 的上游是 `panel:18888`，不是容器自己的 `127.0.0.1`。

文件位于 [deploy/caddy](../deploy/caddy/)：

| 文件 | 用途 |
|---|---|
| `compose.yaml` | 从源码构建面板，启动 Caddy，持久化数据库和证书 |
| `config.yaml` | 面板 YAML 配置，容器内监听 `0.0.0.0:18888` |
| `Caddyfile.docker` | 使用 `.env` 的域名，将整站转发给面板 |
| `.env.example` | 域名示例 |

`panel_data` 保存数据库，`caddy_data` 保存证书与账户私钥，`caddy_config` 保存 Caddy 状态。更新时保留这些卷，不执行 `docker compose down -v`。修改面板 YAML 后 `docker compose restart panel`；更新源码后 `docker compose build panel && docker compose up -d --wait`。更新域名后 `docker compose up -d caddy`。

支付通道在面板的「站点设置 → 支付通道」里配置，保存即生效。也可以用 `payments-file` 挂载一份 JSON：它只在**首次启动**时作为初始配置写进数据库，之后以面板里的为准。回调地址用配置中的明确 URL，或设置 `origin: https://你的域名`，不从客户端请求生成支付回调地址。

## 方式二：同机二进制 + Caddy

构建当前源码（需要 Go 1.26；前端产物已包含在仓库）：

```sh
go build -trimpath -o bin/panel ./cmd/panel
```

将程序放到 `/opt/traffic-forwarding-panel/panel`，将 [完整配置示例](../examples/panel.config.yaml) 复制为 `/opt/traffic-forwarding-panel/config.yaml`。最小配置：

```yaml
database-path: sqlite3://data/panel.db
listen: 127.0.0.1:18888
trust-proxy: true
disable-gzip: true
```

本项目无需商业授权码，不填写参考产品的 `key`。认证使用管理员账号、用户 API Token 和设备接入凭据。

相对路径以 **YAML 文件所在目录** 为基准。上面的 SQLite 数据库位于 `/opt/traffic-forwarding-panel/data/panel.db`。准备系统用户和数据目录，并按需要收紧配置文件权限：

```sh
sudo useradd --system --home /opt/traffic-forwarding-panel --shell /usr/sbin/nologin tfp
sudo install -d -o tfp -g tfp -m 700 /opt/traffic-forwarding-panel/data
sudo chown root:tfp /opt/traffic-forwarding-panel/config.yaml
sudo chmod 640 /opt/traffic-forwarding-panel/config.yaml
sudo chmod 755 /opt/traffic-forwarding-panel/panel
sudo -u tfp /opt/traffic-forwarding-panel/panel -config /opt/traffic-forwarding-panel/config.yaml -check-config
sudo -u tfp /opt/traffic-forwarding-panel/panel -config /opt/traffic-forwarding-panel/config.yaml -init-admin admin
```

`useradd` 只在 `tfp` 用户不存在时执行；`init-admin` 只用于空账号库。将 [systemd 服务](../deploy/caddy/traffic-forwarding-panel.service) 安装为 `/etc/systemd/system/traffic-forwarding-panel.service`，再启动：

```sh
sudo systemctl daemon-reload
sudo systemctl enable --now traffic-forwarding-panel
/opt/traffic-forwarding-panel/panel -config /opt/traffic-forwarding-panel/config.yaml -healthcheck
```

安装 [Caddy 2](https://caddyserver.com/docs/install)，将 [Caddyfile](../deploy/caddy/Caddyfile) 的 `panel.example.com` 替换为实际域名，再合并到 `/etc/caddy/Caddyfile`。已有其他站点时保留其站点块。

```caddyfile
panel.example.com {
    encode zstd gzip
    reverse_proxy 127.0.0.1:18888 {
        header_up Host {hostport}
        header_up X-Forwarded-Proto {scheme}
        header_up X-Real-IP {client_ip}
        flush_interval -1
    }
}
```

```sh
sudo caddy validate --config /etc/caddy/Caddyfile --adapter caddyfile
sudo systemctl reload caddy
```

整站反代会使用程序内嵌的同版本前端，并覆盖登录、支付回调、设备接入下载、SSE 和 WebSocket。Caddy 自动处理 WebSocket Upgrade；`flush_interval -1` 使 SSE 及时到达浏览器。

## 由 Caddy 直接提供前端文件

先从正在部署的面板版本导出前端，再使用 [Caddyfile.static](../deploy/caddy/Caddyfile.static)：

```sh
sudo install -d -m 755 /srv/traffic-forwarding-panel
sudo /opt/traffic-forwarding-panel/panel -export-html /srv/traffic-forwarding-panel/public
```

导出目录必须不存在，以免覆盖已定制的前端。导出结果包含 `index.html` 和 `assets/`，可供 Caddy 读取。更新时导出到新目录，例如 `public-next`，检查后更新 Caddy 的 `root` 并重载；前后端应使用同一版本。

`Caddyfile.static` 明确分开四类请求：

| 路径 | 处理方式 |
|---|---|
| `/api/*`、`/online/*`、`/download/*` | 反代到面板，保留路径前缀 |
| `/assets/*` | Caddy 从导出目录读取资源 |
| `/`、`/admin`、`/admin/` | 返回导出的 `index.html` |
| 其他路径 | 404 |

API 的 404 不回退为前端 HTML。数据库、配置文件、证书私钥不放在 `public/` 下。静态方案带上与面板一致的 CSP 等响应头；无需修改面板 `html-path`。如需让面板自己托管导出的前端，配置 `html-path: ./public` 即可。

## 配置字段与兼容性

优先级是 **显式命令行参数 > 非空环境变量 > YAML > 内置默认值**。`TFP_CONFIG_FILE` 可代替 `-config`；原有 `-addr`、`-database`、`-dsn`、`-origin` 等参数继续可用。旧镜像默认的 `TFP_ADDR/TFP_DSN` 会覆盖 YAML，新的 Caddy Compose 已将它们设为空，避免端口或数据库意外仍用旧值。

| 字段 | 行为 |
|---|---|
| `database-path` | 支持 `sqlite3://`、`sqlite://`、`mysql://` 和 `postgres://`；同时接受参考文档的 PostgreSQL `postgres://host=...` 格式 |
| `max-open-connection` / `max-idle-connection` | 设置 SQL 连接池；空闲连接数允许 0，不能大于显式最大连接数 |
| `disable-queue` | 默认 false；true 时 SQLite 绕过串行写队列，仍等待事务提交后确认；MySQL/PostgreSQL 本来就使用并发事务 |
| `listen` | HTTP(S) 监听地址；同机 Caddy 用 `127.0.0.1:18888`，私有容器网络用 `0.0.0.0:18888` |
| `trust-proxy` | 识别受控代理覆盖的 `X-Forwarded-Proto` 和单值 `X-Real-IP`；不使用 `X-Forwarded-Host` 放宽来源验证 |
| `origin` | 可选固定公开地址；留空时按保留的 Host 与访问协议检查同源 |
| `html-path` | 可选外部前端目录，布局应与 `-export-html` 导出一致；省略时内嵌前端仍可用 |
| `tls-cert` / `tls-key` | 成对指定 PEM 文件时，面板直接监听 HTTPS；Caddy 终止 TLS 时省略 |
| `disable-gzip` | 默认 false；只控制内置静态资源 gzip；不压缩 API、SSE 或 WebSocket，不影响 Caddy 自身压缩 |
| `offline-node-time` | 默认 20 秒，最小 20；依据服务器收到节点通信的时间计算在线状态 |
| `offline-node-retention-time` | 默认 86400 秒，最小 600；超时后从实时探针列表隐藏，保留节点记录、历史和设备地址查询 |
| `user-rate-limit` | 可选 `{rate: 秒, limit: 次数}`，登录账号的 API 请求共享额度；Cookie/API Token 及不同来源 IP 计入同一账号 |
| `default-rate-limit` | 可选匿名 API 请求按客户端 IP 限流；健康检查、Agent API、签名支付通知不计入这两类额度 |
| `payments-file` / `agent-dir` | 支付 JSON 初始配置（首次启动落库）、设备二进制发布目录 |
| `key` | 不适用；填写时会提示移除，不将第三方商业授权码当作本项目凭据 |

两项可选请求限流省略时保持既有接口保护；页面首次加载会并行读取多项 API，参考值 `5 次/5 秒` 可能过低。SSE/WebSocket 只统计建立连接的 HTTP 请求，不按消息累计额度。内置登录等敏感接口限流仍独立生效，达到任一限制会返回 429。

`-check-config` 检查 YAML、字段范围和引用的前端/TLS 文件，不连接数据库；`-healthcheck` 读取同一配置检查运行中的 HTTP(S) 和数据库。配置变更需要重启面板，不提供热加载。直接 HTTPS 的健康检查使用配置证书验证服务端，不跳过证书校验。

## CDN 与证书

默认配置使用 Caddy 自动管理公开受信任证书。已有证书可在站点块配置 `tls /path/fullchain.pem /path/privkey.pem`。`tls internal` 仅适合显式信任测试 CA 的本机测试，不作为公网面板或 Agent 的默认方案。

若使用 Cloudflare，使用 Full (strict) 并为回源配置有效证书。需要按最终用户 IP 限流时，在 Caddy 的 `servers` 中配置明确的 CDN `trusted_proxies` 范围和 `client_ip_headers`；`X-Real-IP {client_ip}` 会传递 Caddy 验证后的地址。不要直接透传客户端提交的 `CF-Connecting-IP`。没有配置 CDN 信任时，限流地址会是 CDN 回源地址。

## 验证

安装 Caddy 2 后，在仓库执行：

```sh
TFP_TEST_CADDY=/usr/bin/caddy node scripts/test_caddy.mjs
```

测试编译真实面板，以临时 YAML/SQLite 启动，仅监听 localhost；对整站反代和独立前端分别验证受信任的测试 HTTPS、Secure Cookie、跨域拒绝、SSE 首帧、WebSocket Upgrade、Agent 注册/配置、下载和设备地址路径。测试 CA 仅在测试客户端信任，不安装到操作系统。设备是模拟 HTTP 客户端，不执行远程命令，不代表真实跨机转发验收。公网 DNS、ACME 证书签发和服务器端口可达性仍需在目标服务器核实。
