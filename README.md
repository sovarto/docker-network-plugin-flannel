# Introduction

This plugin is a complete replacement of the Docker swarm overlay network. It fully supports DNS
resolution for Docker Swarm services, both DNSRR and VIP. It also supports DNS resolution for
containers.  
It supports more than the recommended 255 containers for an overlay network.

With regards to the actual network traffic, this plugin is just the control plane. The data plane is
standard Linux VxLAN, administered by Flannel.  
With regards to the Service VIPs, this plugin is also just the control plane. The data plane is
standard
Linux IPVS and iptables.
With regards to the DNS resolution of services and containers, this plugin is the data plane: Very
similar to what Docker does, this plugin runs a DNS server per container and injects it into the
container, completely replacing the standard Docker DNS.

# Setup

## Install etcd

The plugin needs etcd to store its state.  
Running etcd as a normal docker swarm service inside the same cluster will lead to issues when the
node is restarted, because the network plugin will fail to start and become disabled if it can't
reach etcd.

Instead, run etcd either outside of the cluster, or run it as a normal Linux service on some of the
nodes of the cluster.

## Install the plugin

On each node, install the plugin:

```
docker plugin install --alias flannel-np:1.0.0 --disable --grant-all-permissions sovarto/docker-network-plugin-flannel
docker plugin set flannel-np:1.0.0 ETCD_PREFIX=/flannel/',
docker plugin set flannel-np:1.0.0 ETCD_ENDPOINTS=172.16.0.2:2379,172.16.0.3:2379,172.16.0.4:2379',
docker plugin set flannel-np:1.0.0 DEFAULT_FLANNEL_OPTIONS="-iface=enp7s0"',
docker plugin set flannel-np:1.0.0 AVAILABLE_SUBNETS=10.1.0.0/16,10.10.0.0/16,10.20.0.0/16,10.30.0.0/16,10.50.0.0/16',
docker plugin set flannel-np:1.0.0 NETWORK_SUBNET_SIZE=18',
docker plugin set flannel-np:1.0.0 DEFAULT_HOST_SUBNET_SIZE=23',
docker plugin set flannel-np:1.0.0 IS_HOOK_AVAILABLE=true',
docker plugin set flannel-np:1.0.0 DNS_DOCKER_COMPATIBILITY_MODE=false',
docker plugin enable flannel-np:1.0.0'

```

Adjust the configuration options to match your setup.
Especially, set `ETCD_ENDPOINTS` to the proper configuration of your etcd setup.  
Set `DEFAULT_FLANNEL_OPTIONS` to use the network interface that connects the swarm nodes.  
Adjust `AVAILABLE_SUBNETS`, `NETWORK_SUBNET_SIZE` and `DEFAULT_HOST_SUBNET_SIZE` to your needs.
Set `IS_HOOK_AVAILABLE` to `false` if you don't install the hook (see next section)

