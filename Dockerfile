FROM --platform=$TARGETPLATFORM golang:1.26-alpine AS build

WORKDIR /src
RUN apk add --no-cache gcc musl-dev
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=1 go build -trimpath -ldflags="-s -w" -o /out/trafficpanel ./cmd/trafficpanel

FROM alpine:3.22

RUN addgroup -S trafficpanel && adduser -S -G trafficpanel trafficpanel && apk add --no-cache sqlite-libs wget
WORKDIR /app
RUN mkdir -p /data && chown -R trafficpanel:trafficpanel /data
COPY --from=build /out/trafficpanel /usr/local/bin/trafficpanel
USER trafficpanel
VOLUME ["/data"]
EXPOSE 8080
HEALTHCHECK --interval=30s --timeout=3s --start-period=10s --retries=3 CMD wget -qO- http://127.0.0.1:8080/readyz >/dev/null || exit 1
ENTRYPOINT ["/usr/local/bin/trafficpanel"]

