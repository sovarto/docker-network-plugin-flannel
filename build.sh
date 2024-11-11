#!/bin/bash -e
error() {
  printf '\E[31m'; echo "$@"; printf '\E[0m'
}


export TAG=${1:-latest}
export ARCH=${2:-"$(uname -m)"}

build() {
    if [ $ARCH == "armv7l" ]
    then
        BPLATFORM="linux/arm/v7"
    else if [ $ARCH == "aarch64" ]
        then
            BPLATFORM="linux/arm64"
        else
            BPLATFORM="linux/amd64"
        fi
    fi
    if [ $ARCH == "x86_64" ]
    then
        docker plugin rm -f sovarto/$1:$TAG || true
    else
        docker plugin rm -f sovarto/$1-$ARCH:$TAG || true
    fi
    docker rmi -f rootfsimage || true
    docker buildx build --load --platform ${BPLATFORM} \
        --build-arg GO_VERSION=1.15.10 \
        --build-arg UBUNTU_VERSION=22.04 \
        -t rootfsimage .
    id=$(docker create rootfsimage true)
    rm -rf build/rootfs
    mkdir -p build/rootfs/flannel-env
    docker export "$id" | tar -x -C build/rootfs
    docker rm -vf "$id"
    cp config.json build
    if [ $ARCH == "x86_64" ]
    then
        docker plugin create sovarto/$1:$TAG build
        docker plugin push sovarto/$1:$TAG
    else
        docker plugin create sovarto/$1-$ARCH:$TAG build
        docker plugin push sovarto/$1-$ARCH:$TAG
    fi
}

build docker-network-plugin-flannel