| Name                          | Description                                                                                                                                                                                                                                |
|-------------------------------|--------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|
| ETCD_PREFIX                   | The prefix for all state inside etcd. Usually can be left as is                                                                                                                                                                            |
| ETCD_ENDPOINTS                | The etcd endpoints the plugin should use                                                                                                                                                                                                   |
| DEFAULT_FLANNEL_OPTIONS       | This supports all Flannel options, but -iface is needed and needs to be set to the network interface name that connects the swarm nodes.                                                                                                   |
| AVAILABLE_SUBNETS             | These are the subnets that are available for Flannel. Their size needs to be at least as big as `NETWORK_SUBNET_SIZE`. This setting along with `NETWORK_SUBNET_SIZE` determines the total number of supported networks.                    |
| NETWORK_SUBNET_SIZE           | The size of the subnet from which each node will choose its subnet. The relationship between this setting and `DEFAULT_HOST_SUBNET_SIZE` determines the number of supported nodes in the cluster.                                          |
| DEFAULT_HOST_SUBNET_SIZE      | The default size of the subnet each host reserves for the IP addresses on itself for a particular network. This size determines the number of IP addresses per node, and thus, the number of containers + services the network can support |
| IS_HOOK_AVAILABLE             | Whether our hook is available or not (see next section)                                                                                                                                                                                    |
| DNS_DOCKER_COMPATIBILITY_MODE | The default docker DNS has [some quirks](https://github.com/sovarto/FlannelNetworkPlugin/blob/main/plugin/pkg/dns/resolver.go#L46) when resolving names. Set to true if, for some reason, you depend on them.                              |

## Install hook (optional but strongly recommended)

The docker networking plugin API is quite lacking in terms of flexibility. It doesn't support the
use case of each node having its own subnet. To still support proper DNS resolution, especially for
service VIPs, we run our own DNS server per container, as mentioned above. To properly inject it
into the container, we need the OCI runtime hook `createContainer` which isn't supported by a
standard docker setup.

To fix this, use [oci-add-hooks](https://github.com/sovarto/oci-add-hooks/). Download the released
binary to `/etc/flannel-np/oci-add-hooks`.  
Additionally, we need the actual hook to be executed. Get the file `flannel-network-plugin-hook`
from the releases of this repo and place it at `/etc/flannel-np/create-container-hook`.  
Then make both files executable.    
Add the following file:
`/etc/flannel-np/hook-config.json`

```
{"hooks":{"createContainer":[{"path":"/etc/flannel-np/create-container-hook"}]}}
```

You can use this script to perform the aforementioned steps:

```
wget https://github.com/sovarto/FlannelNetworkPlugin/releases/latest/download/flannel-network-plugin-hook -O /etc/flannel-np/create-container-hook
wget https://github.com/sovarto/oci-add-hooks/releases/latest/download/oci-add-hooks -O /etc/flannel-np/oci-add-hooks
chmod 0555 /etc/flannel-np/create-container-hook
chmod 0555 /etc/flannel-np/oci-add-hooks
echo '{"hooks":{"createContainer":[{"path":"/etc/flannel-np/create-container-hook"}]}}' > /etc/flannel-np/hook-config.json
chmod 0444 /etc/flannel-np/hook-config.json
```

Finally, add the following configuration to `/etc/docker/daemon.json`:

```
"runtimes": {
  "runc-with-hooks": {
    "path": "/etc/flannel-np/oci-add-hooks",
    "runtimeArgs": [
      "--hook-config-path",
      "/etc/flannel-np/hook-config.json",
      "--runtime-path",
      "/usr/sbin/runc"
    ]
  }
},
"default-runtime": "runc-with-hooks"
```

Restart the docker daemon: `systemctl restart docker`

This whole setup with the hook is optional, the network plugin will also work without it. However,
without the hook, there is a race condition that WILL regularly occur during startup of a container:
The container is already running, but our DNS server is not yet injected.  
If a container performs a DNS lookup for another service right at startup, this WILL cause issues
sooner rather than later, because the DNS lookup will resolve the service name to the VIP of the
service as seen from one of the manager nodes.

# Create a network

Because of limitations in the network plugin API of docker, we need to assign a unique ID to each
network, in addition to the one created by docker.  
Additionally, we need to specify our plugin as both the driver as well as the IPAM driver:

    docker network create --attachable=true --driver=flannel-np:1.0.0 --ipam-driver=flannel-np:1.0.0 \
      --ipam-opt=flannel-id=$(uuidgen) <network name>

# Design decision

The data in Docker trumps the data in etcd which trumps the data in memory.

# Needed IP ranges

Note:

- In Flannel, every host has its own subnet
- On each node, we will start one Flannel process per Docker network
- To decrease latency, each Docker Swarm Service will get one load balancer per host and network,
  similar to what docker swarm does

This will result in a higher number of needed IP ranges as opposed to a standard docker swarm setup.

Example:
Assuming we have 192.168.0.0/16 with 65,536 IP addresses total available for all networks.
Setting NETWORK_SUBNET_SIZE to 20 allows us to create 16 networks in total, each with 4096 IPs
available.
Setting DEFAULT_HOST_SUBNET_SIZE to 25 allows us to have 32 hosts in total, each with 128 IPs
available.
Assuming we have 100 Docker Swarm services with endpoint mode VIP connected to each of the networks,
then this would leave us with 28 IPs per host for the actual container IPs.

The clear recommendation here is to not use the 192.168.0.0/16 subnet but rather subnets from the
10.0.0.0/8 range.
