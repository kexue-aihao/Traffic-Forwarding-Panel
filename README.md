# Traffic Forwarding Panel

Traffic Forwarding Panel 是一个基于 Go 和 MySQL 的流量转发控制面板。一个二进制同时支持服务端模式和节点模式：服务端负责用户、节点、隧道、支付、用量统计和命令下发，节点负责在本机监听 TCP/UDP 端口并转发到目标地址。

当前代码包含以下能力：

- 管理员后台：登录、查看统计、创建用户、创建节点、创建隧道、管理转发服务、查看支付订单。
- 用户门户：用户登录、查看自己的隧道和订单、创建充值订单。
- 节点 Agent：拉取控制面命令，启动或停止 TCP/UDP 转发，定时上报用量和活跃连接数。
- TCP 转发：按隧道配置监听本地地址，转发到目标 TCP 地址，支持 `max_conn` 活跃连接限制。
- UDP 转发：按客户端地址维护 UDP 会话，支持空闲会话回收和 `max_conn` 会话限制。
- 用量控制：按隧道配额、用户配额、过期时间自动暂停服务。
- 支付插件：内置 `epay` 和 `bepusdt` 签名表单支付适配器。
- 部署方式：Docker Compose、1Panel Compose、自编译二进制 + systemd。

## 架构说明

```text
管理员/用户浏览器
        |
        v
Traffic Panel 服务端  <---- 支付平台回调
        |
        v
      MySQL
        ^
        |
节点 Agent 定时拉取命令、上报用量
        |
        v
TCP/UDP 监听端口 ----> 目标服务
```

主要数据表由服务端启动时自动创建：

- `admins`：管理员账号。
- `users`：用户账号、余额、流量额度和已用流量。
- `nodes`：节点信息和节点密钥。
- `tunnels`：隧道配置。
- `forward_services`：实际转发服务状态。
- `node_commands`：服务端下发给节点的命令队列。
- `usage_reports`：节点上报的用量记录。
- `payment_channels`、`payment_orders`：支付渠道和订单。
- `sessions`：登录会话。
- `audit_logs`：审计日志表。

## 重要安全要求

当前版本默认启用了启动安全检查。生产环境必须设置强密钥和强管理员密码，否则服务端会拒绝启动。

必须修改：

- `TP_MASTER_SECRET`：不要使用 `change-me` 或 `change-me-in-production`。
- `TP_BOOTSTRAP_ADMIN_PASS`：不要使用默认值 `admin123456`。
- MySQL root 密码和业务用户密码。
- 节点密钥，建议每个节点单独生成。

仅本地临时测试时才可以设置：

```sh
TP_ALLOW_INSECURE_DEFAULTS=true
```

不建议在公网环境使用这个开关。

## 快速部署：Docker Compose

这是最推荐的部署方式。适合普通 Linux 服务器，也适合先在本机验证功能。

### 1. 准备环境

服务器需要安装：

- Docker
- Docker Compose v2
- Git

检查命令：

```sh
docker --version
docker compose version
git --version
```

### 2. 拉取代码

```sh
git clone https://github.com/kexue-aihao/Traffic-Forwarding-Panel.git
cd Traffic-Forwarding-Panel
```

### 3. 创建 `.env`

复制示例文件：

```sh
cp deploy/env.example .env
```

编辑 `.env`：

```sh
nano .env
```

至少要改这些值：

```env
TP_HTTP_PORT=8080
TP_BASE_URL=http://你的服务器IP:8080
TP_MASTER_SECRET=请替换为至少32位随机字符串
TP_ALLOW_INSECURE_DEFAULTS=false
TP_BOOTSTRAP_ADMIN_USER=admin
TP_BOOTSTRAP_ADMIN_PASS=请替换为强密码

MYSQL_ROOT_PASSWORD=请替换为强root密码
MYSQL_DATABASE=traffic_panel
MYSQL_USER=trafficpanel
MYSQL_PASSWORD=请替换为强数据库密码
```

可以用下面命令生成随机密钥：

```sh
openssl rand -base64 32
```

如果最终使用域名和 HTTPS，`TP_BASE_URL` 必须写最终访问地址，例如：

```env
TP_BASE_URL=https://panel.example.com
```

### 4. 启动服务

