package driver

import (
	"fmt"
	"github.com/docker/docker/libnetwork/types"
	"github.com/docker/go-plugins-helpers/network"
	"github.com/pkg/errors"
	"log"
	"net"
	"os"
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
func WaitForSandboxAndConfigure(sandboxKey string, timeout time.Duration, configure func() error) error {
	start := time.Now()

	for {
		// Check if the sandbox key file exists
		if _, err := os.Stat(sandboxKey); err == nil {
			// File exists, apply the configuration
			if configureErr := configure(); configureErr != nil {
				return fmt.Errorf("failed to configure namespace: %w", configureErr)
			}
			fmt.Println("Sandbox configured successfully")
			return nil
		}

		// Check if timeout has been reached
		if time.Since(start) > timeout {
			return fmt.Errorf("timeout reached while waiting for sandbox key: %s", sandboxKey)
		}

		// Sleep briefly before retrying
		time.Sleep(5 * time.Millisecond)
	}
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

	go func() {
		// The node path /var/run/docker is mounted to /hostfs/var/run/docker and the sandbox keys
		// are of form /var/run/docker/netns/<key>
		sandboxKey := adjustSandboxKey(request.SandboxKey)
		err := WaitForSandboxAndConfigure(sandboxKey, 10*time.Second, func() error {
			start := time.Now()
			defer func() {
				fmt.Printf("Execution time: %s\n", time.Since(start))
			}()
			nameserver, err := d.getOrAddNameserver(sandboxKey)
			if err != nil {
				return err
			}

			d.nameserversByEndpointID.Set(request.EndpointID, nameserver)
			nameserver.AddValidNetworkID(request.NetworkID)

			return nil
		})
		if err != nil {
			log.Printf("Error patching DNS server in sandbox %s: %v\n", sandboxKey, err)
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
