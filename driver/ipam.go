package driver

import (
	"errors"
	"fmt"
	"github.com/docker/go-plugins-helpers/ipam"
	"log"
	"strings"
)

func (d *FlannelDriver) RequestPool(request *ipam.RequestPoolRequest) (*ipam.RequestPoolResponse, error) {
	if request.V6 {
		return nil, errors.New("flannel plugin does not support ipv6")
	}

	poolID := "FlannelPool"
	flannelNetworkId, exists := request.Options["id"]
	if exists && flannelNetworkId != "" {
		poolID = fmt.Sprintf("%s-%s", poolID, flannelNetworkId)
	} else {
		return nil, errors.New("the IPAM driver option 'id' needs to be set to a unique ID")
	}

	network, err := d.ensureFlannelIsConfiguredAndRunning(flannelNetworkId)
	if err != nil {
		return nil, err
	}

	return &ipam.RequestPoolResponse{PoolID: poolID, Pool: network.config.Network}, nil
}

func (d *FlannelDriver) ReleasePool(request *ipam.ReleasePoolRequest) error {
	//TODO implement me
	//panic("implement me")
	return nil
}

func (d *FlannelDriver) RequestAddress(request *ipam.RequestAddressRequest) (*ipam.RequestAddressResponse, error) {
	flannelNetworkId := strings.Join(strings.Split(request.PoolID, "-")[1:], "-")

	network, err := d.ensureFlannelIsConfiguredAndRunning(flannelNetworkId)
	if err != nil {
		return nil, err
	}

	requestType, exists := request.Options["RequestAddressType"]
	if exists && requestType == "com.docker.network.gateway" {
		return &ipam.RequestAddressResponse{Address: fmt.Sprintf("%s/32", network.config.Gateway)}, nil
	}

	mac := request.Options["com.docker.network.endpoint.macaddress"]

	address, err := d.etcdClient.ReserveAddress(network, request.Address, mac)

	if err != nil {
		log.Printf("Failed to reserve address for network %s: %+v", flannelNetworkId, err)
		return nil, err
	}

	return &ipam.RequestAddressResponse{Address: fmt.Sprintf("%s/32", address)}, nil
}

func (d *FlannelDriver) ReleaseAddress(request *ipam.ReleaseAddressRequest) error {
	//TODO implement me
	//panic("implement me")
	return nil
}
