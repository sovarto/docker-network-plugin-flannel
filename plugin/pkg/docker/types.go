package docker

import (
	"github.com/sovarto/FlannelNetworkPlugin/pkg/common"
	"net"
)

type ContainerInfo struct {
	common.ContainerInfo
	IpamIPs map[string]net.IP `json:"IpamIPs"` // networkID -> IP
}

type ServiceInfo struct {
	ID           string            `json:"ServiceID"`
	Name         string            `json:"ServiceName"`
	EndpointMode string            `json:"EndpointMode"` // dnsrr or vip
	Networks     []string          `json:"Networks"`     // networkID
	IpamVIPs     map[string]net.IP `json:"IpamVIPs"`     // networkID -> VIP
}

func (c ContainerInfo) Equals(other common.Equaler) bool {
	o, ok := other.(ContainerInfo)
	if !ok {
		return false
	}
	if c.ID != o.ID || c.Name != o.Name || c.ServiceID != o.ServiceID || c.ServiceName != o.ServiceName || c.SandboxKey != o.SandboxKey {
		return false
	}
	if !common.CompareIPMaps(c.IPs, o.IPs) {
		return false
	}
	if !common.CompareIPMaps(c.IpamIPs, o.IpamIPs) {
		return false
	}
	if !common.CompareStringMaps(c.Endpoints, o.Endpoints) {
		return false
	}
	if !common.CompareStringArrayMaps(c.DNSNames, o.DNSNames) {
		return false
	}

	return true
}

type ContainerInfo struct {
	ID          string              `json:"ContainerID"`
	Name        string              `json:"ContainerName"`
	ServiceID   string              `json:"ServiceID"`
	ServiceName string              `json:"ServiceName"`
	SandboxKey  string              `json:"SandboxKey"`
	IPs         map[string]net.IP   `json:"IPs"`       // networkID -> IP
	DNSNames    map[string][]string `json:"DNSNames"`  // networkID -> DNS names
	Endpoints   map[string]string   `json:"Endpoints"` // networkID -> endpoint ID
}

func (c ServiceInfo) Equals(other common.Equaler) bool {
	o, ok := other.(ServiceInfo)
	if !ok {
		return false
	}
	if c.ID != o.ID || c.Name != o.Name {
		return false
	}
	if !common.CompareIPMaps(c.IpamVIPs, o.IpamVIPs) {
		return false
	}

	return true
}
