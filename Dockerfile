FROM --platform=$TARGETPLATFORM golang:1.26-alpine AS build

WORKDIR /src
RUN apk add --no-cache gcc musl-dev
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=1 go build -trimpath -ldflags="-s -w" -o /out/trafficpanel ./cmd/trafficpanel

FROM alpine:3.22

RUN addgroup -S trafficpanel && adduser -S -G trafficpanel trafficpanel && apk add --no-cache sqlite-libs
WORKDIR /app
RUN mkdir -p /data && chown -R trafficpanel:trafficpanel /data
COPY --from=build /out/trafficpanel /usr/local/bin/trafficpanel
USER trafficpanel
VOLUME ["/data"]
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/trafficpanel"]

