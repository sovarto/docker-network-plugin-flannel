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
	"golang.org/x/exp/maps"
	"net"
	"os"
	"sync"
)

// Load balancer per service and flannel network
// Dummy interface per network
// ServiceLbsManagement: global, across networks and services

type ServiceLbsManagement interface {
	AddNetwork(dockerNetworkID string, network flannel_network.Network) error
	DeleteNetwork(dockerNetworkID string) error
	EnsureLoadBalancer(service common.ServiceInfo) error
	DeleteLoadBalancer(serviceID string) error
	AddBackendIPsToLoadBalancer(serviceID string, ips map[string]net.IP) error
	RemoveBackendIPsFromLoadBalancer(serviceID string, ips map[string]net.IP) error
}

type loadBalancerData struct {
	frontendIPs map[string]net.IP // docker network ID -> VIP
}

func (d loadBalancerData) Equals(other common.Equaler) bool {
	o, ok := other.(loadBalancerData)
	if !ok {
		return false
	}

	return common.CompareIPMaps(d.frontendIPs, o.frontendIPs)
}

type serviceLbManagement struct {
	// serviceID, then networkID
	loadBalancers      map[string]map[string]NetworkSpecificServiceLb
	loadBalancersData  etcd.WriteOnlyStore[loadBalancerData]
	etcdClient         etcd.Client
	fwmarksManagement  FwmarksManagement
	interfaces         map[string]*netlink.Link
	networksByDockerID map[string]flannel_network.Network
	hostname           string
	sync.Mutex
}

func NewServiceLbManagement(etcdClient etcd.Client) (ServiceLbsManagement, error) {
	hostname, err := os.Hostname()
	if err != nil {
		return nil, errors.WithMessage(err, "error getting hostname")
	}

	return &serviceLbManagement{
		loadBalancers:      make(map[string]map[string]NetworkSpecificServiceLb),
		etcdClient:         etcdClient,
		loadBalancersData:  etcd.NewWriteOnlyStore(etcdClient.CreateSubClient("data"), etcd.ItemsHandlers[loadBalancerData]{}),
		fwmarksManagement:  NewFwmarksManagement(etcdClient.CreateSubClient("fwmarks")),
		networksByDockerID: make(map[string]flannel_network.Network),
		hostname:           hostname,
	}, nil
}

func (m *serviceLbManagement) AddNetwork(dockerNetworkID string, network flannel_network.Network) error {
	m.networksByDockerID[dockerNetworkID] = network
	return nil
}

func (m *serviceLbManagement) DeleteNetwork(dockerNetworkID string) error {
	delete(m.networksByDockerID, dockerNetworkID)
	return nil
}

func (m *serviceLbManagement) AddBackendIPsToLoadBalancer(serviceID string, ips map[string]net.IP) error {
	err := m.createOrUpdateLoadBalancer(serviceID, maps.Keys(ips))
	if err != nil {
		return err
	}

	lbs := m.loadBalancers[serviceID]
	for dockerNetworkID, ip := range ips {
		lb := lbs[dockerNetworkID]
		err = lb.AddBackend(ip)
		if err != nil {
			return errors.WithMessagef(err, "error adding backend ip %s to load balancer for service %s and network %s", ip, serviceID, dockerNetworkID)
		}
	}

	return nil
}

func (m *serviceLbManagement) RemoveBackendIPsFromLoadBalancer(serviceID string, ips map[string]net.IP) error {
	err := m.createOrUpdateLoadBalancer(serviceID, maps.Keys(ips))
	if err != nil {
		return err
	}

	lbs := m.loadBalancers[serviceID]
	for dockerNetworkID, ip := range ips {
		lb := lbs[dockerNetworkID]
		err = lb.RemoveBackend(ip)
		if err != nil {
			return errors.WithMessagef(err, "error removing backend ip %s to load balancer for service %s and network %s", ip, serviceID, dockerNetworkID)
		}
	}

	return nil
}

