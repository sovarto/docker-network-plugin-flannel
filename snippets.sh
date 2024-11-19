wget https://github.com/flannel-io/flannel/releases/latest/download/flanneld-amd64 && chmod +x flanneld-amd64
docker run --rm -e ETCDCTL_API=3 --net=host quay.io/coreos/etcd etcdctl --endpoints=http://172.16.0.2:2379,http://172.16.0.3:2379,http://172.16.0.4:2379 put /manual-flannel/config '{ "Network": "10.200.0.0/16", "SubnetLen": 24, "Backend": {"Type": "vxlan"}}'
./flanneld-amd64 -iface=enp7s0 -etcd-endpoints=http://172.16.0.2:2379,http://172.16.0.3:2379,http://172.16.0.4:2379 -etcd-prefix=/manual-flannel &
bg 1
source /var/run/flannel/subnet.env
docker network create --attachable=true --subnet=${FLANNEL_SUBNET} -o "com.docker.network.driver.mtu"=${FLANNEL_MTU} manual-flannel

docker run --rm -it -d --network manual-flannel --name whoami traefik/whoami
docker inspect whoami

docker run --rm -it --network manual-flannel fedora

docker plugin install sovarto/docker-network-plugin-flannel --alias flannel --grant-all-permissions --disable && \

ssh root@188.245.202.183
ssh root@116.203.53.199
ssh root@157.90.157.1

ALIAS=flannel:test3; \
PREFIX=/flannel; \
VERSION=latest; \
docker plugin disable --force $ALIAS || true && docker plugin rm $ALIAS || true && \
docker plugin install sovarto/docker-network-plugin-flannel:$VERSION --alias $ALIAS --grant-all-permissions --disable && \
docker plugin set $ALIAS ETCD_PREFIX=$PREFIX && \
docker plugin set $ALIAS ETCD_ENDPOINTS=172.16.0.2:2379,172.16.0.3:2379,172.16.0.4:2379 && \
docker plugin set $ALIAS DEFAULT_FLANNEL_OPTIONS="-iface=enp7s0" && \
docker plugin set $ALIAS AVAILABLE_SUBNETS="10.1.0.0/16,10.10.0.0/16,10.50.0.0/16" && \
docker plugin set $ALIAS NETWORK_SUBNET_SIZE=18 && \
docker plugin set $ALIAS DEFAULT_HOST_SUBNET_SIZE=23 && \
docker plugin enable $ALIAS && \
docker plugin inspect $ALIAS --format "{{.ID}}"

docker plugin disable --force flannel:dev || true && docker plugin upgrade flannel:dev --grant-all-permissions && docker plugin enable flannel:dev

docker network rm fweb
docker network create --attachable=true --driver=flannel:dev --ipam-driver=flannel:dev --ipam-opt=id=$(uuidgen) fweb

docker service update --network-rm fweb whoami
docker service update --network-add fweb whoami

journalctl -u docker.service --since "5m ago" | grep plugin=
journalctl -u docker.service | grep plugin=

docker run --rm -e ETCDCTL_API=3 --net=host quay.io/coreos/etcd etcdctl get /flannel --prefix --keys-only
docker run --rm -e ETCDCTL_API=3 --net=host quay.io/coreos/etcd etcdctl --endpoints=http://172.16.0.2:2379,http://172.16.0.3:2379,http://172.16.0.4:2379 get /flannel --prefix --keys-only
docker run --rm -e ETCDCTL_API=3 --net=host quay.io/coreos/etcd etcdctl del /flannel --prefix

curl --unix-socket /var/run/docker/plugins/$(docker plugin inspect flannel:dev --format "{{.ID}}")/flannel-np.sock http://x/Plugin.Activate

docker network rm f1 && docker plugin disable --force flannel:dev || true && docker plugin upgrade flannel:dev --grant-all-permissions && docker plugin enable flannel:dev && docker network create --attachable=true --driver=flannel:dev --ipam-driver=flannel:dev --ipam-opt=id=f1_123 f1 && journalctl -u docker.service --since "1m ago" --follow | grep plugin=
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
