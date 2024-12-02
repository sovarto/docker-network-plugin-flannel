package driver

import (
	"fmt"
	"github.com/docker/docker/libnetwork/types"
	"github.com/docker/go-plugins-helpers/sdk"
	"github.com/pkg/errors"
	"github.com/sovarto/FlannelNetworkPlugin/pkg/api"
	"github.com/sovarto/FlannelNetworkPlugin/pkg/common"
	"github.com/sovarto/FlannelNetworkPlugin/pkg/docker"
	"github.com/sovarto/FlannelNetworkPlugin/pkg/etcd"
	"github.com/sovarto/FlannelNetworkPlugin/pkg/flannel_network"
	"github.com/sovarto/FlannelNetworkPlugin/pkg/ipam"
	"github.com/sovarto/FlannelNetworkPlugin/pkg/service_lb"
	"log"
	"math"
	"net"
	"sync"
	"time"
)

type FlannelDriver interface {
	Serve() error
}

type flannelDriver struct {
	globalAddressSpace    ipam.AddressSpace
	etcdPrefix            string
	etcdEndPoints         []string
	defaultFlannelOptions []string
	defaultHostSubnetSize int
	networksByFlannelID   map[string]flannel_network.Network // flannel network ID -> network
	networksByDockerID    map[string]flannel_network.Network // docker network ID -> network
	serviceLbsManagement  service_lb.ServiceLbsManagement
	dockerData            docker.Data
	vniStart              int
	sync.Mutex
}

func NewFlannelDriver(etcdEndPoints []string, etcdPrefix string, defaultFlannelOptions []string, completeSpace []net.IPNet, networkSubnetSize int, defaultHostSubnetSize int) (FlannelDriver, error) {

	driver := &flannelDriver{
		etcdPrefix:            etcdPrefix,
		etcdEndPoints:         etcdEndPoints,
		defaultFlannelOptions: defaultFlannelOptions,
		defaultHostSubnetSize: defaultHostSubnetSize,
		networksByFlannelID:   make(map[string]flannel_network.Network),
		networksByDockerID:    make(map[string]flannel_network.Network),
		vniStart:              6514,
	}

	globalAddressSpace, err := ipam.NewEtcdBasedAddressSpace(completeSpace, networkSubnetSize, driver.getEtcdClient("address-space"))
	if err != nil {
		return nil, errors.WithMessage(err, "Failed to create address space")
	}
	driver.globalAddressSpace = globalAddressSpace

	numIPsPerNode := int(math.Pow(2, float64(32-defaultHostSubnetSize)))
	numNodesPerNetwork := int(math.Pow(2, float64(defaultHostSubnetSize-networkSubnetSize)))
	fmt.Printf("The address space settings result in support for a total of %d nodes and %d IP addresses per node and docker network (including service VIPs)\n", numNodesPerNetwork, numIPsPerNode)
	fmt.Println("Initialized address space")

	callbacks := docker.Callbacks{
		NetworkAdded:   driver.handleNetworkAddedOrChanged,
		NetworkChanged: driver.handleNetworkAddedOrChanged,
		NetworkRemoved: driver.handleNetworkRemoved,
	}

	containerCallbacks := etcd.ShardItemsHandlers[common.ContainerInfo]{
		OnAdded:   driver.handleContainersAdded,
		OnChanged: driver.handleContainersChanged,
		OnRemoved: driver.handleContainersRemoved,
	}

	serviceCallbacks := etcd.ItemsHandlers[common.ServiceInfo]{
		OnAdded:   driver.handleServicesAdded,
		OnChanged: driver.handleServicesChanged,
		OnRemoved: driver.handleServicesRemoved,
	}

	serviceLbsManagement, err := service_lb.NewServiceLbManagement(driver.getEtcdClient("service-lbs"))
	if err != nil {
		return nil, errors.WithMessage(err, "Failed to create service lbs management")
	}
	driver.serviceLbsManagement = serviceLbsManagement
	fmt.Println("Initialized service load balancer management")

	dockerData, err := docker.NewData(driver.getEtcdClient("docker-data"), containerCallbacks, serviceCallbacks, callbacks)
	if err != nil {
		return nil, errors.WithMessage(err, "Failed to create docker data handler")
	}
	driver.dockerData = dockerData

	err = dockerData.Init()
	if err != nil {
		return nil, errors.WithMessage(err, "Failed to initialize docker data handler")
	}
	fmt.Println("Initialized docker data handler")

	return driver, nil
}

