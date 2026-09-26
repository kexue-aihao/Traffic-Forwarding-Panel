# Docker 与 1Panel 部署

本方式在已有 Docker 与 Docker Compose v2 的 Linux 服务器安装面板。支持 amd64/arm64，镜像已包含前端，不需要安装 Go、Node 或数据库服务。默认使用 SQLite，数据保存在宿主机；1Panel/OpenResty 负责 HTTPS。

使用 Caddy 自动 HTTPS 与 YAML 启动配置时，见 [Caddy 部署](caddy-deployment.md)。该示例从当前源码构建，包含独立的 `deploy/caddy/compose.yaml`，不覆盖这里的现有 1Panel 安装。

## 一键安装或升级

Debian 服务器已安装 curl、Docker 和 Docker Compose v2 时，复制下面完整的一行执行。root 用户可直接运行，普通用户使用 sudo 提权。命令自动获取最新正式版，首次安装和后续升级均适用：

```sh
curl -fsSL https://github.com/kexue-aihao/Traffic-Forwarding-Panel/raw/master/install.sh | bash
```

入口先将管理脚本完整下载到临时文件，再执行，结束后清理临时文件。运行后会打开管理菜单，可选择首次安装、升级到最新正式版或重置密码；密码重置默认使用 `admin` 账号，也可在菜单中输入其他账号。复制时不要手动换行；若终端出现 `>` 等待后续输入，先按 `Ctrl+C`，再重新复制整行。

在菜单选择首次安装后，无需域名或密码输入。脚本自动选择架构、下载镜像包及 SHA256 清单、校验并 `docker load`、配置容器、初始化管理员，等待健康检查通过。管理员用户名默认 `admin`，使用系统随机源生成 48 位密码，安装完成时显示；请保存并可在登录后修改。密码只通过标准输入传给初始化进程，不写入 `.env` 或镜像。若终端输出丢失，可使用下文的本机改密命令。

如需指定端口、目录、用户名或使用 `--password-stdin`，先将安装脚本保存到本地：

```sh
curl -fsSL https://github.com/kexue-aihao/Traffic-Forwarding-Panel/releases/latest/download/install-docker.sh -o install-docker.sh
sudo bash install-docker.sh --port 18080 \
  --dir /opt/traffic-forwarding-panel --admin admin
```

首次安装生成：

| 路径/配置 | 内容 |
|---|---|
| `/opt/traffic-forwarding-panel/compose.yaml` | Compose 服务配置 |
| `/opt/traffic-forwarding-panel/.env` | 镜像名、宿主机端口；`TFP_ORIGIN` 默认留空 |
| `/opt/traffic-forwarding-panel/data/` | SQLite 数据库及 WAL，UID/GID 65532 |
| `/opt/traffic-forwarding-panel/config/` | 可选支付配置，只读挂载到容器 |
| `traffic-forwarding-panel` | 容器名；自动重启，根文件系统只读，日志轮转 |
| `127.0.0.1:18080` | 默认回环端口；无需在防火墙开放 18080 |

如需指定初始密码，可使用 `--password-stdin` 从标准输入传入一行 12–72 字节的密码；默认无需该选项。

已有旧版会先下载、校验并导入目标镜像，再停机备份整个安装目录，仅更新 `.env` 的 `TFP_IMAGE` 并重建面板容器；管理员账号、数据库、端口、域名、支付配置、Compose 及覆盖文件均保留。`--port`、`--admin` 和 `--password-stdin` 只用于首次安装。统一入口在线安装或升级时始终获取最新 Release 安装器；旧版 `install-docker.sh` 本身不会自动更新，直接重复运行它只会启动原容器。

脚本最后同时核对容器镜像 ID 和 `/panel -version`，通过后显示“升级完成，当前运行版本”。同版本重复执行只启动并验证，不下载镜像或重复备份；旧脚本不能自动降级较新的安装。同一目录的并发安装/升级会被锁阻止，需要系统提供 `flock`（常见发行版的 util-linux 已包含）。

## 1Panel 建站

1. 域名解析到服务器。在 **网站 → 创建网站 → 反向代理** 中填写该域名。
2. 当 OpenResty 使用宿主机网络时，代理地址填 **`http://127.0.0.1:18080`**。
3. 申请并启用 HTTPS 证书，建议开启 HTTP 跳转 HTTPS；域名只在 1Panel 设置。容器内部仍使用 HTTP。
4. 保留请求 Host，将 `X-Forwarded-Proto` 设置为 `$scheme`，启用 WebSocket，关闭代理缓存/缓冲，长连接读取超时设为 3600 秒。

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

