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
	"golang.org/x/exp/maps"
	"log"
	"net"
	"os"
	"sync"
)

// Load balancer per service and flannel network
// Dummy interface per network
// ServiceLbsManagement: global, across networks and services

type ServiceLbsManagement interface {
	SetNetwork(dockerNetworkID string, network flannel_network.Network) error
	DeleteNetwork(dockerNetworkID string) error
	CreateLoadBalancer(service common.Service) error
	DeleteLoadBalancer(serviceID string) error
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
	services                    map[string]common.Service
	servicesEventsUnsubscribers map[string]func()
	loadBalancers               map[string]map[string]NetworkSpecificServiceLb
	loadBalancersData           etcd.WriteOnlyStore[loadBalancerData]
	fwmarksManagement           FwmarksManagement
	networksByDockerID          map[string]flannel_network.Network
	hostname                    string
	sync.Mutex
}

func NewServiceLbManagement(etcdClient etcd.Client) (ServiceLbsManagement, error) {
	hostname, err := os.Hostname()
	if err != nil {
		return nil, errors.WithMessage(err, "error getting hostname")
	}

	return &serviceLbManagement{
		loadBalancers:               make(map[string]map[string]NetworkSpecificServiceLb),
		loadBalancersData:           etcd.NewWriteOnlyStore(etcdClient.CreateSubClient(hostname, "data"), etcd.ItemsHandlers[loadBalancerData]{}),
		fwmarksManagement:           NewFwmarksManagement(etcdClient.CreateSubClient(hostname, "fwmarks")),
		networksByDockerID:          make(map[string]flannel_network.Network),
		hostname:                    hostname,
		services:                    make(map[string]common.Service),
		servicesEventsUnsubscribers: make(map[string]func()),
	}, nil
}

func (m *serviceLbManagement) SetNetwork(dockerNetworkID string, network flannel_network.Network) error {
	if dockerNetworkID == "" {
		return fmt.Errorf("error adding network: docker network id is empty for network %s", network.GetInfo().FlannelID)
	}
	m.networksByDockerID[dockerNetworkID] = network
	return nil
}

func (m *serviceLbManagement) DeleteNetwork(dockerNetworkID string) error {
	if dockerNetworkID == "" {
		return fmt.Errorf("error removing network: docker network id is empty")
	}
	delete(m.networksByDockerID, dockerNetworkID)
	return nil
}

func (m *serviceLbManagement) addBackendIPsToLoadBalancer(serviceID string, ips map[string]net.IP) error {
	m.Lock()
	defer m.Unlock()

	lbs, exists := m.loadBalancers[serviceID]
	if !exists {
		return fmt.Errorf("no load balancer for service %s found. This is a bug", serviceID)
	}
	for dockerNetworkID, ip := range ips {
		lb := lbs[dockerNetworkID]
		err := lb.AddBackend(ip)
		if err != nil {
			return errors.WithMessagef(err, "error adding backend ip %s to load balancer for service %s and network %s", ip, serviceID, dockerNetworkID)
		}
	}

	return nil
}

func (m *serviceLbManagement) removeBackendIPsFromLoadBalancer(serviceID string, ips map[string]net.IP) error {
	m.Lock()
	defer m.Unlock()

	lbs, exists := m.loadBalancers[serviceID]
	if !exists {
		return fmt.Errorf("no load balancer for service %s found. This is a bug", serviceID)
	}

	for dockerNetworkID, ip := range ips {
		lb := lbs[dockerNetworkID]
		err := lb.RemoveBackend(ip)
		if err != nil {
			return errors.WithMessagef(err, "error removing backend ip %s to load balancer for service %s and network %s", ip, serviceID, dockerNetworkID)
		}
	}

	return nil
}

func (m *serviceLbManagement) CreateLoadBalancer(service common.Service) error {
	err := m.createOrUpdateLoadBalancer(service)
	if err != nil {
		return errors.WithMessagef(err, "error ensuring load balancer for service %s", service.GetInfo().ID)
	}

	unsubscribeFromOnNetworksChanged := service.Events().OnNetworksChanged.Subscribe(func(s common.Service) {
		err := m.createOrUpdateLoadBalancer(s)
		if err != nil {
			log.Printf("error ensuring load balancer for service %s after network changes\n", service.GetInfo().ID)
		}
	})

	unsubscribeFromOnContainerAdded := service.Events().OnContainerAdded.Subscribe(func(data common.OnContainerData) {
		serviceID := service.GetInfo().ID
		err := m.addBackendIPsToLoadBalancer(serviceID, data.Container.IPs)
		if err != nil {
			log.Printf("error adding backend IPs to load balancer for service %s\n", serviceID)
		}
	})

	unsubscribeFromOnContainerRemoved := service.Events().OnContainerRemoved.Subscribe(func(data common.OnContainerData) {
		fmt.Printf("Container removed from service %s: %+v\n", service.GetInfo().ID, data)
		serviceID := service.GetInfo().ID
		err := m.removeBackendIPsFromLoadBalancer(serviceID, data.Container.IPs)
		if err != nil {
			log.Printf("error removing backend IPs from load balancer for service %s\n", serviceID)
		}
	})

	m.servicesEventsUnsubscribers[service.GetInfo().ID] = func() {
		unsubscribeFromOnNetworksChanged()
		unsubscribeFromOnContainerAdded()
		unsubscribeFromOnContainerRemoved()
	}

	service.Events().OnEndpointModeChanged.Subscribe(func(s common.Service) {
		info := s.GetInfo()
		serviceID := info.ID
		if info.EndpointMode == common.ServiceEndpointModeDnsrr {
			err := m.DeleteLoadBalancer(serviceID)
			if err != nil {
				log.Printf("error deleting load balancer after service %s switched to endpoint mode DNSRR: %+v\n", serviceID, err)
			}
		} else if info.EndpointMode == common.ServiceEndpointModeVip {
			err := m.CreateLoadBalancer(s)
			if err != nil {
				log.Printf("error creating load balancer after service %s switched to endpoint mode VIP: %+v\n", serviceID, err)
			}
		}
	})

	return nil
}

