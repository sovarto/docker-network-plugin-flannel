package docker

import (
	"github.com/sovarto/FlannelNetworkPlugin/pkg/common"
	"github.com/sovarto/FlannelNetworkPlugin/pkg/etcd"
	"net"
)

type ServiceContainersServiceInfo struct {
	ID         string
	Name       string
	Containers etcd.ShardedDistributedStore[ServiceContainersContainerInfo]
}

type ServiceContainersContainerInfo struct {
	ID   string
	Name string
	IPs  map[string]net.IP `json:"IPs"` // -> networkID -> IP
}

func (c ServiceContainersContainerInfo) Equals(other common.Equaler) bool {
	o, ok := other.(ServiceContainersContainerInfo)
	if !ok {
		return false
	}
	if c.ID != o.ID || c.Name != o.Name {
		return false
	}
	if !common.CompareIPMaps(c.IPs, o.IPs) {
		return false
	}

	return true
}
