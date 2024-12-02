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
	Init() error
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
	completeAddressSpace  []net.IPNet
	networkSubnetSize     int
	vniStart              int
	isInitialized         bool
	sync.Mutex
}

func NewFlannelDriver(etcdEndPoints []string, etcdPrefix string, defaultFlannelOptions []string, completeSpace []net.IPNet, networkSubnetSize int, defaultHostSubnetSize int) FlannelDriver {

	driver := &flannelDriver{
		etcdPrefix:            etcdPrefix,
		etcdEndPoints:         etcdEndPoints,
		defaultFlannelOptions: defaultFlannelOptions,
		defaultHostSubnetSize: defaultHostSubnetSize,
		networksByFlannelID:   make(map[string]flannel_network.Network),
		networksByDockerID:    make(map[string]flannel_network.Network),
		vniStart:              6514,
		isInitialized:         false,
		completeAddressSpace:  completeSpace,
		networkSubnetSize:     networkSubnetSize,
	}

	numNetworks := countPoolSizeSubnets(completeSpace, networkSubnetSize)
	numIPsPerNode := int(math.Pow(2, float64(32-defaultHostSubnetSize)))
	numNodesPerNetwork := int(math.Pow(2, float64(defaultHostSubnetSize-networkSubnetSize)))
	fmt.Printf("The address space settings result in support for a total of %d docker networks, %d nodes and %d IP addresses per node and docker network (including service VIPs)\n", numNetworks, numNodesPerNetwork, numIPsPerNode)

	return driver
}

