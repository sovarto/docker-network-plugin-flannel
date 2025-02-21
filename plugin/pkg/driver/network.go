package driver

import (
	"fmt"
	"github.com/docker/docker/libnetwork/types"
	"github.com/docker/go-plugins-helpers/network"
	"github.com/pkg/errors"
	"log"
	"net"
	"time"
)

func (d *flannelDriver) GetCapabilities() (*network.CapabilitiesResponse, error) {
	return &network.CapabilitiesResponse{
		Scope:             "global",
		ConnectivityScope: "global",
	}, nil
}

func (d *flannelDriver) CreateNetwork(request *network.CreateNetworkRequest) error {
	return nil
}

func (d *flannelDriver) AllocateNetwork(request *network.AllocateNetworkRequest) (*network.AllocateNetworkResponse, error) {
	return &network.AllocateNetworkResponse{}, nil
}

func (d *flannelDriver) DeleteNetwork(request *network.DeleteNetworkRequest) error {
	return nil
}

func (d *flannelDriver) FreeNetwork(request *network.FreeNetworkRequest) error {
	return nil
}

func (d *flannelDriver) CreateEndpoint(request *network.CreateEndpointRequest) (*network.CreateEndpointResponse, error) {
	d.Lock()
	defer d.Unlock()

	if request.Interface == nil || request.Interface.Address == "" || request.Interface.MacAddress == "" {
		log.Println("Received no interface info or interface info without address or mac address. This is not supported")
		return nil, types.InvalidParameterErrorf("Need interface info with IPv4 address and MAC address as input for endpoint %s for network %s.", request.EndpointID, request.NetworkID)
	}

	flannelNetwork, exists, _ := d.networks.Get(networkKey{dockerID: request.NetworkID})
	if !exists {
		return nil, fmt.Errorf("network %s is missing in internal state", request.NetworkID)
	}

	ip, _, err := net.ParseCIDR(request.Interface.Address)
	if err != nil {
		return nil, errors.WithMessagef(err, "failed to parse IP address %s", request.Interface.Address)
	}
	_, err = flannelNetwork.AddEndpoint(request.EndpointID, ip, request.Interface.MacAddress)
	if err != nil {
		return nil, errors.WithMessagef(err, "failed to create endpoint %s for flannel network %s", request.EndpointID, flannelNetwork.GetInfo().FlannelID)
	}

	// Don't return the interface we got passed in. Even without changing any values, it will lead
	// to an error, saying values can't be changed
	return &network.CreateEndpointResponse{}, nil
}

func (d *flannelDriver) DeleteEndpoint(request *network.DeleteEndpointRequest) error {
	d.Lock()
	defer d.Unlock()

	flannelNetwork, exists, _ := d.networks.Get(networkKey{dockerID: request.NetworkID})
	if !exists {
		return fmt.Errorf("network %s is missing in internal state", request.NetworkID)
	}

	err := flannelNetwork.DeleteEndpoint(request.EndpointID)
	if err != nil {
		return errors.WithMessagef(err, "failed to delete endpoint %s", request.EndpointID)
	}

	return nil
}

func (d *flannelDriver) EndpointInfo(request *network.InfoRequest) (*network.InfoResponse, error) {
	d.Lock()
	defer d.Unlock()

	_, endpoint, err := d.getEndpoint(request.NetworkID, request.EndpointID)

	if err != nil {
		return nil, err
	}

	value := make(map[string]string)

	value["ip_address"] = endpoint.GetInfo().IpAddress.String()
	value["mac_address"] = endpoint.GetInfo().MacAddress

	resp := &network.InfoResponse{
		Value: value,
	}

	return resp, nil
}

func (d *flannelDriver) Join(request *network.JoinRequest) (*network.JoinResponse, error) {
	d.Lock()
	defer d.Unlock()

	flannelNetwork, endpoint, err := d.getEndpoint(request.NetworkID, request.EndpointID)

	if err != nil {
		return nil, err
	}

	err = endpoint.Join(request.SandboxKey)
	if err != nil {
		return nil, errors.WithMessagef(err, "failed to join endpoint %s to network %s", request.EndpointID, request.NetworkID)
	}
	networkInfo := flannelNetwork.GetInfo()
	endpointInfo := endpoint.GetInfo()

	start := time.Now()
	nameserver, errChan := d.getOrAddNameserver(request.SandboxKey)
	nameserver.AddValidNetworkID(request.NetworkID)

	go func() {
		if err := <-errChan; err != nil {
			log.Printf("Error patching DNS server in sandbox %s. Wait time %s. Error: %v\n", request.SandboxKey, time.Since(start), err)
		} else {
			d.nameserversByEndpointID.Set(request.EndpointID, nameserver)
			fmt.Printf("Wait time until nameserver in namespace %s was setup: %s\n", request.SandboxKey, time.Since(start))
		}
	}()

	return &network.JoinResponse{
		InterfaceName: network.InterfaceName{
			SrcName:   endpointInfo.VethInside,
			DstPrefix: "eth",
		},
		// TODO: Check if using Gateway instead of StaticRoutes also works
		StaticRoutes: []*network.StaticRoute{
			{
				Destination: networkInfo.Network.String(),
				RouteType:   types.NEXTHOP,
				NextHop:     networkInfo.LocalGateway.String(),
			},
		},
		DisableGatewayService: false,
	}, nil
}

func (d *flannelDriver) Leave(request *network.LeaveRequest) error {
	d.Lock()
	defer d.Unlock()

	_, endpoint, err := d.getEndpoint(request.NetworkID, request.EndpointID)

	if err != nil {
		return errors.WithMessagef(err, "failed to get endpoint %s", request.EndpointID)
	}

	err = endpoint.Leave()
	if err != nil {
		return errors.WithMessagef(err, "failed to leave endpoint %s", request.EndpointID)
	}

	nameserver, wasRemoved := d.nameserversByEndpointID.TryRemove(request.EndpointID)
	if wasRemoved {
		nameserver.RemoveValidNetworkID(request.NetworkID)
	}

	return nil
}

func (d *flannelDriver) DiscoverNew(notification *network.DiscoveryNotification) error {
	return nil
}

func (d *flannelDriver) DiscoverDelete(notification *network.DiscoveryNotification) error {
	return nil
}

func (d *flannelDriver) ProgramExternalConnectivity(request *network.ProgramExternalConnectivityRequest) error {
	return nil
}

func (d *flannelDriver) RevokeExternalConnectivity(request *network.RevokeExternalConnectivityRequest) error {
	return nil
}
