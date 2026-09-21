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
LABEL org.opencontainers.image.title="Traffic Forwarding Panel" \
      org.opencontainers.image.source="https://github.com/kexue-aihao/Traffic-Forwarding-Panel" \
      org.opencontainers.image.version=$VERSION \
      org.opencontainers.image.revision=$REVISION
COPY --from=build /panel /panel
COPY --from=build /agent /agent
COPY --from=build --chown=65532:65532 /empty-data /data
USER 65532:65532
WORKDIR /data
ENV TFP_ADDR=0.0.0.0:8080 TFP_DSN=/data/panel.db
EXPOSE 8080
HEALTHCHECK --interval=15s --timeout=6s --start-period=20s --retries=3 CMD ["/panel", "-healthcheck"]
ENTRYPOINT ["/panel"]
