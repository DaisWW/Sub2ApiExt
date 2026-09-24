ARG GO_IMAGE=golang:1.23-alpine
ARG RUNTIME_IMAGE=alpine:3.22
FROM ${GO_IMAGE} AS build

WORKDIR /src
COPY go.mod main.go ./
RUN CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' -o /out/policy-gateway .

FROM ${RUNTIME_IMAGE}

RUN addgroup -S policy && adduser -S -G policy policy
COPY --from=build /out/policy-gateway /usr/local/bin/policy-gateway
USER policy
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/policy-gateway"]
