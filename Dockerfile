# syntax=docker/dockerfile:1.4
FROM --platform=${TARGETPLATFORM:-linux/amd64} golang:1.22-alpine AS builder

RUN apk update && apk add git

COPY go.mod /src/
COPY go.sum /src/
RUN --mount=type=cache,target=/go/pkg/mod cd /src/ && go mod download

COPY . /src/
RUN --mount=type=cache,target=/root/.cache/go-build cd /src/ && CGO_ENABLED=0 GOOS=linux go build -o /network-plugin-flannel

FROM alpine:3.18

RUN apk add -U --no-cache \
    iptables \
#    ip6tables \
#    nftables \
    dpkg \
    curl \
    rsyslog

RUN mkdir -p /var/lib/rsyslog

WORKDIR /app

RUN update-alternatives --install /sbin/iptables iptables /sbin/iptables-legacy 1 &&
#    update-alternatives --install /sbin/iptables iptables /sbin/iptables-nft 2 && \
#    update-alternatives --install /sbin/ip6tables ip6tables /sbin/ip6tables-legacy 1 && \
#    update-alternatives --install /sbin/ip6tables ip6tables /sbin/ip6tables-nft 2

RUN wget https://github.com/flannel-io/flannel/releases/latest/download/flanneld-amd64 && \
    mv flanneld-amd64 /flanneld && \
    chmod +x /flanneld

COPY --from=builder /network-plugin-flannel /