```sh
docker compose up -d --build
```

查看容器：

```sh
docker compose ps
```

查看日志：

```sh
docker compose logs -f panel
```

看到类似内容表示服务端启动成功：

```text
trafficpanel server listening on :8080
```

### 5. 访问面板

浏览器打开：

```text
http://服务器IP:8080
```

页面入口：

- 首页：`/`
- 管理员后台：`/admin`
- 用户门户：`/user`
- 健康检查：`/healthz`

管理员账号来自 `.env`：

```text
用户名：TP_BOOTSTRAP_ADMIN_USER
密码：TP_BOOTSTRAP_ADMIN_PASS
```

注意：初始管理员只会在 `admins` 表为空时创建。如果已经初始化过数据库，修改 `.env` 里的 `TP_BOOTSTRAP_ADMIN_PASS` 不会自动修改已有管理员密码。

### 6. 停止和升级

停止：

```sh
docker compose down
```

保留数据升级：

```sh
git pull
docker compose up -d --build
```

完全清理数据需要删除 volume，谨慎执行：

```sh
docker compose down -v
```

## 1Panel 部署

仓库提供了 1Panel Compose 文件：

- `deploy/1panel/docker-compose.yml`
- `deploy/1panel/app.yml`
- `deploy/1panel/README.md`

默认镜像：

```text
ghcr.io/kexue-aihao/traffic-forwarding-panel:latest
```

### 方式一：使用 1Panel 容器编排

1. 登录 1Panel。
2. 打开「容器」->「镜像」->「拉取镜像」。
3. 镜像名填写：

```text
ghcr.io/kexue-aihao/traffic-forwarding-panel:latest
```

4. 打开「容器」->「编排」。
5. 新建编排，名称建议为 `trafficpanel`。
6. 将 `deploy/1panel/docker-compose.yml` 内容复制进去。
7. 在环境变量中填写：

```env
PANEL_IMAGE=ghcr.io/kexue-aihao/traffic-forwarding-panel:latest
PANEL_HTTP_PORT=8080
PANEL_BASE_URL=http://你的服务器IP:8080
PANEL_MASTER_SECRET=请替换为至少32位随机字符串
PANEL_ADMIN_USER=admin
PANEL_ADMIN_PASSWORD=请替换为强密码

PANEL_DB_ROOT_PASSWORD=请替换为强root密码
PANEL_DB_NAME=traffic_panel
PANEL_DB_USER=trafficpanel
PANEL_DB_PASSWORD=请替换为强数据库密码

PANEL_EPAY_API_URL=
PANEL_EPAY_PID=
PANEL_EPAY_KEY=
PANEL_EPAY_TYPE=alipay

PANEL_BEPUSDT_API_URL=
PANEL_BEPUSDT_PID=
PANEL_BEPUSDT_KEY=
PANEL_BEPUSDT_TYPE=usdt
```

8. 启动编排。
9. 在「容器」列表确认 MySQL 和 Traffic Panel 都是运行状态。
10. 浏览器访问：

```text
http://服务器IP:8080
```

### 配置 1Panel 反向代理和 HTTPS

如果使用域名访问：

1. 打开「网站」。
2. 新建反向代理网站。
3. 主域名填写 `panel.example.com`。
4. 代理地址填写：

```text
http://127.0.0.1:8080
```

5. 申请或绑定 SSL 证书。
6. 将编排环境变量中的 `PANEL_BASE_URL` 改为：

```env
PANEL_BASE_URL=https://panel.example.com
```

7. 重建或重启编排。

`PANEL_BASE_URL` 会用于支付通知地址生成，必须和用户、支付平台能访问到的公网地址一致。

## 普通 Linux + systemd 部署

适合不使用 Docker 的服务器。需要自己准备 MySQL，并使用编译好的二进制。

### 1. 安装依赖

服务器需要：

- Linux amd64 或 arm64
- MySQL 8.x 或兼容版本
- systemd

### 2. 编译二进制

在有 Go 环境的机器上执行：

```sh
go test ./...
make build-linux
```

生成文件：

```text
dist/trafficpanel-linux-amd64
dist/trafficpanel-linux-arm64
```

如果服务器是 amd64：

```sh
cp dist/trafficpanel-linux-amd64 ./trafficpanel
```

