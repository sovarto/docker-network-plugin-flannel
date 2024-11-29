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
	}

	globalAddressSpace, err := ipam.NewEtcdBasedAddressSpace(completeSpace, networkSubnetSize, driver.getEtcdClient("address-space"))
	if err != nil {
		return nil, errors.WithMessage(err, "Failed to create address space")
	}
	driver.globalAddressSpace = globalAddressSpace

	numIPsPerNode := int(math.Pow(2, float64(32-defaultHostSubnetSize)))
	numNodesPerNetwork := int(math.Pow(2, float64(defaultHostSubnetSize-networkSubnetSize)))
	fmt.Printf("The address space settings results in support for a total of %d nodes and %d IP addresses per node and docker network (including service VIPs)\n", numNodesPerNetwork, numIPsPerNode)
	fmt.Println("Initialized address space")

	callbacks := docker.Callbacks{
		ContainerChanged: driver.handleContainerChanged,
		ContainerAdded:   driver.handleContainerAdded,
		ContainerRemoved: driver.handleContainerRemoved,
		ServiceChanged:   driver.handleServiceChanged,
		ServiceAdded:     driver.handleServiceAdded,
		ServiceRemoved:   driver.handleServiceRemoved,
		NetworkAdded:     driver.handleNetworkAddedOrChanged,
		NetworkChanged:   driver.handleNetworkAddedOrChanged,
		NetworkRemoved:   driver.handleNetworkRemoved,
	}

	serviceLbsManagement := service_lb.NewServiceLbManagement(driver.getEtcdClient("service-lbs"))
	driver.serviceLbsManagement = serviceLbsManagement
	fmt.Println("Initialized service load balancer management")

	dockerData, err := docker.NewData(driver.getEtcdClient("docker-data"), callbacks)
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

func (d *flannelDriver) handleServiceChanged(previousServiceInfo *common.ServiceInfo, currentServiceInfo common.ServiceInfo) {
	err := d.serviceLbsManagement.UpdateLoadBalancer(currentServiceInfo)
	if err != nil {
		log.Printf("Failed to update load balancer for service %s: %v", currentServiceInfo.ID, err)
	}
}

func (d *flannelDriver) handleServiceAdded(serviceInfo common.ServiceInfo) {
	err := d.serviceLbsManagement.CreateLoadBalancer(serviceInfo)
	if err != nil {
		log.Printf("Failed to create load balancer for service %s: %v", serviceInfo.ID, err)
	}
}

func (d *flannelDriver) handleServiceRemoved(serviceID string) {
	err := d.serviceLbsManagement.DeleteLoadBalancer(serviceID)
	if err != nil {
		log.Printf("Failed to remove load balancer for service %s: %+v\n", serviceID, err)
	}
}

func (d *flannelDriver) handleNetworkAddedOrChanged(dockerNetworkID string, flannelNetworkID string) {

	fmt.Printf("handleNetworkAddedOrChanged: dockerNetworkID: %, flannelNetworkID: %s\n", dockerNetworkID, flannelNetworkID)
	network, exists := d.networksByDockerID[dockerNetworkID]
	if !exists {
		network, exists = d.networksByFlannelID[flannelNetworkID]
	}
	if !exists {
		if flannelNetworkID != "" {
			networkSubnet, err := d.globalAddressSpace.GetNewOrExistingPool(flannelNetworkID)
			if err != nil {
				log.Printf("failed to get network subnet pool for network '%s': %+v\n", flannelNetworkID, err)
				return
			}

			network, err = flannel_network.NewNetwork(d.getEtcdClient(common.SubnetToKey(networkSubnet.String())), flannelNetworkID, *networkSubnet, d.defaultHostSubnetSize, d.defaultFlannelOptions)

			if err != nil {
				log.Printf("failed to ensure network '%s' is operational: %+v\n", flannelNetworkID, err)
				return
			}

			err = d.serviceLbsManagement.AddNetwork(dockerNetworkID, network)
			if err != nil {
				log.Printf("Failed to add network '%s' to service load balancer management: %+v\n", flannelNetworkID, err)
			}
		} else {
			log.Printf("network changed or added with docker ID %s but without flannel network ID. This shouldn't happen\n", dockerNetworkID)
		}
	}

	d.networksByDockerID[dockerNetworkID] = network
	d.networksByFlannelID[flannelNetworkID] = network
}

func (d *flannelDriver) handleNetworkRemoved(dockerNetworkID string, flannelNetworkID string) {
	delete(d.networksByDockerID, dockerNetworkID)
	delete(d.networksByFlannelID, flannelNetworkID)
}

func (d *flannelDriver) handleContainerAdded(containerInfo common.ContainerInfo) {
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
}

func (d *flannelDriver) handleContainerChanged(previousContainerInfo *common.ContainerInfo, currentContainerInfo common.ContainerInfo) {

}

func (d *flannelDriver) handleContainerRemoved(containerInfo common.ContainerInfo) {
	for dockerNetworkID, ip := range containerInfo.IPs {

		network, exists := d.networksByDockerID[dockerNetworkID]
		if !exists {
			continue
		}

		if network.GetInfo().HostSubnet.Contains(ip) {
			err := network.GetPool().ReleaseIP(ip.String())
			if err != nil {
				log.Printf("Failed to release IP %s for network %s: %v", ip.String(), dockerNetworkID, err)
			}
		}
	}
}
