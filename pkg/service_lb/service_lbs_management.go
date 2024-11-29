package service_lb

import (
	"fmt"
	"github.com/pkg/errors"
	"github.com/samber/lo"
	"github.com/sovarto/FlannelNetworkPlugin/pkg/common"
	"github.com/sovarto/FlannelNetworkPlugin/pkg/etcd"
	"github.com/sovarto/FlannelNetworkPlugin/pkg/flannel_network"
	"github.com/sovarto/FlannelNetworkPlugin/pkg/ipam"
	"github.com/sovarto/FlannelNetworkPlugin/pkg/networking"
	"github.com/vishvananda/netlink"
	"net"
	"sync"
)

// Load balancer per service and flannel network
// Dummy interface per network
// ServiceLbsManagement: global, across networks and services

type ServiceLbsManagement interface {
	AddNetwork(dockerNetworkID string, network flannel_network.Network) error
	DeleteNetwork(dockerNetworkID string) error
	CreateLoadBalancer(service common.ServiceInfo) error
	DeleteLoadBalancer(serviceID string) error
	UpdateLoadBalancer(service common.ServiceInfo) error
}

type serviceLbManagement struct {
	// serviceID, then networkID
	loadBalancers      map[string]map[string]NetworkSpecificServiceLb
	etcdClient         etcd.Client
	fwmarksManagement  FwmarksManagement
	interfaces         map[string]*netlink.Link
	networksByDockerID map[string]flannel_network.Network
	sync.Mutex
}

func NewServiceLbManagement(etcdClient etcd.Client) ServiceLbsManagement {
	return &serviceLbManagement{
		loadBalancers:      make(map[string]map[string]NetworkSpecificServiceLb),
		etcdClient:         etcdClient,
		fwmarksManagement:  NewFwmarksManagement(etcdClient.CreateSubClient("fwmarks")),
		networksByDockerID: make(map[string]flannel_network.Network),
	}
}

func (m *serviceLbManagement) AddNetwork(dockerNetworkID string, network flannel_network.Network) error {
	m.networksByDockerID[dockerNetworkID] = network
	return nil
}

func (m *serviceLbManagement) DeleteNetwork(dockerNetworkID string) error {
	delete(m.networksByDockerID, dockerNetworkID)
	return nil
}

func (m *serviceLbManagement) CreateLoadBalancer(service common.ServiceInfo) error {
	return m.createOrUpdateLoadBalancer(service)
}

func getInterfaceName(networkID string) string {
	return networking.GetInterfaceName("lbf", "_", networkID)
}

func (m *serviceLbManagement) DeleteLoadBalancer(serviceID string) error {
	m.Lock()
	defer m.Unlock()

	for dockerNetworkID, lb := range m.loadBalancers[serviceID] {
		err := lb.Delete()
		if err != nil {
			return errors.Wrapf(err, "failed to delete load balancer for service %s and network %s", serviceID, dockerNetworkID)
		}

		network := m.networksByDockerID[dockerNetworkID]
		err = network.GetPool().ReleaseIP(lb.GetFrontendIP().String())
		if err != nil {
			return errors.Wrapf(err, "failed to release IP for service %s and network %s", serviceID, dockerNetworkID)
		}
		err = m.fwmarksManagement.Release(serviceID, dockerNetworkID, lb.GetFwmark())
		if err != nil {
			return errors.Wrapf(err, "failed to release fwmark for service %s and network %s", serviceID, dockerNetworkID)
		}
	}

	delete(m.loadBalancers, serviceID)

	return nil
}

func (m *serviceLbManagement) UpdateLoadBalancer(service common.ServiceInfo) error {
	return m.createOrUpdateLoadBalancer(service)
}

func (m *serviceLbManagement) createOrUpdateLoadBalancer(service common.ServiceInfo) error {
	m.Lock()
	defer m.Unlock()

	for dockerNetworkID, vip := range service.VIPs {
		network, exists := m.networksByDockerID[dockerNetworkID]
		if !exists {
			return fmt.Errorf("network %s missing in internal state of service load balancer management", dockerNetworkID)
		}
		networkInfo := network.GetInfo()

		lbs, exists := m.loadBalancers[service.ID]
		if !exists {
			lbs = make(map[string]NetworkSpecificServiceLb)
			m.loadBalancers[service.ID] = lbs
		}

		interfaceName := getInterfaceName(dockerNetworkID)
		link, err := networking.EnsureInterface(interfaceName, "dummy", networkInfo.MTU, true)
		if err != nil {
			return errors.Wrapf(err, "failed to create interface %s for network: %s", interfaceName, dockerNetworkID)
		}

		fwmark, err := m.fwmarksManagement.Get(service.ID, dockerNetworkID)
		if err != nil {
			return errors.Wrapf(err, "failed to get fwmark for service %s and network: %s", service.ID, dockerNetworkID)
		}

		lb, exists := lbs[dockerNetworkID]
		if !exists {
			frontendIP, err := network.GetPool().AllocateIP(vip.String(), "", ipam.ReservationTypeServiceVIP, true)
			if err != nil {
				return errors.Wrapf(err, "failed to allocate frontend IP for service %s and network: %s", service.ID, dockerNetworkID)
			}

			lb, err = NewNetworkSpecificServiceLb(link, dockerNetworkID, service.ID, fwmark, *frontendIP)
			if err != nil {
				return errors.Wrapf(err, "failed to create load balancer for network: %s", dockerNetworkID)
			}

			lbs[dockerNetworkID] = lb
		} else {
			err = lb.UpdateFrontendIP(vip)
			if err != nil {
				return errors.Wrapf(err, "failed to update frontend IP for service %s and network: %s", service.ID, dockerNetworkID)
			}

			err = lb.SetBackends(lo.Map(lo.Values(service.Containers), func(item common.ContainerInfo, index int) net.IP {
				return item.IPs[dockerNetworkID]
			}))
			if err != nil {
				return errors.Wrapf(err, "failed to set backend IPs for service %s and network: %s", service.ID, dockerNetworkID)
			}
		}
	}

	return nil

}