func (m *serviceLbManagement) EnsureLoadBalancer(service common.ServiceInfo) error {
	err := m.createOrUpdateLoadBalancer(service.ID, maps.Keys(service.IpamVIPs))
	if err != nil {
		return errors.WithMessagef(err, "error ensuring load balancer for service %s", service.ID)
	}

	data := loadBalancerData{
		frontendIPs: make(map[string]net.IP),
	}

	existingData, exists := m.loadBalancersData.GetItem(service.ID)

	lbs := m.loadBalancers[service.ID]
	for dockerNetworkID, ipamVip := range service.IpamVIPs {
		network := m.networksByDockerID[dockerNetworkID]
		var ip net.IP
		ipExists := false
		if exists {
			ip, ipExists = existingData.frontendIPs[dockerNetworkID]
		}

		if !ipExists {
			allocatedIP, err := network.GetPool().AllocateIP(ipamVip.String(), "", ipam.ReservationTypeServiceVIP, true)
			if err != nil {
				return errors.WithMessagef(err, "error allocating ip for service %s and network %s", service.ID, service.ID)
			}

			ip = *allocatedIP
		}
		lb := lbs[dockerNetworkID]
		err = lb.UpdateFrontendIP(ip)
		if err != nil {
			return errors.WithMessagef(err, "error updating frontend IP to %s for load balancer for service %s and network %s", ip, service.ID, dockerNetworkID)
		}
		data.frontendIPs[dockerNetworkID] = ip
	}

	err = m.loadBalancersData.AddOrUpdateItem(service.ID, data)
	if err != nil {
		return errors.WithMessagef(err, "error adding load balancer data for service %s", service.ID)
	}

	return nil
}

func getInterfaceName(networkID string) string {
	return networking.GetInterfaceName("lbf", "_", networkID)
}

func (m *serviceLbManagement) DeleteLoadBalancer(serviceID string) error {
	m.Lock()
	defer m.Unlock()

	for dockerNetworkID, lb := range m.loadBalancers[serviceID] {
		err := m.deleteForNetwork(serviceID, lb, dockerNetworkID)
		if err != nil {
			return err
		}
	}
	delete(m.loadBalancers, serviceID)
	err := m.loadBalancersData.DeleteItem(serviceID)
	if err != nil {
		return errors.WithMessagef(err, "failed to delete load balancer data for service %s", serviceID)
	}

	return nil
}

func (m *serviceLbManagement) deleteForNetwork(serviceID string, lb NetworkSpecificServiceLb, dockerNetworkID string) error {
	err := lb.Delete()
	if err != nil {
		return errors.WithMessagef(err, "failed to delete load balancer for service %s and network %s", serviceID, dockerNetworkID)
	}

	if lb.GetFrontendIP() != nil {
		network := m.networksByDockerID[dockerNetworkID]
		err = network.GetPool().ReleaseIP(lb.GetFrontendIP().String())
		if err != nil {
			return errors.WithMessagef(err, "failed to release IP for service %s and network %s", serviceID, dockerNetworkID)
		}
	}
	err = m.fwmarksManagement.Release(serviceID, dockerNetworkID, lb.GetFwmark())
	if err != nil {
		return errors.WithMessagef(err, "failed to release fwmark for service %s and network %s", serviceID, dockerNetworkID)
	}
	return nil
}

func (m *serviceLbManagement) createOrUpdateLoadBalancer(serviceID string, dockerNetworkIDs []string) error {
	m.Lock()
	defer m.Unlock()

	lbs, exists := m.loadBalancers[serviceID]
	if !exists {
		lbs = make(map[string]NetworkSpecificServiceLb)
		m.loadBalancers[serviceID] = lbs
	}

	deleted, _ := lo.Difference(maps.Keys(lbs), dockerNetworkIDs)

	for _, dockerNetworkID := range dockerNetworkIDs {
		network, exists := m.networksByDockerID[dockerNetworkID]
		if !exists {
			return fmt.Errorf("network %s missing in internal state of service load balancer management", dockerNetworkID)
		}
		networkInfo := network.GetInfo()

		interfaceName := getInterfaceName(dockerNetworkID)
		link, err := networking.EnsureInterface(interfaceName, "dummy", networkInfo.MTU, true)
		if err != nil {
			return errors.WithMessagef(err, "failed to create interface %s for network: %s", interfaceName, dockerNetworkID)
		}

		_, exists = lbs[dockerNetworkID]
		if !exists {
			fwmark, err := m.fwmarksManagement.Get(serviceID, dockerNetworkID)
			if err != nil {
				return errors.WithMessagef(err, "failed to get fwmark for service %s and network: %s", serviceID, dockerNetworkID)
			}

			lbs[dockerNetworkID] = NewNetworkSpecificServiceLb(link, dockerNetworkID, serviceID, fwmark)
		}
	}

	for _, deletedNetworkID := range deleted {
		err := m.deleteForNetwork(serviceID, lbs[deletedNetworkID], deletedNetworkID)
		if err != nil {
			return errors.WithMessagef(err, "failed to delete load balancer for service %s and network %s", serviceID, deletedNetworkID)
		}
		delete(lbs, deletedNetworkID)
	}

	return nil
}
