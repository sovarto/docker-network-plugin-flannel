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
	SetFlannelNetwork(dockerNetworkID string, network flannel_network.Network) error
	RegisterOtherNetwork(dockerNetworkID string)
	DeleteNetwork(dockerNetworkID string) error
	CreateLoadBalancer(service common.Service) <-chan error
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
	services                    *common.ConcurrentMap[string, common.Service]
	servicesEventsUnsubscribers *common.ConcurrentMap[string, func()]
	loadBalancers               *common.ConcurrentMap[string, *common.ConcurrentMap[string, NetworkSpecificServiceLb]]
	loadBalancersData           etcd.WriteOnlyStore[loadBalancerData]
	fwmarksManagement           FwmarksManagement
	flannelNetworksByDockerID   *common.ConcurrentMap[string, flannel_network.Network]
	otherNetworksByDockerID     *common.ConcurrentMap[string, struct{}]
	hostname                    string
	networksChanged             *sync.Cond
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
		loadBalancers:               common.NewConcurrentMap[string, *common.ConcurrentMap[string, NetworkSpecificServiceLb]](),
		loadBalancersData:           loadBalancerData,
		fwmarksManagement:           NewFwmarksManagement(etcdClient.CreateSubClient(hostname, "fwmarks")),
		flannelNetworksByDockerID:   common.NewConcurrentMap[string, flannel_network.Network](),
		otherNetworksByDockerID:     common.NewConcurrentMap[string, struct{}](),
		hostname:                    hostname,
		services:                    common.NewConcurrentMap[string, common.Service](),
		servicesEventsUnsubscribers: common.NewConcurrentMap[string, func()](),
		networksChanged:             sync.NewCond(&sync.Mutex{}),
	}, nil
}

func (m *serviceLbManagement) SetFlannelNetwork(dockerNetworkID string, network flannel_network.Network) error {
	if dockerNetworkID == "" {
		return fmt.Errorf("error adding network: docker network id is empty for network %s", network.GetInfo().FlannelID)
	}
	m.flannelNetworksByDockerID.Set(dockerNetworkID, network)
	m.networksChanged.Broadcast()
	return nil
}

func (m *serviceLbManagement) RegisterOtherNetwork(dockerNetworkID string) {
	m.otherNetworksByDockerID.Set(dockerNetworkID, struct{}{})
	m.networksChanged.Broadcast()
}

func (m *serviceLbManagement) DeleteNetwork(dockerNetworkID string) error {
	if dockerNetworkID == "" {
		return fmt.Errorf("error removing network: docker network id is empty")
	}
	m.flannelNetworksByDockerID.Remove(dockerNetworkID)
	m.otherNetworksByDockerID.Remove(dockerNetworkID)
	m.networksChanged.Broadcast()
	return nil
}

