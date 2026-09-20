FROM golang:1.26 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -o /panel ./cmd/panel && CGO_ENABLED=0 go build -trimpath -o /agent ./cmd/agent
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /panel /panel
COPY --from=build /agent /agent
WORKDIR /data
ENV TFP_ADDR=0.0.0.0:8080 TFP_DSN=/data/panel.db
EXPOSE 8080
ENTRYPOINT ["/panel"]
