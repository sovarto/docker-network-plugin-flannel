package service_lb

import (
	"github.com/pkg/errors"
	"github.com/sovarto/FlannelNetworkPlugin/pkg/common"
	"github.com/sovarto/FlannelNetworkPlugin/pkg/etcd"
	"github.com/sovarto/FlannelNetworkPlugin/pkg/interface_management"
	"github.com/vishvananda/netlink"
	"sync"
)

// Load balancer per service and network
// Dummy interface per network
// ServiceLbManagement: global, across networks and services

type ServiceLbManagement interface {
	AddNetwork(network common.NetworkInfo) error
	DeleteNetwork(network common.NetworkInfo) error
	CreateLoadBalancer(service common.ServiceInfo) (ServiceLb, error)
	DeleteLoadBalancer(service common.ServiceInfo) error
}

type serviceLbManagement struct {
	// service, then network
	loadBalancers     map[string]map[string]NetworkSpecificServiceLb
	etcdClient        etcd.Client
	fwmarksManagement FwmarksManagement
	interfaces        map[string]*netlink.Link
	networks          map[string]common.NetworkInfo
	sync.Mutex
}

func NewServiceLbManagement(etcdClient etcd.Client) ServiceLbManagement {
	return &serviceLbManagement{
		loadBalancers:     make(map[string]map[string]NetworkSpecificServiceLb),
		etcdClient:        etcdClient,
		fwmarksManagement: NewFwmarksManagement(etcdClient),
		networks:          make(map[string]common.NetworkInfo),
	}
}

func (m *serviceLbManagement) AddNetwork(network common.NetworkInfo) error {
	m.networks[network.ID] = network
}

func (m *serviceLbManagement) DeleteNetwork(network common.NetworkInfo) error {
	m.networks[network.ID] = network
}
func (m *serviceLbManagement) CreateLoadBalancer(service common.ServiceInfo) (ServiceLb, error) {
	m.Lock()
	defer m.Unlock()

	for networkID, _ := range service.Networks {
		networkInfo := m.networks[networkID]
		interfaceName := getInterfaceName(networkID)
		link, err := interface_management.EnsureInterface(interfaceName, "dummy", networkInfo.MTU, true)
		if err != nil {
			return nil, errors.Wrapf(err, "failed to create interface %s for network: %s", interfaceName, networkID)
		}

		NewServiceLb
	}
}

func getInterfaceName(networkID string) string {
	return interface_management.GetInterfaceName("lbf", "_", networkID)
}
