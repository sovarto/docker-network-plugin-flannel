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
	networks              map[string]flannel_network.Network
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
		networks:              make(map[string]flannel_network.Network),
	}

	globalAddressSpace, err := ipam.NewEtcdBasedAddressSpace(completeSpace, networkSubnetSize, driver.getEtcdClient("address-space"))
	if err != nil {
		return nil, errors.Wrap(err, "Failed to create address space")
	}
	driver.globalAddressSpace = globalAddressSpace

	fmt.Println("Initialized address space")

	callbacks := docker.Callbacks{
		ContainerChanged: driver.handleContainerChanged,
		ContainerAdded:   driver.handleContainerAdded,
		ContainerRemoved: driver.handleContainerRemoved,
		ServiceChanged:   driver.handleServiceChanged,
		ServiceAdded:     driver.handleServiceAdded,
		ServiceRemoved:   driver.handleServiceRemoved,
		NetworkAdded:     driver.handleNetworkAdded,
		NetworkRemoved:   driver.handleNetworkRemoved,
	}

	serviceLbsManagement := service_lb.NewServiceLbManagement(driver.getEtcdClient("service-lbs"))
	driver.serviceLbsManagement = serviceLbsManagement
	fmt.Println("Initialized service load balancer management")

	dockerData, err := docker.NewData(driver.getEtcdClient("docker-data"), callbacks)
	if err != nil {
		return nil, errors.Wrap(err, "Failed to create docker data handler")
	}
	driver.dockerData = dockerData
	fmt.Println("Initialized docker data handler")

	return driver, nil
}

func (d *flannelDriver) Serve() error {
	handler := sdk.NewHandler(`{"Implements": ["IpamDriver", "NetworkDriver"]}`)
	api.InitIpamMux(&handler, d)
	api.InitNetworkMux(&handler, d)

	if err := handler.ServeUnix("flannel-np", 0); err != nil {
		return errors.Wrapf(err, "Failed to start flannel plugin server")
	}

	return nil
}

func (d *flannelDriver) getEtcdClient(prefix string) etcd.Client {
	return etcd.NewEtcdClient(d.etcdEndPoints, 5*time.Second, fmt.Sprintf("%s/%s", d.etcdPrefix, prefix))
}

func (d *flannelDriver) getFlannelNetworkFromDockerNetworkID(networkID string) (flannel_network.Network, error) {
	flannelNetworkID, err := d.dockerData.GetFlannelNetworkID(networkID)
	if err != nil {
		return nil, err
	}

	flannelNetwork, exists := d.networks[flannelNetworkID]

	if !exists {
		return nil, fmt.Errorf("no flannel network found for ID %s", flannelNetworkID)
	}

	return flannelNetwork, nil
}

func (d *flannelDriver) getEndpoint(networkID, endpointID string) (flannel_network.Network, flannel_network.Endpoint, error) {
	flannelNetwork, err := d.getFlannelNetworkFromDockerNetworkID(networkID)
	if err != nil {
		return nil, nil, err
	}

	endpoint := flannelNetwork.GetEndpoint(endpointID)

	if endpoint == nil {
		return nil, nil, types.ForbiddenErrorf("endpoint %s does not exist for network %s", endpointID, networkID)
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
		log.Printf("Failed to remove load balancer for service %s: %+v", serviceID, err)
	}
}

func (d *flannelDriver) handleNetworkAdded(networkID string) {
	//poolID := "FlannelPool"
	//
	//networkSubnet, err := d.globalAddressSpace.GetNewOrExistingPool()
	//if err != nil {
	//	return nil, errors.Wrapf(err, "failed to get network subnet pool for network '%s'", flannelNetworkId)
	//}
	//
	//network, err := flannel_network.NewNetwork(d.getEtcdClient(common.SubnetToKey(networkSubnet.String())), flannelNetworkId, *networkSubnet, d.defaultHostSubnetSize, d.defaultFlannelOptions)
	//
	//if err != nil {
	//	return nil, errors.Wrapf(err, "failed to ensure network '%s' is operational", flannelNetworkId)
	//}
	//
	//d.networks[flannelNetworkId] = network
}

func (d *flannelDriver) handleNetworkRemoved(networkID string) {

}

func (d *flannelDriver) handleContainerAdded(containerInfo common.ContainerInfo) {
	for networkID, ipamIP := range containerInfo.IpamIPs {
		network, exists := d.networks[networkID]
		if !exists {
			continue
		}

		if !ipamIP.Equal(containerInfo.IPs[networkID]) {
			if network.GetInfo().HostSubnet.Contains(ipamIP) {
				err := network.GetPool().ReleaseIP(ipamIP.String())
				if err != nil {
					log.Printf("Failed to release IPAM IP %s for network %s: %v", ipamIP.String(), networkID, err)
				}
			}
		}
	}
}

func (d *flannelDriver) handleContainerChanged(previousContainerInfo *common.ContainerInfo, currentContainerInfo common.ContainerInfo) {

}

func (d *flannelDriver) handleContainerRemoved(containerInfo common.ContainerInfo) {
	for networkID, ip := range containerInfo.IPs {
		network, exists := d.networks[networkID]
		if !exists {
			continue
		}

		if network.GetInfo().HostSubnet.Contains(ip) {
			err := network.GetPool().ReleaseIP(ip.String())
			if err != nil {
				log.Printf("Failed to release IP %s for network %s: %v", ip.String(), networkID, err)
			}
		}
	}
}