func (m *serviceLbManagement) addBackendIPsToLoadBalancer(serviceID string, ips map[string]net.IP) error {
	m.Lock()
	defer m.Unlock()

	lbs, exists := m.loadBalancers.Get(serviceID)
	if !exists {
		return fmt.Errorf("no load balancer for service %s found. This is a bug", serviceID)
	}
	for dockerNetworkID, ip := range ips {
		lb, exists := lbs.Get(dockerNetworkID)
		if !exists {
			return fmt.Errorf("no load balancer for network %s for service %s found. This is a bug", dockerNetworkID, serviceID)
		}
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

	lbs, exists := m.loadBalancers.Get(serviceID)
	if !exists {
		return fmt.Errorf("no load balancer for service %s found. This is a bug", serviceID)
	}

	for dockerNetworkID, ip := range ips {
		lb, exists := lbs.Get(dockerNetworkID)
		if !exists {
			return fmt.Errorf("no load balancer for network %s for service %s found. This is a bug", dockerNetworkID, serviceID)
		}
		err := lb.RemoveBackend(ip)
		if err != nil {
			return errors.WithMessagef(err, "error removing backend ip %s to load balancer for service %s and network %s", ip, serviceID, dockerNetworkID)
		}
	}

	return nil
}

func (m *serviceLbManagement) CreateLoadBalancer(service common.Service) <-chan error {
	done := make(chan error, 1)
	errChan := m.createOrUpdateLoadBalancer(service)
	go func() {
		defer close(done)
		if err := <-errChan; err != nil {
			done <- errors.WithMessagef(err, "error ensuring load balancer for service %s", service.GetInfo().ID)
			return
		}

		unsubscribeFromOnNetworksChanged := service.Events().OnNetworksChanged.Subscribe(func(s common.Service) {
			errChan := m.createOrUpdateLoadBalancer(s)
			go func() {
				if err := <-errChan; err != nil {
					log.Printf("error ensuring load balancer for service %s after network changes. Error: %v\n", service.GetInfo().ID, err)
				}
			}()
		})

		unsubscribeFromOnContainerAdded := service.Events().OnContainerAdded.Subscribe(func(data common.OnContainerData) {
			serviceID := service.GetInfo().ID
			err := m.addBackendIPsToLoadBalancer(serviceID, data.Container.IPs)
			if err != nil {
				log.Printf("error adding backend IPs to load balancer for service %s. Error: %v\n", serviceID, err)
			}
		})

		unsubscribeFromOnContainerRemoved := service.Events().OnContainerRemoved.Subscribe(func(data common.OnContainerData) {
			fmt.Printf("Container removed from service %s: %+v\n", service.GetInfo().ID, data)
			serviceID := service.GetInfo().ID
			err := m.removeBackendIPsFromLoadBalancer(serviceID, data.Container.IPs)
			if err != nil {
				log.Printf("error removing backend IPs from load balancer for service %s. Error: %v\n", serviceID, err)
			}
		})

		m.servicesEventsUnsubscribers.Set(service.GetInfo().ID, func() {
			unsubscribeFromOnNetworksChanged()
			unsubscribeFromOnContainerAdded()
			unsubscribeFromOnContainerRemoved()
		})

		service.Events().OnEndpointModeChanged.Subscribe(func(s common.Service) {
			info := s.GetInfo()
			serviceID := info.ID
			if info.EndpointMode == common.ServiceEndpointModeDnsrr {
				if err := m.DeleteLoadBalancer(serviceID); err != nil {
					log.Printf("error deleting load balancer after service %s switched to endpoint mode DNSRR: %+v\n", serviceID, err)
				}
			} else if info.EndpointMode == common.ServiceEndpointModeVip {
				errChan := m.CreateLoadBalancer(s)
				go func() {
					if err := <-errChan; err != nil {
						log.Printf("error creating load balancer after service %s switched to endpoint mode VIP: %+v\n", serviceID, err)
					}
				}()
			}
		})
	}()

	return done
}

func getInterfaceName(networkID string) string {
	return networking.GetInterfaceName("lbf", "_", networkID)
}

func (m *serviceLbManagement) DeleteLoadBalancer(serviceID string) error {
	m.Lock()
	defer m.Unlock()

	lbs, exists := m.loadBalancers.TryRemove(serviceID)
	if !exists {
		return fmt.Errorf("no load balancer found for service %s.\n", serviceID)
	}

	fmt.Printf("Deleting load balancer for service %s...\n", serviceID)
	unsubscriber, exists := m.servicesEventsUnsubscribers.TryRemove(serviceID)
	if !exists {
		log.Printf("no events unsubscriber for service %s.\n", serviceID)
	} else {
		unsubscriber()
	}

	service, exists := m.services.TryRemove(serviceID)
	if !exists {
		return fmt.Errorf("no service found for ID %s.\n", serviceID)
	}
	serviceInfo := service.GetInfo()

	for _, dockerNetworkID := range lbs.Keys() {
		lb, exists := lbs.Get(dockerNetworkID)
		if !exists {
			continue
		}
		err := m.deleteForNetwork(serviceInfo, lb, dockerNetworkID)
		if err != nil {
			return err
		}
	}
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
		network, exists := m.flannelNetworksByDockerID.Get(dockerNetworkID)
		if !exists {
			return fmt.Errorf("no network found for docker network ID %s", dockerNetworkID)
		}
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

func (m *serviceLbManagement) createOrUpdateLoadBalancer(service common.Service) <-chan error {
	done := make(chan error, 1)
	go func() {
		defer close(done)
		m.networksChanged.L.Lock()
		for m.hasMissingNetworks(service) {
			m.networksChanged.Wait()
		}
		m.networksChanged.L.Unlock()

		m.Lock()
		defer m.Unlock()

		serviceInfo := service.GetInfo()
		m.services.Set(serviceInfo.ID, service)
		lbs, exists, _ := m.loadBalancers.GetOrAdd(serviceInfo.ID, func() (*common.ConcurrentMap[string, NetworkSpecificServiceLb], error) {
			return common.NewConcurrentMap[string, NetworkSpecificServiceLb](), nil
		})

		deleted, _ := lo.Difference(lbs.Keys(), serviceInfo.Networks)

		for _, dockerNetworkID := range serviceInfo.Networks {
			network, exists := m.flannelNetworksByDockerID.Get(dockerNetworkID)
			if !exists {
				if _, exists := m.otherNetworksByDockerID.Get(dockerNetworkID); exists {
					continue
				}
				done <- fmt.Errorf("network %s missing in internal state of service load balancer management. This is a bug", dockerNetworkID)
				return
			}
			networkInfo := network.GetInfo()

			interfaceName := getInterfaceName(dockerNetworkID)
			link, err := networking.EnsureInterface(interfaceName, "dummy", networkInfo.MTU, true)
			if err != nil {
				done <- errors.WithMessagef(err, "failed to create interface %s for network: %s", interfaceName, dockerNetworkID)
				return
			}

			_, err = lbs.TryAdd(dockerNetworkID, func() (NetworkSpecificServiceLb, error) {
				fwmark, err := m.fwmarksManagement.Get(serviceInfo.ID, dockerNetworkID)
				if err != nil {
					return nil, errors.WithMessagef(err, "failed to get fwmark for service %s and network: %s", serviceInfo.ID, dockerNetworkID)
				}
				return NewNetworkSpecificServiceLb(link, dockerNetworkID, serviceInfo.ID, fwmark), nil
			})

			if err != nil {
				done <- err
				return
			}
		}

		for _, deletedNetworkID := range deleted {
			lb, wasRemoved := lbs.TryRemove(deletedNetworkID)
			if wasRemoved {
				err := m.deleteForNetwork(serviceInfo, lb, deletedNetworkID)
				if err != nil {
					done <- errors.WithMessagef(err, "failed to delete load balancer for service %s and network %s", serviceInfo.ID, deletedNetworkID)
					return
				}
			}
		}

		data := loadBalancerData{
			FrontendIPs: make(map[string]net.IP),
		}

		existingData, exists := m.loadBalancersData.GetItem(serviceInfo.ID)

		fmt.Printf("Service %s (%s) has IPAM VIPs %v\n", serviceInfo.Name, serviceInfo.ID, serviceInfo.IpamVIPs)
		// Assumption: for every network we also have an IPAM VIP
		for dockerNetworkID, ipamVip := range serviceInfo.IpamVIPs {
			var ip net.IP
			ipExists := false
			if exists {
				ip, ipExists = existingData.FrontendIPs[dockerNetworkID]
			}

			if !ipExists {
				network, exists := m.flannelNetworksByDockerID.Get(dockerNetworkID)
				if !exists {
					done <- fmt.Errorf("network %s missing in internal state of service load balancer management", dockerNetworkID)
					return
				}

				allocatedIP, err := network.GetPool().AllocateServiceVIP(ipamVip.String(), serviceInfo.ID, true)
				if err != nil {
					done <- errors.WithMessagef(err, "error allocating ip for service %s and network %s", serviceInfo.ID, serviceInfo.ID)
					return
				}

				ip = *allocatedIP
			}
			lb, exists := lbs.Get(dockerNetworkID)
			if !exists {
				done <- fmt.Errorf("load balancer for network %s does not exist in internal state. This is a bug", dockerNetworkID)
				return
			}
			err := lb.UpdateFrontendIP(ip)
			if err != nil {
				done <- errors.WithMessagef(err, "error updating frontend IP to %s for load balancer for service %s and network %s", ip, serviceInfo.ID, dockerNetworkID)
				return
			}
			data.FrontendIPs[dockerNetworkID] = ip
		}

		err := m.loadBalancersData.AddOrUpdateItem(serviceInfo.ID, data)
		if err != nil {
			done <- errors.WithMessagef(err, "error adding load balancer data for service %s", serviceInfo.ID)
			return
		}

		fmt.Printf("Service %s (%s) got these local VIPs: %v\n", serviceInfo.Name, serviceInfo.ID, data.FrontendIPs)
		service.SetVIPs(data.FrontendIPs)

		done <- nil
	}()

	return done
}

func (m *serviceLbManagement) hasMissingNetworks(service common.Service) bool {
	for _, dockerNetworkID := range service.GetInfo().Networks {
		if _, exists := m.flannelNetworksByDockerID.Get(dockerNetworkID); exists {
			continue
		}
		if _, exists := m.otherNetworksByDockerID.Get(dockerNetworkID); exists {
			continue
		}
		return true
	}
	return false
}

func CleanUpStaleLoadBalancers(etcdClient etcd.Client, existingServices []string) error {
	fmt.Println("Cleaning up stale load balancers")
	hostname, err := os.Hostname()
	if err != nil {
		return errors.WithMessage(err, "error getting hostname")
	}
	staleFwmarks, err := cleanUpStaleFwmarks(etcdClient.CreateSubClient(hostname, "fwmarks"), existingServices)
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
	iptablesRulesMangle, err := iptables.List("mangle", "PREROUTING")
	if err != nil {
		return errors.WithMessage(err, "Error getting iptables rules for table mangle")
	}
	iptablesRulesNat, err := iptables.List("nat", "POSTROUTING")
	if err != nil {
		return errors.WithMessage(err, "Error getting iptables rules for table nat")
	}

	iptablesRules := append(iptablesRulesMangle, iptablesRulesNat...)

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
		iptablesRulesForFwmark := lo.Filter(iptablesRules, func(item string, index int) bool {
			return strings.Contains(item, hexFwmark)
		})

		for _, rawRule := range iptablesRulesForFwmark {
			rule := strings.Fields(rawRule)[2:]
			if len(rule) == 0 {
				continue
			}
			if err := iptables.Delete("mangle", "PREROUTING", rule...); err != nil {
				log.Printf("Error deleting iptables rule: %s, err: %v\n", rule, err)
			}
		}
	}

	_, err = etcd.WithConnection(etcdClient, func(connection *etcd.Connection) (struct{}, error) {
		prefix := etcdClient.GetKey(hostname, "data")
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
