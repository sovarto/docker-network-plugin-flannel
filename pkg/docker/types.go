package docker

import (
	"github.com/sovarto/FlannelNetworkPlugin/pkg/common"
	"net"
)

type ContainerInfo struct {
	ID          string            `json:"ContainerID"`
	Name        string            `json:"ContainerName"`
	ServiceID   string            `json:"ServiceID"`
	ServiceName string            `json:"ServiceName"`
	SandboxKey  string            `json:"SandboxKey"`
	IPs         map[string]net.IP `json:"IPs"`     // networkID -> IP
	IpamIPs     map[string]net.IP `json:"IpamIPs"` // networkID -> IP
}

type ServiceInfo struct {
	ID       string            `json:"ServiceID"`
	Name     string            `json:"ServiceName"`
	IpamVIPs map[string]net.IP `json:"IpamVIPs"` // networkID -> VIP
}

type NetworkInfo struct {
	DockerID  string `json:"DockerID"`
	FlannelID string `json:"FlannelID"`
	Name      string `json:"Name"`
}

func (c ContainerInfo) Equals(other common.Equaler) bool {
	o, ok := other.(ContainerInfo)
	if !ok {
		return false
	}
	if c.ID != o.ID || c.Name != o.Name || c.ServiceID != o.ServiceID || c.ServiceName != o.ServiceName {
		return false
	}
	if !common.CompareIPMaps(c.IPs, o.IPs) {
		return false
	}
	if !common.CompareIPMaps(c.IpamIPs, o.IpamIPs) {
		return false
	}

	return true
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

func (c NetworkInfo) Equals(other common.Equaler) bool {
	o, ok := other.(NetworkInfo)
	if !ok {
		return false
	}
	if c.FlannelID != o.FlannelID || c.Name != o.Name {
		return false
	}

	return true
}
