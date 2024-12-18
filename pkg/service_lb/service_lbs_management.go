package service_lb

import (
	"fmt"
	"github.com/coreos/go-iptables/iptables"
	"github.com/moby/ipvs"
	"github.com/pkg/errors"
	"github.com/samber/lo"
	"github.com/sovarto/FlannelNetworkPlugin/pkg/common"
	"github.com/sovarto/FlannelNetworkPlugin/pkg/etcd"
	"github.com/sovarto/FlannelNetworkPlugin/pkg/flannel_network"
	"github.com/sovarto/FlannelNetworkPlugin/pkg/networking"
	clientv3 "go.etcd.io/etcd/client/v3"
	"golang.org/x/exp/maps"
	"log"
	"net"
	"os"
	"strings"
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
	FrontendIPs map[string]net.IP `json:"FrontendIPs"` // docker network ID -> VIP
}

func (d loadBalancerData) Equals(other common.Equaler) bool {
	o, ok := other.(loadBalancerData)
	if !ok {
		return false
	}

	return common.CompareIPMaps(d.FrontendIPs, o.FrontendIPs)
}

type serviceLbManagement struct {
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

	loadBalancerData := etcd.NewWriteOnlyStore(etcdClient.CreateSubClient(hostname, "data"), etcd.ItemsHandlers[loadBalancerData]{})
	if err := loadBalancerData.InitFromEtcd(); err != nil {
		return nil, errors.WithMessage(err, "error initializing load balancer data store")
	}

	return &serviceLbManagement{
		loadBalancers:               make(map[string]map[string]NetworkSpecificServiceLb),
		loadBalancersData:           loadBalancerData,
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

	serviceInfo := m.services[serviceID].GetInfo()

	for dockerNetworkID, lb := range lbs {
		err := m.deleteForNetwork(serviceInfo, lb, dockerNetworkID)
		if err != nil {
			return err
		}
	}
	delete(m.loadBalancers, serviceID)
	delete(m.services, serviceID)
	err := m.loadBalancersData.DeleteItem(serviceID)
	if err != nil {
		return errors.WithMessagef(err, "failed to delete load balancer data for service %s", serviceID)
	}

	return nil
}

func (m *serviceLbManagement) deleteForNetwork(serviceInfo common.ServiceInfo, lb NetworkSpecificServiceLb, dockerNetworkID string) error {
	err := lb.Delete()
	if err != nil {
		return errors.WithMessagef(err, "failed to delete load balancer for service %s and network %s", serviceInfo.ID, dockerNetworkID)
	}

	// Only release VIP if we allocated it - that's the case when IpamVIP and actual VIP differ
	if lb.GetFrontendIP() != nil && !serviceInfo.IpamVIPs[dockerNetworkID].Equal(serviceInfo.VIPs[dockerNetworkID]) {
		network := m.networksByDockerID[dockerNetworkID]
		err = network.GetPool().ReleaseIP(lb.GetFrontendIP().String())
		if err != nil {
			return errors.WithMessagef(err, "failed to release IP for service %s and network %s", serviceInfo.ID, dockerNetworkID)
		}
	}
	err = m.fwmarksManagement.Release(serviceInfo.ID, dockerNetworkID, lb.GetFwmark())
	if err != nil {
		return errors.WithMessagef(err, "failed to release fwmark for service %s and network %s", serviceInfo.ID, dockerNetworkID)
	}
	return nil
}

func (m *serviceLbManagement) createOrUpdateLoadBalancer(service common.Service) error {
	m.Lock()
	defer m.Unlock()

	serviceInfo := service.GetInfo()
	m.services[serviceInfo.ID] = service
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
		err := m.deleteForNetwork(serviceInfo, lbs[deletedNetworkID], deletedNetworkID)
		if err != nil {
			return errors.WithMessagef(err, "failed to delete load balancer for service %s and network %s", serviceInfo.ID, deletedNetworkID)
		}
		delete(lbs, deletedNetworkID)
	}

	data := loadBalancerData{
		FrontendIPs: make(map[string]net.IP),
	}

	existingData, exists := m.loadBalancersData.GetItem(serviceInfo.ID)

	fmt.Printf("Service %s has IPAM VIPs %v\n", serviceInfo.ID, serviceInfo.IpamVIPs)
	// Assumption: for every network we also have an IPAM VIP
	for dockerNetworkID, ipamVip := range serviceInfo.IpamVIPs {
		network := m.networksByDockerID[dockerNetworkID]
		var ip net.IP
		ipExists := false
		if exists {
			ip, ipExists = existingData.FrontendIPs[dockerNetworkID]
		}

		if !ipExists {
			allocatedIP, err := network.GetPool().AllocateServiceVIP(ipamVip.String(), serviceInfo.ID, true)
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
		data.FrontendIPs[dockerNetworkID] = ip
	}

	err := m.loadBalancersData.AddOrUpdateItem(serviceInfo.ID, data)
	if err != nil {
		return errors.WithMessagef(err, "error adding load balancer data for service %s", serviceInfo.ID)
	}

	service.SetVIPs(data.FrontendIPs)

	return nil
}

func CleanUpStaleLoadBalancers(etcdClient etcd.Client, existingServices []string) error {
	hostname, err := os.Hostname()
	if err != nil {
		return errors.WithMessage(err, "error getting hostname")
	}
	staleFwmarks, err := CleanUpStaleFwmarks(etcdClient.CreateSubClient(hostname, "fwmarks"), existingServices)
	if err != nil {
		return errors.WithMessage(err, "error cleaning up stale fwmarks")
	}

	iptables, err := iptables.New()
	if err != nil {
		return errors.WithMessage(err, "Error creating iptables handle")
	}

	ipvsHandle, err := ipvs.New("")
	if err != nil {
		return errors.WithMessage(err, "Error creating IPVS handle")
	}
	defer ipvsHandle.Close()

	ipvsServices, err := ipvsHandle.GetServices()
	if err != nil {
		return errors.WithMessage(err, "Error getting IPVS services")
	}
	iptableRules, err := iptables.List("mangle", "PREROUTING")
	if err != nil {
		return errors.WithMessage(err, "Error getting iptables rules")
	}

	for _, staleFwmark := range staleFwmarks {
		ipvsServicesForFwmark := lo.Filter(ipvsServices, func(item *ipvs.Service, index int) bool {
			return item.FWMark == staleFwmark
		})

		for _, ipvsService := range ipvsServicesForFwmark {
			if err := ipvsHandle.DelService(ipvsService); err != nil {
				log.Printf("Error deleting ipvs service for fwmark %d: %v\n", staleFwmark, err)
			}
		}

		hexFwmark := fmt.Sprintf("0x%08x", staleFwmark)
		iptablesRulesForFwmark := lo.Filter(iptableRules, func(item string, index int) bool {
			return strings.Contains(item, hexFwmark)
		})

		for _, rule := range iptablesRulesForFwmark {
			if err := iptables.Delete("mangle", "PREROUTING", rule); err != nil {
				log.Printf("Error deleting iptables rule: %s, err: %v\n", rule, err)
			}
		}
	}
	_, err = etcd.WithConnection(etcdClient, func(connection *etcd.Connection) (struct{}, error) {
		prefix := etcdClient.GetKey("data")
		resp, err := connection.Client.Get(connection.Ctx, prefix, clientv3.WithPrefix())
		if err != nil {
			return struct{}{}, errors.WithMessagef(err, "error retrieving existing networks data from etcd")
		}

		for _, kv := range resp.Kvs {
			key := strings.TrimLeft(strings.TrimPrefix(string(kv.Key), prefix), "/")
			if !lo.Some(existingServices, []string{key}) {
				_, err = connection.Client.Delete(connection.Ctx, string(kv.Key))
				if err != nil {
					log.Printf("Error deleting key %s, err: %v\n", string(kv.Key), err)
				}
			}
		}

		return struct{}{}, nil
	})

	return err
}
