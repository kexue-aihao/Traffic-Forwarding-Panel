# 1Panel Deployment / 1Panel 部署

## 中文

当前 GitHub Actions 已在 `master` 推送后自动构建并推送多架构 Docker 镜像。1Panel 中可以直接拉取该镜像并通过容器编排启动，再用网站反向代理完成访问。

默认镜像：

```text
ghcr.io/kexue-aihao/traffic-forwarding-panel:latest
```

也可以使用提交标签：

```text
ghcr.io/kexue-aihao/traffic-forwarding-panel:sha-<commit>
```

### 方式一：容器编排部署

1. 登录 1Panel。
2. 打开「容器」->「镜像」。
3. 点击「拉取镜像」。
4. 镜像名填写：

```text
ghcr.io/kexue-aihao/traffic-forwarding-panel:latest
```

5. 等待镜像拉取完成。
6. 打开「容器」->「编排」。
7. 新建编排，名称建议填写 `trafficpanel`。
8. 将本目录的 `docker-compose.yml` 内容复制到编排内容中。
9. 在编排环境变量中填写：

```sh
PANEL_IMAGE=ghcr.io/kexue-aihao/traffic-forwarding-panel:latest
PANEL_HTTP_PORT=8080
PANEL_BASE_URL=https://panel.example.com
PANEL_MASTER_SECRET=replace-with-long-random-secret
PANEL_ADMIN_USER=admin
PANEL_ADMIN_PASSWORD=replace-me

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

10. 启动编排。
11. 打开「容器」->「容器」，确认 `trafficpanel-trafficpanel` 和 `trafficpanel-mysql` 都处于运行状态。
12. 浏览器访问 `http://服务器IP:8080` 验证服务。

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

GitHub Actions now builds and pushes a multi-architecture Docker image after every `master` push. In 1Panel, you can pull the image, start it with Compose, and expose it through a website reverse proxy.

Default image:

```text
ghcr.io/kexue-aihao/traffic-forwarding-panel:latest
```

Commit tag example:

```text
ghcr.io/kexue-aihao/traffic-forwarding-panel:sha-<commit>
```

### Compose Deployment

1. Sign in to 1Panel.
2. Open `Container` -> `Images`.
3. Click `Pull Image`.
4. Use this image:

```text
ghcr.io/kexue-aihao/traffic-forwarding-panel:latest
```

5. Wait for the image pull to finish.
6. Open `Container` -> `Compose`.
7. Create a Compose project named `trafficpanel`.
8. Paste the content of this directory's `docker-compose.yml`.
9. Fill the environment variables listed in the Chinese section above.
10. Start the Compose project.
11. Confirm both `trafficpanel-trafficpanel` and `trafficpanel-mysql` are running.
12. Open `http://SERVER_IP:8080` for a quick check.

### Website Reverse Proxy

1. Open `Websites`.
2. Create a reverse proxy website.
3. Set the domain, for example `panel.example.com`.
4. Set the proxy target:

```text
http://127.0.0.1:8080
```

5. Save the website.
6. Enable or bind an SSL certificate.
7. Make sure `PANEL_BASE_URL` matches the public URL, for example `https://panel.example.com`.

### Upgrade

After pushing new code to `master`, GitHub Actions rebuilds the `latest` image. To upgrade in 1Panel:

1. Pull `ghcr.io/kexue-aihao/traffic-forwarding-panel:latest` again.
2. Recreate or restart the `trafficpanel` Compose project.
3. Verify the website.
