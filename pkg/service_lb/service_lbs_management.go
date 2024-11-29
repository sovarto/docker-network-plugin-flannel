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
	AddNetwork(network flannel_network.Network) error
	DeleteNetwork(networkID string) error
	CreateLoadBalancer(service common.ServiceInfo) error
	DeleteLoadBalancer(serviceID string) error
	UpdateLoadBalancer(service common.ServiceInfo) error
}

type serviceLbManagement struct {
	// serviceID, then networkID
	loadBalancers     map[string]map[string]NetworkSpecificServiceLb
	etcdClient        etcd.Client
	fwmarksManagement FwmarksManagement
	interfaces        map[string]*netlink.Link
	networks          map[string]flannel_network.Network
	sync.Mutex
}

func NewServiceLbManagement(etcdClient etcd.Client) ServiceLbsManagement {
	return &serviceLbManagement{
		loadBalancers:     make(map[string]map[string]NetworkSpecificServiceLb),
		etcdClient:        etcdClient,
		fwmarksManagement: NewFwmarksManagement(etcdClient.CreateSubClient("fwmarks")),
		networks:          make(map[string]flannel_network.Network),
	}
}

func (m *serviceLbManagement) AddNetwork(network flannel_network.Network) error {
	m.networks[network.GetInfo().ID] = network
	return nil
}

func (m *serviceLbManagement) DeleteNetwork(networkID string) error {
	delete(m.networks, networkID)
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

	for networkID, lb := range m.loadBalancers[serviceID] {
		err := lb.Delete()
		if err != nil {
			return errors.Wrapf(err, "failed to delete load balancer for service %s and network %s", serviceID, networkID)
		}

		network := m.networks[networkID]
		err = network.GetPool().ReleaseIP(lb.GetFrontendIP().String())
		if err != nil {
			return errors.Wrapf(err, "failed to release IP for service %s and network %s", serviceID, networkID)
		}
		err = m.fwmarksManagement.Release(serviceID, networkID, lb.GetFwmark())
		if err != nil {
			return errors.Wrapf(err, "failed to release fwmark for service %s and network %s", serviceID, networkID)
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

	for networkID, vip := range service.VIPs {
		network, exists := m.networks[networkID]
		if !exists {
			return fmt.Errorf("network %s missing in internal state of service load balancer management", networkID)
		}
		networkInfo := network.GetInfo()

		lbs, exists := m.loadBalancers[service.ID]
		if !exists {
			lbs = make(map[string]NetworkSpecificServiceLb)
			m.loadBalancers[service.ID] = lbs
		}

		interfaceName := getInterfaceName(networkID)
		link, err := networking.EnsureInterface(interfaceName, "dummy", networkInfo.MTU, true)
		if err != nil {
			return errors.Wrapf(err, "failed to create interface %s for network: %s", interfaceName, networkID)
		}

		fwmark, err := m.fwmarksManagement.Get(service.ID, networkID)
		if err != nil {
			return errors.Wrapf(err, "failed to get fwmark for service %s and network: %s", service.ID, networkID)
		}

		lb, exists := lbs[networkInfo.ID]
		if !exists {
			frontendIP, err := network.GetPool().AllocateIP(vip.String(), "", ipam.ReservationTypeServiceVIP, true)
			if err != nil {
				return errors.Wrapf(err, "failed to allocate frontend IP for service %s and network: %s", service.ID, networkID)
			}

			lb, err = NewNetworkSpecificServiceLb(link, networkID, service.ID, fwmark, *frontendIP)
			if err != nil {
				return errors.Wrapf(err, "failed to create load balancer for network: %s", networkID)
			}

			lbs[networkInfo.ID] = lb
		} else {
			err = lb.UpdateFrontendIP(vip)
			if err != nil {
				return errors.Wrapf(err, "failed to update frontend IP for service %s and network: %s", service.ID, networkID)
			}

			err = lb.SetBackends(lo.Map(lo.Values(service.Containers), func(item common.ContainerInfo, index int) net.IP {
				return item.IPs[networkID]
			}))
			if err != nil {
				return errors.Wrapf(err, "failed to set backend IPs for service %s and network: %s", service.ID, networkID)
			}
		}
	}

	return nil

}
