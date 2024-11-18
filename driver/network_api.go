package driver

import (
	"github.com/docker/go-plugins-helpers/network"
	"log"
)

type FlannelNetworkPlugin struct {
	driver *FlannelDriver
}

func (k *FlannelNetworkPlugin) GetCapabilities() (*network.CapabilitiesResponse, error) {
	log.Printf("Received GetCapabilities req")

	capabilities := &network.CapabilitiesResponse{
		Scope:             "global",
		ConnectivityScope: "global",
	}
	log.Printf("CreateNetwork response: %+v\n", capabilities)
	return capabilities, nil
}

func (k *FlannelNetworkPlugin) CreateNetwork(req *network.CreateNetworkRequest) error {
	log.Printf("Received CreateNetwork req: %+v\n", req)

	err := k.driver.CreateNetwork(req)

	log.Printf("CreateNetwork response: %+v\n", err)

	return err
}

func (k *FlannelNetworkPlugin) DeleteNetwork(req *network.DeleteNetworkRequest) error {
	log.Printf("Received DeleteNetwork req: %+v\n", req)

	err := k.driver.DeleteNetwork(req)

	log.Printf("CreateNetwork response: %+v\n", err)

	return err
}

func (k *FlannelNetworkPlugin) AllocateNetwork(req *network.AllocateNetworkRequest) (*network.AllocateNetworkResponse, error) {
	log.Printf("Received AllocateNetwork req: %+v\n", req)

	// This happens during docker network create
	// CreateNetwork happens when a container is being started that uses this network
	// Maybe start flannel process?

	return nil, nil
}

func (k *FlannelNetworkPlugin) FreeNetwork(req *network.FreeNetworkRequest) error {
	log.Printf("Received FreeNetwork req: %+v\n", req)

	// Maybe stop flannel process?

	return nil
}

func (k *FlannelNetworkPlugin) CreateEndpoint(req *network.CreateEndpointRequest) (*network.CreateEndpointResponse, error) {
	log.Printf("Received CreateEndpoint req: %+v\n", req)

	response, err := k.driver.CreateEndpoint(req)
	log.Printf("CreateEndpoint response: %+v; error:%+v\n", response, err)
	return response, err
}

func (k *FlannelNetworkPlugin) DeleteEndpoint(req *network.DeleteEndpointRequest) error {
	log.Printf("Received DeleteEndpoint req: %+v\n", req)

	err := k.driver.DeleteEndpoint(req)

	log.Printf("DeleteEndpoint response: %+v\n", err)

	return err
}

func (k *FlannelNetworkPlugin) EndpointInfo(req *network.InfoRequest) (*network.InfoResponse, error) {
	log.Printf("Received EndpointOperInfo req: %+v\n", req)
	response, err := k.driver.EndpointInfo(req)
	log.Printf("EndpointInfo response: %+v; error:%+v\n", response, err)
	return response, err

}

func (k *FlannelNetworkPlugin) Join(req *network.JoinRequest) (*network.JoinResponse, error) {
	log.Printf("Received Join req: %+v\n", req)
	response, err := k.driver.Join(req)
	log.Printf("Join response: %+v; error:%+v\n", response, err)
	return response, err

}

func (k *FlannelNetworkPlugin) Leave(req *network.LeaveRequest) error {
	log.Printf("Received Leave req: %+v\n", req)
	err := k.driver.Leave(req)

	log.Printf("Leave response: %+v\n", err)

	return err
}

func (k *FlannelNetworkPlugin) DiscoverNew(req *network.DiscoveryNotification) error {
	log.Printf("Received DiscoverNew req: %+v\n", req)

	return nil
}

func (k *FlannelNetworkPlugin) DiscoverDelete(req *network.DiscoveryNotification) error {
	log.Printf("Received DiscoverDelete req: %+v\n", req)

	return nil
}

func (k *FlannelNetworkPlugin) ProgramExternalConnectivity(req *network.ProgramExternalConnectivityRequest) error {
	log.Printf("Received ProgramExternalConnectivity req: %+v\n", req)

	return nil
}

func (k *FlannelNetworkPlugin) RevokeExternalConnectivity(req *network.RevokeExternalConnectivityRequest) error {
	log.Printf("Received RevokeExternalConnectivity req: %+v\n", req)

	return nil
}
