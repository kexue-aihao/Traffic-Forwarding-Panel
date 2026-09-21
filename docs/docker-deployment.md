# Docker 与 1Panel 部署

本方式在已有 Docker 与 Docker Compose v2 的 Linux 服务器安装面板。支持 amd64/arm64，镜像已包含前端，不需要安装 Go、Node 或数据库服务。默认使用 SQLite，数据保存在宿主机；1Panel/OpenResty 负责 HTTPS。

## 一键安装

```sh
curl -fsSL https://github.com/kexue-aihao/Traffic-Forwarding-Panel/releases/download/v0.1.0-beta.2/install-docker.sh -o install-docker.sh && sudo bash install-docker.sh
```

按提示填写域名（如 `panel.example.com`）及管理员密码（12–72 字节）。脚本自动选择架构、下载镜像包及 SHA256 清单、校验并 `docker load`、配置容器、初始化管理员，等待健康检查通过。管理员用户名默认 `admin`。密码只通过标准输入传给初始化进程，不写入 `.env` 或镜像。

可指定域名、端口、目录及用户名；密码仍交互输入：

```sh
sudo bash install-docker.sh --domain panel.example.com --port 18080 \
  --dir /opt/traffic-forwarding-panel --admin admin
```

首次安装生成：

| 路径/配置 | 内容 |
|---|---|
| `/opt/traffic-forwarding-panel/compose.yaml` | Compose 服务配置 |
| `/opt/traffic-forwarding-panel/.env` | 镜像名、HTTPS 公开地址、宿主机端口 |
| `/opt/traffic-forwarding-panel/data/` | SQLite 数据库及 WAL，UID/GID 65532 |
| `/opt/traffic-forwarding-panel/config/` | 可选支付配置，只读挂载到容器 |
| `traffic-forwarding-panel` | 容器名；自动重启，根文件系统只读，日志轮转 |
| `127.0.0.1:18080` | 默认回环端口；无需在防火墙开放 18080 |

重复运行仅启动现有安装，保留账号、数据库和配置。安装器不会自动替换已有版本，也不会清理已有数据。

## 1Panel 建站

1. 域名解析到服务器。在 **网站 → 创建网站 → 反向代理** 中填写该域名。
2. 当 OpenResty 使用宿主机网络时，代理地址填 **`http://127.0.0.1:18080`**。
3. 申请并启用 HTTPS 证书，建议开启 HTTP 跳转 HTTPS；使用与安装时完全一致的域名。容器内部仍使用 HTTP。
4. 保留请求 Host，启用 WebSocket，关闭代理缓存/缓冲，长连接读取超时设为 3600 秒。

随后访问 `https://panel.example.com/admin`；普通用户入口为 `/`。无需访问后台 Docker 端口初始化网站。

如果需要核对代理配置，网站现有的 `location /` 应包含以下设置（修改已有 location，不要重复添加）：

```nginx
proxy_pass http://127.0.0.1:18080;
proxy_set_header Host $http_host;
proxy_set_header X-Forwarded-Proto $scheme;
proxy_http_version 1.1;
proxy_set_header Upgrade $http_upgrade;
proxy_set_header Connection "upgrade";
proxy_buffering off;
proxy_cache off;
proxy_read_timeout 3600s;
proxy_send_timeout 3600s;
```

`TFP_ORIGIN` 是浏览器访问的完整 HTTPS 地址，决定来源校验和 Secure Cookie。改变域名时更新 `.env` 的 `TFP_ORIGIN`，再执行 `docker compose up -d`。实时探针使用 SSE，需要关闭缓冲；节点终端使用 WebSocket。

### OpenResty 使用桥接网络时

桥接容器的 `127.0.0.1` 指向自身。将面板加入 OpenResty 所在 Docker 网络，然后在 1Panel 使用 `http://traffic-forwarding-panel:8080`。例如网络名为 `1panel-network` 时，在安装目录创建 `compose.override.yaml`：

```yaml
services:
  panel:
    networks:
      - default
      - onepanel
networks:
  onepanel:
    external: true
    name: 1panel-network
```

执行 `sudo docker compose up -d`。网络实际名称可在 1Panel 容器详情中查看；只有 OpenResty 和面板加入同一个网络才能用容器名互访。无需把面板端口开放到公网。

## 常用命令

先进入安装目录：`cd /opt/traffic-forwarding-panel`。以下命令需 root 或 Docker 操作权限。

```sh
docker compose ps                         # 健康状态
docker compose logs --tail=100 -f panel   # 日志
docker compose restart panel             # 重启
docker compose stop                      # 停止，保留数据
docker compose up -d --wait               # 启动并等待健康
docker compose exec panel /panel -version
docker compose exec panel /panel -reset-password admin
```

改密命令从标准输入读取新密码，不把密码写在命令参数中。管理员账号初始化前失败会保留配置和数据库：确认该目录属于本次安装后，可执行 `docker compose run --rm -T panel -init-admin admin`，从标准输入提供密码。若账号已存在，使用本机改密命令；成功后执行 `touch .initialized`、`docker compose up -d --wait`。不要删除已有数据库来重试。

## 备份与支付配置

一致性数据库快照可直接生成到持久化目录：

```sh
docker compose exec panel /panel -backup /data/backup-20260921.jsonl
```

目标文件必须不存在。将生成的 `data/backup-20260921.jsonl` 另存到受保护备份位置，同时保留 `.env`、Compose 文件及 `config/`；原始数据库文件需先停止面板再复制。具体恢复约束见 README。

启用充值渠道时，将仓库 `examples/payments.example.json` 按商户资料填写为安装目录中的 `config/payments.json`：

```sh
chown 65532:65532 config/payments.json
chmod 600 config/payments.json
```

在 `.env` 设置 `TFP_PAYMENTS_FILE=/config/payments.json`，再运行 `docker compose up -d`。不配置支付渠道也可正常登录、管理资源和使用钱包/套餐相关的已有数据；商户实付需另行验收。

## 离线安装和升级

从同一 Release 下载对应架构的 `traffic-forwarding-panel_0.1.0-beta.2_docker_amd64.tar.gz`（ARM64 为 `docker_arm64`）、`compose.yaml`、`install-docker.sh` 和 `docker-SHA256SUMS`，放到一个目录。服务器已有 Docker/Compose 时不需要访问镜像仓库：

```sh
sudo bash install-docker.sh --bundle /path/to/downloads --domain panel.example.com
```

升级采用显式步骤：备份 → 校验新版本 Docker 包 → `docker load -i 新镜像包.tar.gz` → 修改 `.env` 的 `TFP_IMAGE` 为新版本及当前架构 → `docker compose up -d --wait`。保留数据目录和域名配置，勿重新初始化管理员。数据库迁移可能不兼容旧版程序，回退需要同时恢复升级前备份。

镜像只部署面板服务；Agent 仍需安装在真实入口/出口节点。此处的 HTTPS 反代测试不替代真实 1Panel 安装、Linux 跨机转发、容量与商户实付验收。