func (d *flannelDriver) IsInitialized() bool {
	return d.isInitialized
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

func (d *flannelDriver) Init() error {
	err := d.getEtcdClient("").WaitUntilAvailable(5*time.Second, 6)

	globalAddressSpace, err := ipam.NewEtcdBasedAddressSpace(d.completeAddressSpace, d.networkSubnetSize, d.getEtcdClient("address-space"))
	if err != nil {
		return errors.WithMessage(err, "Failed to create address space")
	}
	d.globalAddressSpace = globalAddressSpace
	fmt.Println("Initialized address space")

	containerCallbacks := etcd.ShardItemsHandlers[docker.ContainerInfo]{
		OnAdded:   d.handleContainersAdded,
		OnChanged: d.handleContainersChanged,
		OnRemoved: d.handleContainersRemoved,
	}

	serviceCallbacks := etcd.ItemsHandlers[docker.ServiceInfo]{
		OnAdded:   d.handleServicesAdded,
		OnChanged: d.handleServicesChanged,
		OnRemoved: d.handleServicesRemoved,
	}

	networkCallbacks := etcd.ItemsHandlers[docker.NetworkInfo]{
		OnAdded:   d.handleNetworksAdded,
		OnChanged: d.handleNetworksChanged,
		OnRemoved: d.handleNetworksRemoved,
	}

	serviceLbsManagement, err := service_lb.NewServiceLbManagement(d.getEtcdClient("service-lbs"))
	if err != nil {
		return errors.WithMessage(err, "Failed to create service lbs management")
	}
	d.serviceLbsManagement = serviceLbsManagement
	fmt.Println("Initialized service load balancer management")

	dockerData, err := docker.NewData(d.getEtcdClient("docker-data"), containerCallbacks, serviceCallbacks, networkCallbacks)
	if err != nil {
		return errors.WithMessage(err, "Failed to create docker data handler")
	}
	d.dockerData = dockerData

	err = dockerData.Init()
	if err != nil {
		return errors.WithMessage(err, "Failed to initialize docker data handler")
	}
	fmt.Println("Initialized docker data handler")

	d.isInitialized = true

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

func (d *flannelDriver) handleServicesAdded(added []etcd.Item[docker.ServiceInfo]) {
	for _, addedItem := range added {
		err := d.serviceLbsManagement.EnsureLoadBalancer(addedItem.Value)
		if err != nil {
			log.Printf("Failed to create load balancer for service %s: %v", addedItem.Value.ID, err)
		}
	}
}

func (d *flannelDriver) handleServicesChanged(changed []etcd.ItemChange[docker.ServiceInfo]) {
	for _, changedItem := range changed {
		err := d.serviceLbsManagement.EnsureLoadBalancer(changedItem.Current)
		if err != nil {
			log.Printf("Failed to create load balancer for service %s: %v", changedItem.Current.ID, err)
		}
	}
}

func (d *flannelDriver) handleServicesRemoved(removed []etcd.Item[docker.ServiceInfo]) {
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

func (d *flannelDriver) handleNetworksAdded(added []etcd.Item[docker.NetworkInfo]) {
	for _, addedItem := range added {
		networkInfo := addedItem.Value
		_, err := d.getOrCreateNetwork(networkInfo.DockerID, networkInfo.FlannelID)
		if err != nil {
			log.Printf("Failed to handle added or changed network %s / %s: %s", networkInfo.DockerID, networkInfo.FlannelID, err)
		}
	}
}

func (d *flannelDriver) handleNetworksChanged(changed []etcd.ItemChange[docker.NetworkInfo]) {
	for _, changedItem := range changed {
		networkInfo := changedItem.Current
		_, err := d.getOrCreateNetwork(networkInfo.DockerID, networkInfo.FlannelID)
		if err != nil {
			log.Printf("Failed to handle added or changed network %s / %s: %s", networkInfo.DockerID, networkInfo.FlannelID, err)
		}
	}
}

func (d *flannelDriver) handleNetworksRemoved(removed []etcd.Item[docker.NetworkInfo]) {
	for _, removedItem := range removed {
		networkInfo := removedItem.Value
		network, exists := d.getNetwork(networkInfo.DockerID, networkInfo.FlannelID)
		if exists {
			err := network.Delete()
			if err != nil {
				log.Printf("Failed to remove network '%s': %+v\n", networkInfo.FlannelID, err)
			}
		}
		delete(d.networksByDockerID, networkInfo.DockerID)
		delete(d.networksByFlannelID, networkInfo.FlannelID)
	}
}

func (d *flannelDriver) handleContainersAdded(added []etcd.ShardItem[docker.ContainerInfo]) {
	for _, addedItem := range added {
		containerInfo := addedItem.Value
		for dockerNetworkID, ipamIP := range containerInfo.IpamIPs {

			network, exists := d.networksByDockerID[dockerNetworkID]
			if !exists {
				continue
			}

			if !ipamIP.Equal(containerInfo.IPs[dockerNetworkID]) {
				if network.GetInfo().HostSubnet.Contains(ipamIP) {
					log.Printf("Releasing IPAM IP %s of container %s", ipamIP, containerInfo.ID)
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

func (d *flannelDriver) handleContainersChanged(changed []etcd.ShardItemChange[docker.ContainerInfo]) {
	for _, changedItem := range changed {
		log.Printf("Received container changed info for container %s on host %s. Previous: %+v, Current: %+v\n", changedItem.ID, changedItem.ShardKey, changedItem.Previous, changedItem.Current)
	}
}

func (d *flannelDriver) handleContainersRemoved(removed []etcd.ShardItem[docker.ContainerInfo]) {
	for _, removedItem := range removed {
		containerInfo := removedItem.Value

		if containerInfo.ServiceID != "" {
			err := d.serviceLbsManagement.RemoveBackendIPsFromLoadBalancer(containerInfo.ServiceID, containerInfo.IPs)
			if err != nil {
				log.Printf("Error removing backend IPs from load balancer of service %s: %v\n", containerInfo.ServiceID, err)
			}
		}
	}
}

// TODO: Handle service created, changed and deleted: create, change (?) and delete load balancer
// Allocate and release IPs and fwmark here, not in service_lbs_management -> ???
// Store allocated IPs, use sharded distributed store for it

// TODO: Properly handle startup

func countPoolSizeSubnets(completeSpace []net.IPNet, poolSize int) int {
	total := 0

	for _, subnet := range completeSpace {
		maskSize, bits := subnet.Mask.Size()

		if maskSize > poolSize || bits != 32 {
			continue
		}

		delta := poolSize - maskSize
		subnets := 1 << delta

		total += subnets
	}

	return total
}