如果服务器是 arm64：

```sh
cp dist/trafficpanel-linux-arm64 ./trafficpanel
```

### 3. 创建数据库

登录 MySQL 后执行，密码请自行替换：

```sql
CREATE DATABASE traffic_panel CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci;
CREATE USER 'trafficpanel'@'%' IDENTIFIED BY '请替换为强数据库密码';
GRANT ALL PRIVILEGES ON traffic_panel.* TO 'trafficpanel'@'%';
FLUSH PRIVILEGES;
```

如果 MySQL 和服务端在同一台机器，也可以把 `'%'` 改为 `'127.0.0.1'` 或 `'localhost'`。

### 4. 安装服务端

必须传入强密钥和强管理员密码，否则服务会启动失败。

```sh
chmod +x ./trafficpanel

MYSQL_HOST=127.0.0.1 \
MYSQL_PORT=3306 \
MYSQL_DATABASE=traffic_panel \
MYSQL_USER=trafficpanel \
MYSQL_PASSWORD='请替换为强数据库密码' \
HTTP_ADDR=:8080 \
BASE_URL=http://你的服务器IP:8080 \
MASTER_SECRET='请替换为至少32位随机字符串' \
ADMIN_USER=admin \
ADMIN_PASSWORD='请替换为强管理员密码' \
sh scripts/install-linux.sh
```

脚本会创建：

- `/usr/local/bin/trafficpanel`
- `/opt/trafficpanel/env`
- `/etc/systemd/system/trafficpanel.service`

查看状态：

```sh
systemctl status trafficpanel --no-pager
```

查看日志：

```sh
journalctl -u trafficpanel -f
```

重启：

```sh
systemctl restart trafficpanel
```

### 5. 配置反向代理

如果用 Nginx，可以参考：

```nginx
server {
    listen 80;
    server_name panel.example.com;

    location / {
        proxy_pass http://127.0.0.1:8080;
        proxy_set_header Host $host;
        proxy_set_header X-Real-IP $remote_addr;
        proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
        proxy_set_header X-Forwarded-Proto $scheme;
    }
}
```

启用 HTTPS 后，把 `/opt/trafficpanel/env` 中的 `TP_BASE_URL` 改为 HTTPS 地址，然后重启：

```sh
systemctl restart trafficpanel
```

## 节点部署

节点使用同一个二进制，但运行模式为 `TP_MODE=node`。节点会连接服务端 API，拉取命令后在节点本机监听转发端口。

### 1. 在管理端准备节点信息

当前管理后台页面的创建节点表单只填写名称、主机和端口。为了确保后续节点能部署成功，建议通过 API 创建节点并手动指定密钥。

先在 `/admin` 登录，浏览器开发者工具或页面返回中拿到管理员 token。然后执行：

```sh
curl -X POST 'https://panel.example.com/api/admin/nodes' \
  -H 'Authorization: Bearer 管理员TOKEN' \
  -H 'Content-Type: application/json' \
  -d '{
    "name":"edge-1",
    "host":"203.0.113.10",
    "port":0,
    "secret":"请替换为节点随机密钥"
  }'
```

返回中的 `id` 就是节点 ID。部署节点时需要：

- 节点 ID
- 刚才填写的节点密钥
- 服务端公网地址

节点密钥可以生成：

```sh
openssl rand -base64 32
```

### 2. 安装节点服务

将匹配架构的 `trafficpanel` 二进制上传到节点机器当前目录，然后执行：

```sh
chmod +x ./trafficpanel

AGENT_SERVER_URL=https://panel.example.com \
AGENT_NODE_ID=1 \
AGENT_NODE_SECRET='请替换为节点随机密钥' \
AGENT_NODE_NAME=edge-1 \
AGENT_NODE_HOST=203.0.113.10 \
AGENT_NODE_PORT=0 \
AGENT_UDP_IDLE_TIMEOUT=2m \
sh scripts/install-node-linux.sh
```

脚本会创建：

- `/usr/local/bin/trafficpanel`
- `/opt/trafficpanel-node/env`
- `/etc/systemd/system/trafficpanel-node.service`

查看状态：

```sh
systemctl status trafficpanel-node --no-pager
```

查看日志：

