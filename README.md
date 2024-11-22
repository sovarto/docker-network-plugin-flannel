# Design Decisions
The data in Docker trumps the data in etcd which trumps the data in memory.

Note:

- In Flannel, every host has its own subnet
- We will start one Flannel process per Docker network
- To decrease latency, each Docker Swarm Service will get one load balancer per host

This severly limits the number of host and networks in the cluster, because we will need number of
networks x number of hosts subnets, where each of these subnets need to be able to provide an IP for
each Docker Swarm service that's connected to that network and an IP for each container, that's
running on that host and is connected to the network.

Example:
Assuming we have 192.168.0.0/16 with 65,536 IP addresses total available for all networks.
Setting NETWORK_SUBNET_SIZE to 20 allows us to create 16 networks in total, each with 4096 IPs
available.
Setting DEFAULT_HOST_SUBNET_SIZE to 25 allows us to have 32 hosts in total, each with 128 IPs
available.
Assuming we have 100 Docker Swarm services with endpoint mode VIP connected to each of the networks,
then this would leave us with 28 IPs per host for the actual container IPs.



