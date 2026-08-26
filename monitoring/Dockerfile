ARG GOLANG_IMAGE=docker.m.daocloud.io/library/golang:1.23-alpine
ARG ALPINE_IMAGE=docker.m.daocloud.io/library/alpine:3.21
ARG APK_MIRROR=https://mirrors.aliyun.com/alpine

FROM ${GOLANG_IMAGE} AS build

ARG GOPROXY=https://goproxy.cn,direct
ARG GOSUMDB=sum.golang.google.cn
ENV GOPROXY=${GOPROXY}
ENV GOSUMDB=${GOSUMDB}

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . ./
RUN CGO_ENABLED=0 GOOS=linux go test ./... && \
    CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/sub2api-monitor ./cmd/server

FROM ${ALPINE_IMAGE}
ARG APK_MIRROR
RUN sed -i "s#https://dl-cdn.alpinelinux.org/alpine#${APK_MIRROR}#g" /etc/apk/repositories && \
    apk add --no-cache tzdata && \
    addgroup -S monitor && adduser -S -G monitor monitor
WORKDIR /app
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=build /out/sub2api-monitor /app/sub2api-monitor
USER monitor
EXPOSE 8090
HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 \
  CMD wget -q -T 4 -O /dev/null http://127.0.0.1:8090/healthz || exit 1
ENTRYPOINT ["/app/sub2api-monitor"]
