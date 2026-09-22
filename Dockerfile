# syntax=docker/dockerfile:1
FROM --platform=$BUILDPLATFORM golang:1.26 AS build
ARG TARGETOS
ARG TARGETARCH
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN version="$(cat VERSION)" && \
    CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -ldflags="-s -w -X main.Version=$version" -o /panel ./cmd/panel && \
    CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -ldflags="-s -w -X github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/agent.Version=$version" -o /agent ./cmd/agent && \
    mkdir /empty-data
FROM gcr.io/distroless/static-debian12:nonroot
ARG VERSION
ARG REVISION
# 内置构建参数必须逐阶段重新声明，否则下面的平台后缀名会解析成空串。
ARG TARGETARCH
LABEL org.opencontainers.image.title="Traffic Forwarding Panel" \
      org.opencontainers.image.source="https://github.com/kexue-aihao/Traffic-Forwarding-Panel" \
      org.opencontainers.image.version=$VERSION \
      org.opencontainers.image.revision=$REVISION
COPY --from=build /panel /panel
COPY --from=build /agent /agent
# 再放一份带平台后缀的副本：面板默认从这个目录（可执行文件所在目录）发布
# Agent，裸 agent 只服务面板自身平台。要给别的架构的设备接入，把对应产物
# 放进挂载目录并设置 TFP_AGENT_DIR。
COPY --from=build /agent /agent-linux-${TARGETARCH}
COPY --from=build --chown=65532:65532 /empty-data /data
USER 65532:65532
WORKDIR /data
ENV TFP_ADDR=0.0.0.0:8080 TFP_DSN=/data/panel.db
EXPOSE 8080
HEALTHCHECK --interval=15s --timeout=6s --start-period=20s --retries=3 CMD ["/panel", "-healthcheck"]
ENTRYPOINT ["/panel"]
