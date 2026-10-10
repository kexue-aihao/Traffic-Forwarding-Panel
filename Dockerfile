# syntax=docker/dockerfile:1
FROM --platform=$BUILDPLATFORM golang:1.26.9@sha256:f1f0bcc2c524a3ced375fcb4d1ecb7aa371aa7070e112599aaca45cc02d0101b AS build
ARG TARGETOS
ARG TARGETARCH
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN version="$(cat VERSION)" && \
    CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -ldflags="-s -w -X main.Version=$version" -o /panel ./cmd/panel && \
    CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -ldflags="-s -w -X github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/agent.Version=$version" -o /agent ./cmd/agent && \
    mkdir /agents && \
    CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags="-s -w -X github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/agent.Version=$version" -o /agents/agent-linux-amd64 ./cmd/agent && \
    CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -trimpath -ldflags="-s -w -X github.com/kexue-aihao/Traffic-Forwarding-Panel/internal/agent.Version=$version" -o /agents/agent-linux-arm64 ./cmd/agent && \
    go run ./cmd/agentrelease -dir /agents -version "$version" && \
    mkdir /empty-data
FROM gcr.io/distroless/static-debian12:nonroot@sha256:afa5c872c891853ca7fcf1f12c3edb23f7eeef36189728842dd51042ff57f7ab
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
COPY --from=build /agents/ /
COPY --from=build /src/docs/protocol-dependency-notices.txt /licenses/protocol-dependency-notices.txt
# 裸 agent 保留原生平台布局；/agents 已携带双架构产物和发布清单。
COPY --from=build /agent /agent-linux-${TARGETARCH}
COPY --from=build --chown=65532:65532 /empty-data /data
USER 65532:65532
WORKDIR /data
ENV TFP_ADDR=0.0.0.0:8080 TFP_DSN=/data/panel.db
EXPOSE 8080
HEALTHCHECK --interval=15s --timeout=6s --start-period=20s --retries=3 CMD ["/panel", "-healthcheck"]
ENTRYPOINT ["/panel"]