```sh
journalctl -u trafficpanel-node -f
```

### 3. 节点防火墙要求

节点必须能访问服务端：

```text
AGENT_SERVER_URL 指向的 HTTP/HTTPS 地址
```

用户流量访问的是节点机器上的隧道监听端口，因此还需要在节点服务器安全组或防火墙放行对应端口。例如隧道监听 `:9000`，就要放行节点的 `9000/tcp` 或 `9000/udp`。

## 创建用户和隧道

### 1. 创建用户

可以在 `/admin` 页面创建用户，或使用 API：

```sh
curl -X POST 'https://panel.example.com/api/admin/users' \
  -H 'Authorization: Bearer 管理员TOKEN' \
  -H 'Content-Type: application/json' \
  -d '{
    "username":"user1",
    "password":"请替换为用户密码",
    "flow_quota_mb":10240
  }'
```

返回的 `id` 是用户 ID。

### 2. 创建隧道

```sh
curl -X POST 'https://panel.example.com/api/admin/tunnels' \
  -H 'Authorization: Bearer 管理员TOKEN' \
  -H 'Content-Type: application/json' \
  -d '{
    "user_id":1,
    "node_id":1,
    "name":"web-9000",
    "protocol":"tcp",
    "listen_addr":":9000",
    "target_addr":"127.0.0.1:80",
    "max_conn":100,
    "speed_limit_kb":0,
    "quota_bytes":0,
    "auto_pause_on_limit":true
  }'
```

字段说明：

- `protocol`：`tcp` 或 `udp`。
- `listen_addr`：节点本机监听地址，例如 `:9000` 或 `0.0.0.0:9000`。
- `target_addr`：节点可访问的目标地址，例如 `127.0.0.1:80`。
- `max_conn`：TCP 为最大活跃连接数，UDP 为最大活跃客户端会话数；`0` 表示不限制。
- `speed_limit_kb`：当前字段会入库和下发，但转发层尚未实现限速。
- `quota_bytes`：隧道用量配额，`0` 表示不限制。
- `auto_pause_on_limit`：达到配额或过期后是否自动暂停。

创建成功后，服务端会向节点下发命令。节点下一次轮询后会启动对应监听端口。

## 支付配置

内置支付渠道：

- `epay`
- `bepusdt`

支付参数通过环境变量配置。

### Epay

```env
TP_EPAY_API_URL=https://pay.example.com
TP_EPAY_PID=1000
TP_EPAY_KEY=请替换为支付密钥
TP_EPAY_TYPE=alipay
```

### BEpusdt

```env
TP_BEPUSDT_API_URL=https://bepusdt.example.com
TP_BEPUSDT_PID=1000
TP_BEPUSDT_KEY=请替换为支付密钥
TP_BEPUSDT_TYPE=usdt
```

支付通知地址由 `TP_BASE_URL` 自动拼接：

```text
https://panel.example.com/api/pay/epay/notify
https://panel.example.com/api/pay/bepusdt/notify
```

支付安全行为：

- 配置了支付密钥时，回调必须携带合法签名。
- 回调订单号必须存在。
- 回调通道必须和订单通道一致。
- 如果回调携带金额，金额必须和订单金额一致。
- 已支付订单重复回调不会重复给用户加余额。

## 环境变量完整说明

### 服务端变量

