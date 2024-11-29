package driver

import (
	"context"
	"fmt"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/client"
	dockerIpam "github.com/docker/go-plugins-helpers/ipam"
	"github.com/pkg/errors"
	"github.com/samber/lo"
	"github.com/sovarto/FlannelNetworkPlugin/pkg/common"
	"github.com/sovarto/FlannelNetworkPlugin/pkg/flannel_network"
	"github.com/sovarto/FlannelNetworkPlugin/pkg/ipam"
	"log"
	"strings"
)

func poolIDtoNetworkID(poolID string) string {
	return strings.Join(strings.Split(poolID, "-")[1:], "-")
}

func (d *flannelDriver) GetIpamCapabilities() (*dockerIpam.CapabilitiesResponse, error) {
	return &dockerIpam.CapabilitiesResponse{RequiresMACAddress: true}, nil
}

func (d *flannelDriver) GetDefaultAddressSpaces() (*dockerIpam.AddressSpacesResponse, error) {
	return &dockerIpam.AddressSpacesResponse{
		LocalDefaultAddressSpace:  "FlannelLocal",
		GlobalDefaultAddressSpace: "FlannelGlobal",
	}, nil
}

func (d *flannelDriver) RequestPool(request *dockerIpam.RequestPoolRequest) (*dockerIpam.RequestPoolResponse, error) {
	d.Lock()
	defer d.Unlock()

	if request.V6 {
		return nil, errors.New("flannel plugin does not support ipv6")
	}

	poolID := "FlannelPool"
	flannelNetworkId, exists := request.Options["flannel-id"]
	if exists && flannelNetworkId != "" {
		poolID = fmt.Sprintf("%s-%s", poolID, flannelNetworkId)
	} else {
		return nil, errors.New("the IPAM driver option 'flannel-id' needs to be set to a unique ID")
	}
	dockerClient, err := client.NewClientWithOpts(
		client.WithHost("unix:///var/run/docker.sock"),
		client.WithAPIVersionNegotiation(),
	)

	networks, err := dockerClient.NetworkList(context.Background(), network.ListOptions{})
	fmt.Printf("Networks: %+v", lo.Map(networks, func(item network.Summary, index int) string {
		return fmt.Sprintf("%s: %+v", item.Name, item.IPAM.Options)
	}))

	networkSubnet, err := d.globalAddressSpace.GetNewOrExistingPool(flannelNetworkId)
	if err != nil {
		return nil, errors.Wrapf(err, "failed to get network subnet pool for network '%s'", flannelNetworkId)
	}

	network, err := flannel_network.NewNetwork(d.getEtcdClient(common.SubnetToKey(networkSubnet.String())), flannelNetworkId, *networkSubnet, d.defaultHostSubnetSize, d.defaultFlannelOptions)

	if err != nil {
		return nil, errors.Wrapf(err, "failed to ensure network '%s' is operational", flannelNetworkId)
	}

	d.networks[flannelNetworkId] = network

	return &dockerIpam.RequestPoolResponse{
		PoolID: poolID,
		Pool:   networkSubnet.String(),
	}, nil
}

func (d *flannelDriver) ReleasePool(request *dockerIpam.ReleasePoolRequest) error {
	d.Lock()
	defer d.Unlock()

	flannelNetworkID := poolIDtoNetworkID(request.PoolID)
	network, exists := d.networks[flannelNetworkID]
	if !exists {
		return fmt.Errorf("no network found for pool '%s'", request.PoolID)
	}

	err := network.Delete()

	if err != nil {
		return errors.Wrapf(err, "failed to delete network '%s'", flannelNetworkID)
	}

	err = d.globalAddressSpace.ReleasePool(request.PoolID)

	if err != nil {
		return errors.Wrapf(err, "failed to release address pool of network '%s'", flannelNetworkID)
	}

	delete(d.networks, flannelNetworkID)

	return nil
}

func (d *flannelDriver) RequestAddress(request *dockerIpam.RequestAddressRequest) (*dockerIpam.RequestAddressResponse, error) {
	d.Lock()
	defer d.Unlock()

	flannelNetworkID := poolIDtoNetworkID(request.PoolID)
	network, exists := d.networks[flannelNetworkID]
	if !exists {
		return nil, fmt.Errorf("no network found for pool '%s'", request.PoolID)
	}

	networkInfo := network.GetInfo()

	requestType, exists := request.Options["RequestAddressType"]
	if exists && requestType == "com.docker.network.gateway" {
		return &dockerIpam.RequestAddressResponse{Address: fmt.Sprintf("%s/32", networkInfo.LocalGateway)}, nil
	}

	mac := request.Options["com.docker.network.endpoint.macaddress"]

	reservationType := ipam.ReservationTypeReserved
	if request.Address != "" && mac != "" {
		reservationType = ipam.ReservationTypeContainerIP
	}
	address, err := network.GetPool().AllocateIP(request.Address, mac, reservationType, true)

	if err != nil {
		log.Printf("Failed to reserve address for network %s: %+v", flannelNetworkID, err)
		return nil, err
	}
	ones, _ := networkInfo.HostSubnet.Mask.Size()
	return &dockerIpam.RequestAddressResponse{Address: fmt.Sprintf("%s/%d", address, ones)}, nil
}

func (d *flannelDriver) ReleaseAddress(request *dockerIpam.ReleaseAddressRequest) error {
	d.Lock()
	defer d.Unlock()

	flannelNetworkID := poolIDtoNetworkID(request.PoolID)
	network, exists := d.networks[flannelNetworkID]
	if !exists {
		return fmt.Errorf("no network found for pool '%s'", request.PoolID)
	}
	return network.GetPool().ReleaseIP(request.Address)
}