func getInterfaceName(networkID string) string {
	return networking.GetInterfaceName("lbf", "_", networkID)
}

func (m *serviceLbManagement) DeleteLoadBalancer(serviceID string) error {
	m.Lock()
	defer m.Unlock()
	fmt.Printf("Deleting load balancer for service %s...\n", serviceID)
	unsubscriber, exists := m.servicesEventsUnsubscribers[serviceID]
	if !exists {
		log.Printf("no events unsubscriber for service %s. This is a bug\n", serviceID)
	} else {
		delete(m.servicesEventsUnsubscribers, serviceID)
		unsubscriber()
	}

	lbs, exists := m.loadBalancers[serviceID]
	if !exists {
		return fmt.Errorf("no load balancer found for service %s.\n", serviceID)
	}

	for dockerNetworkID, lb := range lbs {
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

func (m *serviceLbManagement) createOrUpdateLoadBalancer(service common.Service) error {
	m.Lock()
	defer m.Unlock()

	serviceInfo := service.GetInfo()
	lbs, exists := m.loadBalancers[serviceInfo.ID]
	if !exists {
		lbs = make(map[string]NetworkSpecificServiceLb)
		m.loadBalancers[serviceInfo.ID] = lbs
	}

	deleted, _ := lo.Difference(maps.Keys(lbs), serviceInfo.Networks)

	for _, dockerNetworkID := range serviceInfo.Networks {
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
			fwmark, err := m.fwmarksManagement.Get(serviceInfo.ID, dockerNetworkID)
			if err != nil {
				return errors.WithMessagef(err, "failed to get fwmark for service %s and network: %s", serviceInfo.ID, dockerNetworkID)
			}

			lbs[dockerNetworkID] = NewNetworkSpecificServiceLb(link, dockerNetworkID, serviceInfo.ID, fwmark)
		}
	}

	for _, deletedNetworkID := range deleted {
		err := m.deleteForNetwork(serviceInfo.ID, lbs[deletedNetworkID], deletedNetworkID)
		if err != nil {
			return errors.WithMessagef(err, "failed to delete load balancer for service %s and network %s", serviceInfo.ID, deletedNetworkID)
		}
		delete(lbs, deletedNetworkID)
	}

	data := loadBalancerData{
		frontendIPs: make(map[string]net.IP),
	}

	existingData, exists := m.loadBalancersData.GetItem(serviceInfo.ID)

	fmt.Printf("Service %s has IPAM VIPs %v\n", serviceInfo.ID, serviceInfo.IpamVIPs)
	// Assumption: for every network we also have an IPAM VIP
	for dockerNetworkID, ipamVip := range serviceInfo.IpamVIPs {
		network := m.networksByDockerID[dockerNetworkID]
		var ip net.IP
		ipExists := false
		if exists {
			ip, ipExists = existingData.frontendIPs[dockerNetworkID]
		}

		if !ipExists {
			allocatedIP, err := network.GetPool().AllocateIP(ipamVip.String(), "", ipam.ReservationTypeServiceVIP, true)
			if err != nil {
				return errors.WithMessagef(err, "error allocating ip for service %s and network %s", serviceInfo.ID, serviceInfo.ID)
			}

			ip = *allocatedIP
		}
		lb := lbs[dockerNetworkID]
		err := lb.UpdateFrontendIP(ip)
		if err != nil {
			return errors.WithMessagef(err, "error updating frontend IP to %s for load balancer for service %s and network %s", ip, serviceInfo.ID, dockerNetworkID)
		}
		data.frontendIPs[dockerNetworkID] = ip
	}

	err := m.loadBalancersData.AddOrUpdateItem(serviceInfo.ID, data)
	if err != nil {
		return errors.WithMessagef(err, "error adding load balancer data for service %s", serviceInfo.ID)
	}

	fmt.Printf("Setting VIPs of service %s to %v\n", serviceInfo.ID, data.frontendIPs)
	service.SetVIPs(data.frontendIPs)
	fmt.Printf("Service %s infos: %+v\n", serviceInfo.ID, service.GetInfo())

	return nil
}
