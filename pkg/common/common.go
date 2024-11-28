package common

import (
	"net"
	"strings"
)

type ContainerInfo struct {
	ID      string            `json:"ContainerID"`
	Name    string            `json:"ContainerName"`
	IPs     map[string]net.IP `json:"IPs"`     // -> networkID -> IP
	IpamIPs map[string]net.IP `json:"IpamIPs"` // -> networkID -> IP
}

type ServiceInfo struct {
	ID         string
	Name       string
	VIPs       map[string]net.IP        // networkID -> VIP
	Containers map[string]ContainerInfo // containerID -> info
}

type NetworkInfo struct {
	ID           string
	MTU          int
	Network      *net.IPNet
	HostSubnet   *net.IPNet
	LocalGateway net.IP
}

func SubnetToKey(subnet string) string {
	return strings.ReplaceAll(subnet, "/", "-")
}
