# syntax=docker/dockerfile:1.4
FROM --platform=${TARGETPLATFORM:-linux/amd64} golang:1.23-alpine AS builder

RUN apk update && apk add git

COPY go.mod /src/
COPY go.sum /src/
RUN --mount=type=cache,target=/go/pkg/mod cd /src/ && go mod download

COPY . /src/
RUN --mount=type=cache,target=/root/.cache/go-build cd /src/ && CGO_ENABLED=0 GOOS=linux go build -o /network-plugin-flannel

FROM alpine:3.19

RUN apk add -U --no-cache iptables

RUN wget https://github.com/flannel-io/flannel/releases/download/v0.26.3/flanneld-amd64 && \
    mv flanneld-amd64 /flanneld && \
    chmod +x /flanneld

COPY --from=builder /network-plugin-flannel /

