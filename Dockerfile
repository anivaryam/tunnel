FROM golang:1.23-alpine AS builder

WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -ldflags="-s -w -X main.version=${VERSION}" -o /tunnel-server ./cmd/server

FROM alpine:3.21
RUN apk add --no-cache ca-certificates wget && \
    addgroup -S tunnel && adduser -S -G tunnel -u 65532 tunnel
COPY --from=builder /tunnel-server /tunnel-server
USER 65532:65532
EXPOSE 8080 9000 9001
HEALTHCHECK --interval=30s --timeout=3s --start-period=5s \
    CMD wget -q -O- http://127.0.0.1:8080/health || exit 1
ENTRYPOINT ["/tunnel-server"]
