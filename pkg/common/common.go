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
	VIPs       map[string]map[string]net.IP        // hostname -> networkID -> VIP
	IpamVIPs   map[string]net.IP                   // networkID -> VIP
	Containers map[string]map[string]ContainerInfo // hostname -> containerID -> info
}

type ServiceContainerInfo struct {
	ID         string
	Name       string
	Containers map[string]map[string]ContainerInfo // hostname -> containerID -> info
}

type NetworkInfo struct {
	FlannelID    string
	MTU          int
	Network      *net.IPNet
	HostSubnet   *net.IPNet
	LocalGateway net.IP
}

func SubnetToKey(subnet string) string {
	return strings.ReplaceAll(subnet, "/", "-")
}
func GetPtrFromMap[K comparable, V any](m map[K]V, key K) *V {
	if val, ok := m[key]; ok {
		return &val
	}
	return nil
}

type Equaler interface {
	Equals(other Equaler) bool
}

func (c ContainerInfo) Equals(other Equaler) bool {
	// Type assertion to *ContainerInfo
	o, ok := other.(ContainerInfo)
	if !ok {
		return false
	}
	if c.ID != o.ID || c.Name != o.Name {
		return false
	}
	if !compareIPMaps(c.IPs, o.IPs) {
		return false
	}
	if !compareIPMaps(c.IpamIPs, o.IpamIPs) {
		return false
	}

	return true
}

func CompareIPMaps(a, b map[string]net.IP) bool {
	if len(a) != len(b) {
		return false
	}

	for key, valA := range a {
		valB, exists := b[key]
		if !exists {
			return false
		}
		if !valA.Equal(valB) {
			return false
		}
	}

	return true
}
