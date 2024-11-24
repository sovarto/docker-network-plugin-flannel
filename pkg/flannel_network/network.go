package flannel_network

import (
	"github.com/sovarto/FlannelNetworkPlugin/pkg/common"
	"github.com/sovarto/FlannelNetworkPlugin/pkg/etcd"
	"net"
)

type Network interface {
	Ensure() error
	GetInfo() common.NetworkInfo
	Delete() error
}

type network struct {
	id            string
	pid           int
	etcdClient    etcd.Client
	networkSubnet net.IPNet
	hostSubnet    net.IPNet
	localGateway  net.IP
	mtu           int
}

func NewNetwork(etcdClient etcd.Client, id string, networkSubnet net.IPNet) Network {
	result := &network{
		id:            id,
		etcdClient:    etcdClient,
		networkSubnet: networkSubnet,
	}

	return result
}

func (n *network) GetInfo() common.NetworkInfo {
	return common.NetworkInfo{
		ID:           n.id,
		MTU:          n.mtu,
		Network:      n.networkSubnet,
		HostSubnet:   n.hostSubnet,
		LocalGateway: n.localGateway,
	}
}

func (n *network) Delete() error {

}

func (n *network) Ensure() error {

}
