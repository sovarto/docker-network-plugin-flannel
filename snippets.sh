docker plugin install sovarto/docker-network-plugin-flannel --alias flannel --grant-all-permissions --disable && \

ssh root@188.245.202.183
ssh root@116.203.53.199
ssh root@157.90.157.1

docker plugin disable --force flannel:latest && docker plugin upgrade flannel:latest --grant-all-permissions
docker plugin set flannel:latest ETCD_PREFIX=/flannel/ && \
docker plugin set flannel:latest ETCD_ENDPOINTS=172.16.0.2:2379 && \
docker plugin set flannel:latest DEFAULT_FLANNEL_OPTIONS="-iface=enp7s0" && \
docker plugin set flannel:latest AVAILABLE_SUBNETS="192.168.32.0/19,192.168.64.0/19,192.168.96.0/19,192.168.128.0/19,192.168.160.0/19,192.168.192.0/19" && \
docker plugin enable flannel:latest

docker plugin disable --force flannel:dev || true && docker plugin upgrade flannel:dev --grant-all-permissions && docker plugin enable flannel:dev

docker network rm fweb
docker network create --attachable=true --driver=flannel:dev --ipam-driver=flannel:dev --ipam-opts=id=fweb123 fweb

docker service update --network-rm fweb whoami
docker service update --network-add fweb whoami

journalctl -u docker.service --since "5m ago"
journalctl -u docker.service | grep plugin=

docker run --rm -e ETCDCTL_API=3 --net=host quay.io/coreos/etcd etcdctl get /flannel --prefix --keys-only
docker run --rm -e ETCDCTL_API=3 --net=host quay.io/coreos/etcd etcdctl --endpoints=http://172.16.0.2:2379,http://172.16.0.3:2379,http://172.16.0.4:2379 get /flannel --prefix --keys-only
docker run --rm -e ETCDCTL_API=3 --net=host quay.io/coreos/etcd etcdctl del /flannel --prefix

# Add Docker's official GPG key:
apt-get update
apt-get install ca-certificates curl
install -m 0755 -d /etc/apt/keyrings
curl -fsSL https://download.docker.com/linux/ubuntu/gpg -o /etc/apt/keyrings/docker.asc
chmod a+r /etc/apt/keyrings/docker.asc

# Add the repository to Apt sources:
echo \
  "deb [arch=$(dpkg --print-architecture) signed-by=/etc/apt/keyrings/docker.asc] https://download.docker.com/linux/ubuntu \
  $(. /etc/os-release && echo "$VERSION_CODENAME") stable" | \
  tee /etc/apt/sources.list.d/docker.list > /dev/null
apt-get update

docker run --rm --name=etcd-1 -e ETCD_INITIAL_CLUSTER_TOKEN=XgPi6fld0vQ6oikbcvyB -e ETCD_LISTEN_PEER_URLS=http://0.0.0.0:2380 -e ETCD_INITIAL_CLUSTER=etcd-1=http://172.16.0.2:2380,etcd-2=http://172.16.0.3:2380,etcd-3=http://172.16.0.4:2380 -e ETCD_LISTEN_CLIENT_URLS=http://0.0.0.0:2379 -e ALLOW_NONE_AUTHENTICATION=yes -e ETCD_NAME=etcd-1 -e ETCD_ADVERTISE_CLIENT_URLS=http://172.16.0.2:2379 -e ETCD_DATA_DIR=/etcd-data -e ETCD_INITIAL_CLUSTER_STATE=new -e ETCD_INITIAL_ADVERTISE_PEER_URLS=http://172.16.0.2:2380 -v /etc/etcd/data:/etcd-data quay.io/coreos/etcd etcdctl
                    "Image": "bitnami/etcd:3.5.16@sha256:c1419aec942eae324576cc4ff6c7af20527c8b2e1d25d32144636d8b61dfd986",
                    "Hostname": "etcd-1",
                    "Env": [
-v /etc/etcd/data:/etcd-data
                    "Mounts": [
                        {
                            "Type": "bind",
                            "Source": "/etc/etcd/data",
                            "Target": "/etcd-data"
                        }
                    ],
