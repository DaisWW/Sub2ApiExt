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
    CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/priority-sync .

FROM ${RUNTIME_IMAGE}
ARG APK_MIRROR
RUN sed -i "s#https://dl-cdn.alpinelinux.org/alpine#${APK_MIRROR}#g" /etc/apk/repositories && \
    apk add --no-cache ca-certificates tzdata && \
    addgroup -S priority && adduser -S -G priority priority && \
    mkdir -p /app /data && chown -R priority:priority /app /data
COPY --from=builder /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=builder /out/priority-sync /app/priority-sync

USER priority
WORKDIR /app
ENTRYPOINT ["/app/priority-sync"]