func (d *flannelDriver) Serve() error {
	handler := sdk.NewHandler(`{"Implements": ["IpamDriver", "NetworkDriver"]}`)
	api.InitIpamMux(&handler, d)
	api.InitNetworkMux(&handler, d)

	if err := handler.ServeUnix("flannel-np", 0); err != nil {
		return errors.WithMessagef(err, "Failed to start flannel plugin server")
	}

	return nil
}

func (d *flannelDriver) getEtcdClient(prefix string) etcd.Client {
	return etcd.NewEtcdClient(d.etcdEndPoints, 5*time.Second, fmt.Sprintf("%s/%s", d.etcdPrefix, prefix))
}

func (d *flannelDriver) getEndpoint(dockerNetworkID, endpointID string) (flannel_network.Network, flannel_network.Endpoint, error) {
	flannelNetwork, exists := d.networksByDockerID[dockerNetworkID]
	if !exists {
		return nil, nil, fmt.Errorf("no flannel network found for ID %s", dockerNetworkID)
	}

	endpoint := flannelNetwork.GetEndpoint(endpointID)

	if endpoint == nil {
		return nil, nil, types.ForbiddenErrorf("endpoint %s does not exist for network %s", endpointID, dockerNetworkID)
	}

	return flannelNetwork, endpoint, nil
}

func (d *flannelDriver) handleServicesAdded(added []etcd.Item[common.ServiceInfo]) {
	for _, addedItem := range added {
		err := d.serviceLbsManagement.EnsureLoadBalancer(addedItem.Value)
		if err != nil {
			log.Printf("Failed to create load balancer for service %s: %v", addedItem.Value.ID, err)
		}
	}
}

func (d *flannelDriver) handleServicesChanged(changed []etcd.ItemChange[common.ServiceInfo]) {
	for _, changedItem := range changed {
		err := d.serviceLbsManagement.EnsureLoadBalancer(changedItem.Current)
		if err != nil {
			log.Printf("Failed to create load balancer for service %s: %v", changedItem.Current.ID, err)
		}
	}
}

func (d *flannelDriver) handleServicesRemoved(removed []etcd.Item[common.ServiceInfo]) {
	for _, removedItem := range removed {
		err := d.serviceLbsManagement.DeleteLoadBalancer(removedItem.ID)
		if err != nil {
			log.Printf("Failed to remove load balancer for service %s: %+v\n", removedItem.ID, err)
		}
	}
}

func (d *flannelDriver) getNetwork(dockerNetworkID string, flannelNetworkID string) (flannel_network.Network, bool) {
	network, exists := d.networksByDockerID[dockerNetworkID]
	if !exists {
		network, exists = d.networksByFlannelID[flannelNetworkID]
		if exists && dockerNetworkID != "" {
			d.networksByDockerID[dockerNetworkID] = network
		}
	} else if flannelNetworkID != "" {
		d.networksByFlannelID[flannelNetworkID] = network
	}

	return network, exists
}

func (d *flannelDriver) getOrCreateNetwork(dockerNetworkID string, flannelNetworkID string) (flannel_network.Network, error) {
	network, exists := d.getNetwork(dockerNetworkID, flannelNetworkID)
	if exists {
		return network, nil
	}

	networkSubnet, err := d.globalAddressSpace.GetNewOrExistingPool(flannelNetworkID)
	if err != nil {
		return nil, errors.WithMessagef(err, "failed to get network subnet pool for network '%s'", flannelNetworkID)
	}

	vni := d.vniStart + common.Max(len(d.networksByFlannelID), len(d.networksByDockerID)) + 1
	network, err = flannel_network.NewNetwork(d.getEtcdClient(common.SubnetToKey(networkSubnet.String())), flannelNetworkID, *networkSubnet, d.defaultHostSubnetSize, d.defaultFlannelOptions, vni)

	if err != nil {
		return nil, errors.WithMessagef(err, "failed to ensure network '%s' is operational", flannelNetworkID)
	}

	err = d.serviceLbsManagement.AddNetwork(dockerNetworkID, network)
	if err != nil {
		return nil, errors.WithMessagef(err, "Failed to add network '%s' to service load balancer management", flannelNetworkID)
	}

	d.networksByDockerID[dockerNetworkID] = network
	d.networksByFlannelID[flannelNetworkID] = network

	return network, nil
}