| 变量 | 默认值 | 说明 |
| --- | --- | --- |
| `TP_MODE` | `server` | 运行模式，服务端使用 `server` |
| `TP_HTTP_ADDR` | `:8080` | HTTP 监听地址 |
| `TP_DATABASE_DSN` | `root:root@tcp(127.0.0.1:3306)/traffic_panel?...` | MySQL DSN |
| `TP_APP_NAME` | `Traffic Panel` | 页面显示名称 |
| `TP_BASE_URL` | `http://127.0.0.1:8080` | 外部访问地址，支付通知依赖它 |
| `TP_MASTER_SECRET` | `change-me-in-production` | 主密钥，生产必须修改 |
| `TP_ALLOW_INSECURE_DEFAULTS` | `false` | 是否允许默认弱配置，仅本地开发使用 |
| `TP_SESSION_TTL` | `24h` | 登录会话有效期 |
| `TP_BOOTSTRAP_ADMIN_USER` | `admin` | 初始管理员用户名 |
| `TP_BOOTSTRAP_ADMIN_PASS` | `admin123456` | 初始管理员密码，生产必须修改 |
| `TP_BOOTSTRAP_ADMIN_EMAIL` | `admin@example.com` | 预留字段，当前未写入表结构 |
| `TP_EPAY_API_URL` | 空 | Epay 网关地址 |
| `TP_EPAY_PID` | 空 | Epay 商户 ID |
| `TP_EPAY_KEY` | 空 | Epay 签名密钥 |
| `TP_EPAY_TYPE` | `alipay` | Epay 支付类型 |
| `TP_BEPUSDT_API_URL` | 空 | BEpusdt 网关地址 |
| `TP_BEPUSDT_PID` | 空 | BEpusdt 商户 ID |
| `TP_BEPUSDT_KEY` | 空 | BEpusdt 签名密钥 |
| `TP_BEPUSDT_TYPE` | `usdt` | BEpusdt 支付类型 |

### 节点变量

| 变量 | 默认值 | 说明 |
| --- | --- | --- |
| `TP_MODE` | `server` | 节点必须设置为 `node` |
| `TP_AGENT_SERVER_URL` | `http://127.0.0.1:8080` | 服务端地址 |
| `TP_AGENT_NODE_ID` | `0` | 节点 ID，必须大于 0 |
| `TP_AGENT_NODE_SECRET` | 空 | 节点密钥，必须填写 |
| `TP_AGENT_NODE_NAME` | `default-node` | 节点名称 |
| `TP_AGENT_NODE_HOST` | `127.0.0.1` | 节点公网地址或展示地址 |
| `TP_AGENT_NODE_PORT` | `0` | 节点展示端口 |
| `TP_NODE_POLL_INTERVAL` | `3s` | 节点拉取命令间隔 |
| `TP_NODE_REPORT_INTERVAL` | `10s` | 节点上报用量间隔 |
| `TP_AGENT_UDP_IDLE_TIMEOUT` | `2m` | UDP 会话空闲回收时间 |

## API 入口

页面：

| 路径 | 说明 |
| --- | --- |
| `/` | 首页 |
| `/admin` | 管理员后台 |
| `/user` | 用户门户 |
| `/healthz` | 健康检查 |

管理员 API：

| 路径 | 方法 | 说明 |
| --- | --- | --- |
| `/api/admin/login` | `POST` | 管理员登录 |
| `/api/logout` | `POST` | 退出登录 |
| `/api/admin/summary` | `GET` | 统计概览 |
| `/api/admin/users` | `GET/POST` | 用户列表/创建用户 |
| `/api/admin/nodes` | `GET/POST` | 节点列表/创建节点 |
| `/api/admin/tunnels` | `GET/POST` | 隧道列表/创建隧道 |
| `/api/admin/services` | `GET/POST` | 服务列表/暂停、恢复、删除服务 |
| `/api/admin/payments/channels` | `GET` | 支付渠道 |
| `/api/admin/payments/orders` | `GET` | 支付订单 |

用户 API：

| 路径 | 方法 | 说明 |
| --- | --- | --- |
| `/api/user/login` | `POST` | 用户登录 |
| `/api/user/me` | `GET` | 当前用户信息 |
| `/api/user/tunnels` | `GET` | 当前用户隧道 |
| `/api/user/orders` | `GET` | 当前用户订单 |
| `/api/user/pay` | `POST` | 创建支付订单 |

节点 API：

| 路径 | 方法 | 说明 |
| --- | --- | --- |
| `/api/nodes/register` | `POST` | 节点注册/心跳信息同步 |
| `/api/nodes/report` | `POST` | 节点上报用量 |
| `/api/nodes/commands` | `GET` | 节点拉取命令 |
| `/api/nodes/commands/ack` | `POST` | 节点确认命令 |

节点 API 使用 `X-Node-ID` 和 `X-Node-Sign` 请求头做 HMAC 签名认证。

## 本地开发

### 1. 准备 MySQL

可以使用 Docker 启动一个 MySQL：

