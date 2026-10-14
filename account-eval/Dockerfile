ARG GOLANG_IMAGE=docker.m.daocloud.io/library/golang:1.23-alpine
ARG ALPINE_IMAGE=docker.m.daocloud.io/library/alpine:3.21
FROM ${GOLANG_IMAGE} AS build
ARG GOPROXY=https://goproxy.cn,direct
ARG GOSUMDB=sum.golang.google.cn
ENV GOPROXY=${GOPROXY} GOSUMDB=${GOSUMDB}
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . ./
RUN CGO_ENABLED=0 go test ./... && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/account-eval .
FROM ${ALPINE_IMAGE}
RUN addgroup -S eval && adduser -S -G eval eval && mkdir /data && chown eval:eval /data
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=build /out/account-eval /app/account-eval
USER eval
EXPOSE 8091
ENTRYPOINT ["/app/account-eval"]