func (d *flannelDriver) handleNetworkAddedOrChanged(dockerNetworkID string, flannelNetworkID string) {
	_, err := d.getOrCreateNetwork(dockerNetworkID, flannelNetworkID)
	if err != nil {
		log.Printf("Failed to handle added or changed network %s / %s: %s", dockerNetworkID, flannelNetworkID, err)
	}
}

func (d *flannelDriver) handleNetworkRemoved(dockerNetworkID string, flannelNetworkID string) {
	network, exists := d.getNetwork(dockerNetworkID, flannelNetworkID)
	if exists {
		err := network.Delete()
		if err != nil {
			log.Printf("Failed to remove network '%s': %+v\n", flannelNetworkID, err)
		}
	}
	delete(d.networksByDockerID, dockerNetworkID)
	delete(d.networksByFlannelID, flannelNetworkID)
}

func (d *flannelDriver) handleContainersAdded(added []etcd.ShardItem[common.ContainerInfo]) {
	for _, addedItem := range added {
		containerInfo := addedItem.Value
		for dockerNetworkID, ipamIP := range containerInfo.IpamIPs {

			network, exists := d.networksByDockerID[dockerNetworkID]
			if !exists {
				continue
			}

			if !ipamIP.Equal(containerInfo.IPs[dockerNetworkID]) {
				if network.GetInfo().HostSubnet.Contains(ipamIP) {
					err := network.GetPool().ReleaseIP(ipamIP.String())
					if err != nil {
						log.Printf("Failed to release IPAM IP %s for network %s: %v", ipamIP.String(), dockerNetworkID, err)
					}
				}
			}
		}

		if containerInfo.ServiceID != "" {
			err := d.serviceLbsManagement.AddBackendIPsToLoadBalancer(containerInfo.ServiceID, containerInfo.IPs)
			if err != nil {
				log.Printf("error adding backend IPs to load balancer of service %s: %v", containerInfo.ServiceID, err)
			}
		}
	}
}

func (d *flannelDriver) handleContainersChanged(changed []etcd.ShardItemChange[common.ContainerInfo]) {
	for _, changedItem := range changed {
		log.Printf("Received container changed info for container %s on host %s. Previous: %+v, Current: %+v\n", changedItem.ID, changedItem.ShardKey, changedItem.Previous, changedItem.Current)
	}
}

func (d *flannelDriver) handleContainersRemoved(removed []etcd.ShardItem[common.ContainerInfo]) {
	for _, removedItem := range removed {
		containerInfo := removedItem.Value
		// This should be handled by IPAM
		//for dockerNetworkID, ip := range containerInfo.IPs {

		//network, exists := d.networksByDockerID[dockerNetworkID]
		//if !exists {
		//	continue
		//}

		//if network.GetInfo().HostSubnet.Contains(ip) {
		//	err := network.GetPool().ReleaseIP(ip.String())
		//	if err != nil {
		//		log.Printf("Failed to release IP %s for network %s: %v", ip.String(), dockerNetworkID, err)
		//	}
		//}
		//}

		if containerInfo.ServiceID != "" {
			d.serviceLbsManagement.RemoveBackendIPsFromLoadBalancer(containerInfo.ServiceID, containerInfo.IPs)
		}
	}
}

// TODO: Handle service created, changed and deleted: create, change (?) and delete load balancer
// Allocate and release IPs and fwmark here, not in service_lbs_management -> ???
// Store allocated IPs, use sharded distributed store for it

// TODO: Properly handle startup
