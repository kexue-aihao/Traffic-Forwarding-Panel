FROM --platform=$BUILDPLATFORM golang:1.26-alpine AS build

ARG TARGETOS
ARG TARGETARCH
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH:-amd64} go build -trimpath -ldflags="-s -w" -o /out/trafficpanel ./cmd/trafficpanel

FROM alpine:3.22

RUN addgroup -S trafficpanel && adduser -S -G trafficpanel trafficpanel
WORKDIR /app
COPY --from=build /out/trafficpanel /usr/local/bin/trafficpanel
USER trafficpanel
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/trafficpanel"]

