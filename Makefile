APP := trafficpanel
VERSION ?= dev

.PHONY: test build build-linux buildx docker-up docker-down

test:
	go test ./...

build:
	go build -trimpath -ldflags="-s -w -X main.version=$(VERSION)" -o bin/$(APP) ./cmd/trafficpanel

build-linux:
	mkdir -p dist
	GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o dist/$(APP)-linux-amd64 ./cmd/trafficpanel
	GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o dist/$(APP)-linux-arm64 ./cmd/trafficpanel

buildx:
	docker buildx build --platform linux/amd64,linux/arm64 -t trafficpanel:$(VERSION) .

docker-up:
	docker compose up -d --build

docker-down:
	docker compose down
