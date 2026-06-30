# 1Panel Deployment / 1Panel 部署

## 中文

GitHub Actions 会在 `master` 推送后自动构建并推送多架构 Docker 镜像。1Panel 中可以直接拉取镜像，通过容器编排启动，再用网站反向代理完成访问。

默认镜像：

```text
ghcr.io/kexue-aihao/traffic-forwarding-panel:latest
```

提交标签镜像：

```text
ghcr.io/kexue-aihao/traffic-forwarding-panel:sha-<commit>
```

本目录提供两份编排文件：

- `docker-compose.external-mysql.yml`：推荐。只启动 Traffic Panel，连接 1Panel「数据库」中托管的 MySQL。
- `docker-compose.yml`：自带 MySQL 容器。数据库数据保存在当前编排目录的 `./data/mysql`。

如果你希望 1Panel 能直接管理、备份和查看转发面板数据库，请使用 `docker-compose.external-mysql.yml`。

### 方式一：使用 1Panel 托管 MySQL，推荐

1. 登录 1Panel。
2. 打开「数据库」->「MySQL」。
3. 创建数据库和用户：

```text
数据库名：traffic_panel
用户名：trafficpanel
密码：replace-db-password
```

4. 记录数据库连接地址和端口。容器内不能盲目使用 `127.0.0.1`，因为它指向应用容器自身；应使用 1Panel 给出的 MySQL 连接地址、MySQL 容器名、同网络服务名或宿主机内网 IP。
5. 打开「容器」->「镜像」->「拉取镜像」。
6. 镜像名填写：

```text
ghcr.io/kexue-aihao/traffic-forwarding-panel:latest
```

7. 打开「容器」->「编排」。
8. 新建编排，名称建议填写 `trafficpanel`。
9. 将 `docker-compose.external-mysql.yml` 内容复制到编排内容中。
10. 在编排环境变量中填写：

```sh
PANEL_IMAGE=ghcr.io/kexue-aihao/traffic-forwarding-panel:latest
PANEL_HTTP_PORT=8080
PANEL_BASE_URL=https://panel.example.com
PANEL_MASTER_SECRET=replace-with-long-random-secret
PANEL_ADMIN_USER=admin
PANEL_ADMIN_PASSWORD=replace-admin-password

PANEL_DB_HOST=mysql-host-reachable-from-container
PANEL_DB_PORT=3306
PANEL_DB_NAME=traffic_panel
PANEL_DB_USER=trafficpanel
PANEL_DB_PASSWORD=replace-db-password

PANEL_EPAY_API_URL=
PANEL_EPAY_PID=
PANEL_EPAY_KEY=
PANEL_EPAY_TYPE=alipay

PANEL_BEPUSDT_API_URL=
PANEL_BEPUSDT_PID=
PANEL_BEPUSDT_KEY=
PANEL_BEPUSDT_TYPE=usdt
```

11. 启动编排。
12. 查看容器日志，确认出现：

```text
trafficpanel server listening on :8080
```

13. 浏览器访问 `http://服务器IP:8080` 验证服务。

### 方式二：编排内自带 MySQL

如果不想使用 1Panel 的数据库功能，可以使用 `docker-compose.yml`。这个方式会额外启动 `mysql` 容器。

1. 拉取镜像 `ghcr.io/kexue-aihao/traffic-forwarding-panel:latest`。
2. 打开「容器」->「编排」。
3. 新建编排，名称建议填写 `trafficpanel`。
4. 将 `docker-compose.yml` 内容复制到编排内容中。
5. 在编排环境变量中填写：

```sh
PANEL_IMAGE=ghcr.io/kexue-aihao/traffic-forwarding-panel:latest
PANEL_HTTP_PORT=8080
PANEL_BASE_URL=https://panel.example.com
PANEL_MASTER_SECRET=replace-with-long-random-secret
PANEL_ADMIN_USER=admin
PANEL_ADMIN_PASSWORD=replace-admin-password

PANEL_DB_ROOT_PASSWORD=replace-root-password
PANEL_DB_NAME=traffic_panel
PANEL_DB_USER=trafficpanel
PANEL_DB_PASSWORD=replace-db-password

PANEL_EPAY_API_URL=
PANEL_EPAY_PID=
PANEL_EPAY_KEY=
PANEL_EPAY_TYPE=alipay

PANEL_BEPUSDT_API_URL=
PANEL_BEPUSDT_PID=
PANEL_BEPUSDT_KEY=
PANEL_BEPUSDT_TYPE=usdt
```

6. 启动编排。
7. 打开「容器」->「容器」，确认 `trafficpanel-trafficpanel` 和 `trafficpanel-mysql` 都处于运行状态。

### 反向代理建站

1. 打开「网站」。
2. 新建网站，选择「反向代理」。
3. 主域名填写你的域名，例如 `panel.example.com`。
4. 代理地址填写：

```text
http://127.0.0.1:8080
```

5. 保存网站。
6. 在网站设置中申请或绑定 SSL 证书。
7. 确认 `PANEL_BASE_URL` 与最终访问地址一致，例如：

```text
https://panel.example.com
```

### 升级镜像

推送新代码到 `master` 后，GitHub Actions 会重新构建 `latest` 镜像。1Panel 升级流程：

1. 打开「容器」->「镜像」。
2. 重新拉取 `ghcr.io/kexue-aihao/traffic-forwarding-panel:latest`。
3. 打开「容器」->「编排」。
4. 选择 `trafficpanel` 编排并重新创建/重启。
5. 打开网站验证版本是否更新。

### 端口说明

- `PANEL_HTTP_PORT=8080` 是宿主机暴露端口。
- 反向代理目标固定指向 `127.0.0.1:8080`。
- 节点流量转发端口不经过此反向代理，需要在节点机器安全组/防火墙中单独放行。

## English

GitHub Actions builds and pushes a multi-architecture Docker image after every `master` push.

Default image:

```text
ghcr.io/kexue-aihao/traffic-forwarding-panel:latest
```

This directory includes two Compose files:

- `docker-compose.external-mysql.yml`: recommended. Runs only Traffic Panel and connects to a MySQL database managed by 1Panel.
- `docker-compose.yml`: runs Traffic Panel plus a dedicated MySQL container.

### Recommended: 1Panel-Managed MySQL

1. Create a MySQL database and user in 1Panel's database page.
2. Pull `ghcr.io/kexue-aihao/traffic-forwarding-panel:latest`.
3. Create a Compose project and paste `docker-compose.external-mysql.yml`.
4. Fill `PANEL_DB_HOST`, `PANEL_DB_PORT`, `PANEL_DB_NAME`, `PANEL_DB_USER`, and `PANEL_DB_PASSWORD`.
5. Start the project and check the container logs.

Use a MySQL host reachable from inside the Traffic Panel container. Do not assume `127.0.0.1` works inside Docker.

### Built-In MySQL

Use `docker-compose.yml` if you want the Compose project to start its own MySQL container. The database data is stored under `./data/mysql` in the Compose project directory.

### Website Reverse Proxy

Create a reverse proxy website in 1Panel and proxy to:

```text
http://127.0.0.1:8080
```

Set `PANEL_BASE_URL` to the final public URL, for example `https://panel.example.com`.
