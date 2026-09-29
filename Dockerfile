# ---- 构建阶段 ----
FROM golang:1.22-alpine AS builder

WORKDIR /src

# 先装依赖，利用层缓存
COPY go.mod go.sum ./
RUN go mod download

# 拷源码（含内嵌迁移 SQL、openapi.yaml、swagger-ui 静态资源）
COPY . .

ARG VERSION=dev
RUN CGO_ENABLED=0 GOOS=linux go build \
    -ldflags="-s -w -X main.version=${VERSION}" \
    -o /out/coffee-server ./cmd/server

# ---- 运行阶段 ----
FROM alpine:3.19

RUN apk add --no-cache ca-certificates wget tzdata && \
    adduser -D -u 10001 appuser
ENV TZ=Asia/Shanghai

COPY --from=builder /out/coffee-server /usr/local/bin/coffee-server

USER appuser
EXPOSE 8080

ENTRYPOINT ["/usr/local/bin/coffee-server"]
