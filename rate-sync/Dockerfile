ARG GO_IMAGE=docker.m.daocloud.io/library/golang:1.23-alpine
ARG RUNTIME_IMAGE=docker.m.daocloud.io/library/alpine:3.21
ARG APK_MIRROR=https://mirrors.aliyun.com/alpine

FROM ${GO_IMAGE} AS builder

WORKDIR /src
ENV GOPROXY=https://goproxy.io,direct
COPY go.mod go.sum ./
RUN go mod download
COPY *.go ./
RUN CGO_ENABLED=0 GOOS=linux go test ./... && \
    CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/rate-sync .

FROM ${RUNTIME_IMAGE}
ARG APK_MIRROR
RUN sed -i "s#https://dl-cdn.alpinelinux.org/alpine#${APK_MIRROR}#g" /etc/apk/repositories && \
    apk add --no-cache ca-certificates tzdata && \
    addgroup -S rate-sync && adduser -S -G rate-sync rate-sync && \
    mkdir -p /app /data && chown -R rate-sync:rate-sync /app /data
COPY --from=builder /out/rate-sync /app/rate-sync

USER rate-sync
ENV CONFIG_PATH=/app/config.json
ENTRYPOINT ["/app/rate-sync"]