Compose 默认启用 `TFP_TRUST_PROXY=true`，从受控代理覆盖的 `X-Forwarded-Proto` 识别 HTTPS，按请求 Host 校验浏览器来源并设置 Secure Cookie；不会使用 `X-Forwarded-Host` 放宽来源校验。`TFP_ORIGIN` 可留空，改变域名只需更新 1Panel。若主动设置 `TFP_ORIGIN`，则仍强制使用该固定公开地址。请保持面板回环端口或受控容器网络，代理应覆盖协议头，不能直接透传客户端值。实时探针使用 SSE，需要关闭缓冲；节点终端使用 WebSocket。

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

## 综合管理脚本

也可以直接用统一入口运行安装、升级和密码重置；下面的管理脚本还提供卸载操作：

该脚本面向 Docker Compose 部署；Caddy + systemd 原生部署请继续使用对应的二进制和 service 配置。

```sh
curl -fsSL https://raw.githubusercontent.com/kexue-aihao/Traffic-Forwarding-Panel/master/scripts/panel-manager.sh -o /tmp/panel-manager.sh && sudo bash /tmp/panel-manager.sh
```

脚本默认使用 `/opt/traffic-forwarding-panel`，不带参数时打开菜单，也可以直接指定操作：

```sh
sudo bash /tmp/panel-manager.sh install
sudo bash /tmp/panel-manager.sh upgrade
sudo bash /tmp/panel-manager.sh reset-password --admin admin
sudo bash /tmp/panel-manager.sh uninstall              # 保留数据
sudo bash /tmp/panel-manager.sh uninstall --delete-data --yes
```

安装和升级仍使用带镜像校验、停机备份及健康检查的官方安装器。重置密码会沿用当前 Compose 的数据库配置，并撤销旧会话和 API Token。卸载默认只删除容器和服务，保留数据库与配置；只有同时指定 `--delete-data` 才会删除安装目录。

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

支付渠道需要稳定的回调地址：在 `payments.json` 显式填写各渠道的 `notify_url`、`return_url`，或在 `.env` 设置 `TFP_ORIGIN=https://你的域名` 以生成回调地址。然后设置 `TFP_PAYMENTS_FILE=/config/payments.json`，再运行 `docker compose up -d`。此项只在接入商户支付时需要，容器安装与登录无需配置域名。不配置支付渠道也可正常登录、管理资源和使用钱包/套餐相关的已有数据；商户实付需另行验收。

## 离线安装和升级

从同一 Release 下载对应架构的 `traffic-forwarding-panel_0.1.20_docker_amd64.tar.gz`（ARM64 为 `docker_arm64`）、`compose.yaml`、`install-docker.sh` 和 `docker-SHA256SUMS`，放到一个目录。服务器已有 Docker/Compose 时不需要访问镜像仓库：

```sh
sudo bash install-docker.sh --bundle /path/to/downloads
```

`--bundle` 对已有安装同样会执行自动升级。备份保存在安装目录旁，例如 `/opt/traffic-forwarding-panel.backup-日期-随机后缀/installation.tar`，目录仅 root 可读，包含停机后的数据库、`.env`、Compose 文件和配置。升级成功后请按自己的备份保留策略保管或清理，脚本不会删除这些备份。

下载或校验失败发生在停机前；备份失败会尝试重新启动原容器。新程序开始运行后若健康或版本验证失败，脚本会返回非零状态并输出备份路径，不会自动降级可能已迁移的数据库。先查看 `docker compose logs panel` 修复配置；确需回退时，停止面板，将当前安装目录另存，再在原路径完整恢复 `installation.tar`，使用备份内的 `.env` 和旧镜像启动。不要仅切换旧镜像运行升级后的数据库。

自动备份面向本安装器默认的 SQLite `data/panel.db` 和安装目录内的绑定挂载。自定义镜像、外部数据库、目录外数据挂载需手动备份并升级：校验 Docker 包 → `docker load` → 修改 `.env` 的 `TFP_IMAGE` → `docker compose up -d --wait`。保留数据和配置，勿重新初始化管理员。

镜像只部署面板服务；Agent 仍需安装在真实入口/出口节点。镜像已经把 `/agent` 放在 `/panel` 旁边，面板因此会从自身可执行文件所在目录发布 Agent，控制台「设备组 → 接入设备」生成的命令可直接在目标设备上执行。要给与面板不同架构的设备接入，把 `agent-linux-<arch>` 放进一个目录并把 `TFP_AGENT_DIR` 指向它。此处的 HTTPS 反代测试不替代真实 1Panel 安装、Linux 跨机转发、容量与商户实付验收。


v0.1.0 在空域名提示处退出时尚未创建安装配置和数据库，直接重新下载 v0.1.1 脚本执行即可。已经成功安装的旧版需按上述升级步骤导入新镜像、更新 Compose 配置；保留 data/ 和管理员账号，按需将 TFP_ORIGIN 留空。
