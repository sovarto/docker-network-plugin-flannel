FROM --platform=${TARGETPLATFORM:-linux/amd64} golang:1.22-alpine AS builder

RUN apk update && apk add git

COPY . /src/

RUN cd /src/ && CGO_ENABLED=0 GOOS=linux go build -o /network-plugin-flannel

FROM alpine:3.18

RUN apk add -U --no-cache \
    iptables \
    ip6tables \
    nftables \
    dpkg \
    curl \
    rsyslog \
    tini

RUN mkdir -p /var/lib/rsyslog

WORKDIR /app

RUN update-alternatives --install /sbin/iptables iptables /sbin/iptables-legacy 1 && \
    update-alternatives --install /sbin/iptables iptables /sbin/iptables-nft 2 && \
    update-alternatives --install /sbin/ip6tables ip6tables /sbin/ip6tables-legacy 1 && \
    update-alternatives --install /sbin/ip6tables ip6tables /sbin/ip6tables-nft 2

COPY --from=builder /network-plugin-flannel /