```sh
docker run -d --name trafficpanel-mysql \
  -e MYSQL_ROOT_PASSWORD=rootpass \
  -e MYSQL_DATABASE=traffic_panel \
  -e MYSQL_USER=trafficpanel \
  -e MYSQL_PASSWORD=trafficpanel_pass \
  -p 3306:3306 \
  mysql:8.4
```

### 2. 运行测试

```sh
go test ./...
```

### 3. 启动服务端

```sh
TP_MODE=server \
TP_HTTP_ADDR=:8080 \
TP_DATABASE_DSN='trafficpanel:trafficpanel_pass@tcp(127.0.0.1:3306)/traffic_panel?parseTime=true&charset=utf8mb4&loc=Local' \
TP_BASE_URL=http://127.0.0.1:8080 \
TP_MASTER_SECRET='local-development-secret-change-me-32' \
TP_BOOTSTRAP_ADMIN_USER=admin \
TP_BOOTSTRAP_ADMIN_PASS='local-admin-password' \
go run ./cmd/trafficpanel
```

访问：

```text
http://127.0.0.1:8080/admin
```

## 构建

本机平台构建：

```sh
go build -trimpath -ldflags='-s -w' -o bin/trafficpanel ./cmd/trafficpanel
```

Linux amd64/arm64 构建：

```sh
make build-linux
```

Docker 镜像构建：

```sh
docker build -t trafficpanel:local .
```

多架构镜像构建：

```sh
docker buildx build --platform linux/amd64,linux/arm64 -t trafficpanel:latest .
```

GitHub Actions 会在推送到 `master` 时执行：

```sh
go test ./...
```

然后构建并推送 GHCR 镜像：

```text
ghcr.io/kexue-aihao/traffic-forwarding-panel:latest
ghcr.io/kexue-aihao/traffic-forwarding-panel:sha-<commit>
```

## 常见问题

### 1. 启动时报 `TP_MASTER_SECRET must be changed`

原因：使用了默认弱主密钥。

解决：设置强随机值：

```env
TP_MASTER_SECRET=请替换为至少32位随机字符串
```

### 2. 启动时报 `TP_BOOTSTRAP_ADMIN_PASS must be changed`

原因：使用了默认管理员密码 `admin123456`。

解决：设置强密码：

```env
TP_BOOTSTRAP_ADMIN_PASS=请替换为强密码
```

### 3. Docker Compose 提示 `set TP_MASTER_SECRET`

原因：`.env` 缺少必填变量。

解决：复制并编辑示例文件：

```sh
cp deploy/env.example .env
nano .env
```

### 4. 页面能打开，但支付回调地址不对

检查 `TP_BASE_URL` 或 `PANEL_BASE_URL`。它必须是公网最终访问地址，例如：

```env
TP_BASE_URL=https://panel.example.com
```

修改后重启服务。

### 5. 节点启动失败，提示节点 ID 或密钥缺失

节点模式必须设置：

```env
TP_AGENT_NODE_ID=1
TP_AGENT_NODE_SECRET=节点密钥
```

并且服务端 `nodes` 表里必须存在对应 ID 和密钥。

### 6. 隧道创建成功但端口无法访问

按顺序检查：

1. 节点服务是否运行：`systemctl status trafficpanel-node`。
2. 节点是否能访问服务端 `TP_AGENT_SERVER_URL`。
3. 管理端服务列表中服务是否为 `active`。
4. 节点日志是否显示监听成功。
5. 节点服务器防火墙或云安全组是否放行隧道端口。
6. `target_addr` 是否能从节点机器访问。

### 7. 修改初始管理员密码后没有生效

初始管理员只在 `admins` 表为空时创建。已有管理员密码不会被环境变量覆盖。需要直接在数据库中重置，或清空数据卷后重新初始化。

## 运维建议

- 给 MySQL 做定期备份，至少备份 `traffic_panel` 数据库。
- 生产环境使用 HTTPS。
- 支付回调地址必须走 HTTPS。
- 节点密钥不要复用，泄露后应重新创建节点或更新数据库密钥。
- 转发端口需要结合防火墙和云安全组管理。
- 不要在生产环境设置 `TP_ALLOW_INSECURE_DEFAULTS=true`。
- 升级前先备份数据库。

## 许可证

当前仓库未声明开源许可证。使用、分发或商用前请先确认仓库所有者的授权要求。
