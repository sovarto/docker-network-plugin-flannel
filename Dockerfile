ARG UBUNTU_VERSION
FROM --platform=${TARGETPLATFORM:-linux/amd64} ubuntu:${UBUNTU_VERSION:-20.04} AS base

ENV DEBIAN_FRONTEND="noninteractive"

RUN apt update && \
    apt install -y curl rsyslog tini && \
    mkdir -p /var/lib/rsyslog && \
    apt clean && rm -rf /var/lib/apt/lists/*

FROM --platform=${TARGETPLATFORM:-linux/amd64} golang:1.22-alpine AS dev

RUN apk update && apk add git

COPY . /src/

RUN cd /src/ && CGO_ENABLED=0 GOOS=linux go build -o /network-plugin-flannel

FROM base

COPY --from=dev /network-plugin-flannel /
