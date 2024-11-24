package common

import "net"

type ContainerInfo struct {
	ID       string
	Name     string
	Networks map[string]net.IP // networkID to container IP
}

type ServiceInfo struct {
	ID         string
	Name       string
	Networks   map[string]net.IP // networkID to VIP
	Containers []ContainerInfo
}

type NetworkInfo struct {
	ID           string
	MTU          int
	Network      net.IPNet
	HostSubnet   net.IPNet
	LocalGateway net.IP
}
