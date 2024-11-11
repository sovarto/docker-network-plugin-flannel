package main

import (
	"github.com/docker/docker/libnetwork/types"
	"github.com/docker/go-plugins-helpers/network"
	"log"
	"net"
	"sync"
)

var (
	PLUGIN_NAME = "katharanp"
	PLUGIN_GUID = 0
)

type flannelEndpoint struct {
	macAddress net.HardwareAddr
}

type flannelNetwork struct {
	bridgeName string
	endpoints  map[string]*flannelEndpoint
}

type FlannelNetworkPlugin struct {
	networks map[string]*flannelNetwork
	sync.Mutex
}

func (k *FlannelNetworkPlugin) GetCapabilities() (*network.CapabilitiesResponse, error) {
	log.Printf("Received GetCapabilities req")

	capabilities := &network.CapabilitiesResponse{
		Scope:             "local",
		ConnectivityScope: "global",
	}

	return capabilities, nil
}

func (k *FlannelNetworkPlugin) CreateNetwork(req *network.CreateNetworkRequest) error {
	log.Printf("Received CreateNetwork req:\n%+v\n", req)

	k.Lock()
	defer k.Unlock()

	if _, ok := k.networks[req.NetworkID]; ok {
		return types.ForbiddenErrorf("network %s exists", req.NetworkID)
	}

	// Check etcd for /<options.prefix>/<req.NetworkID>/config
	//   Create if it is missing
	//   Ensure it matches the supplied configuration
	// Start flannel process:
	//   flanneld -subnet-file=/flannel-env/<req.NetworkID>.env -etcd-prefix=/<options.prefix>/<req.NetworkID> -iface=<options.interfaceName>

	flannelNetwork := &flannelNetwork{
		endpoints: make(map[string]*flannelEndpoint),
	}

	k.networks[req.NetworkID] = flannelNetwork

	return nil
}

func (k *FlannelNetworkPlugin) DeleteNetwork(req *network.DeleteNetworkRequest) error {
	log.Printf("Received DeleteNetwork req:\n%+v\n", req)

	k.Lock()
	defer k.Unlock()

	/* Skip if not in map */
	if _, ok := k.networks[req.NetworkID]; !ok {
		return nil
	}

	// Stop flannel process
	// Delete /<options.prefix>/<req.NetworkID> and all children from etcd

	delete(k.networks, req.NetworkID)

	return nil
}

func (k *FlannelNetworkPlugin) AllocateNetwork(req *network.AllocateNetworkRequest) (*network.AllocateNetworkResponse, error) {
	log.Printf("Received AllocateNetwork req:\n%+v\n", req)

	// Maybe start flannel process?

	return nil, nil
}

func (k *FlannelNetworkPlugin) FreeNetwork(req *network.FreeNetworkRequest) error {
	log.Printf("Received FreeNetwork req:\n%+v\n", req)

	// Maybe stop flannel process?

	return nil
}

func (k *FlannelNetworkPlugin) CreateEndpoint(req *network.CreateEndpointRequest) (*network.CreateEndpointResponse, error) {
	log.Printf("Received CreateEndpoint req:\n%+v\n", req)

	k.Lock()
	defer k.Unlock()

	/* Throw error if not in map */
	if _, ok := k.networks[req.NetworkID]; !ok {
		return nil, types.ForbiddenErrorf("%s network does not exist", req.NetworkID)
	}

	interfaceInfo := new(network.EndpointInterface)

	if req.Options["kathara.mac_addr"] != nil {
		// Use a pre-defined MAC Address passed by the user
		interfaceInfo.MacAddress = req.Options["kathara.mac_addr"].(string)
	} else if req.Options["kathara.machine"] != nil && req.Options["kathara.iface"] != nil {
		// Generate the interface MAC Address by concatenating the machine name and the interface idx
		interfaceInfo.MacAddress = generateMacAddressFromID(req.Options["kathara.machine"].(string) + "-" + req.Options["kathara.iface"].(string))
	} else if req.Interface == nil {
		// Generate the interface MAC Address by concatenating the network id and the endpoint id
		interfaceInfo.MacAddress = generateMacAddressFromID(req.NetworkID + "-" + req.EndpointID)
	}

	// Should we set the IP address here? Should we record it somewhere?

	parsedMac, _ := net.ParseMAC(interfaceInfo.MacAddress)

	endpoint := &flannelEndpoint{
		macAddress: parsedMac,
	}

	k.networks[req.NetworkID].endpoints[req.EndpointID] = endpoint

	resp := &network.CreateEndpointResponse{
		Interface: interfaceInfo,
	}

	return resp, nil
}

func (k *FlannelNetworkPlugin) DeleteEndpoint(req *network.DeleteEndpointRequest) error {
	log.Printf("Received DeleteEndpoint req:\n%+v\n", req)

	k.Lock()
	defer k.Unlock()

	/* Skip if not in map (both network and endpoint) */
	if _, netOk := k.networks[req.NetworkID]; !netOk {
		return nil
	}

	if _, epOk := k.networks[req.NetworkID].endpoints[req.EndpointID]; !epOk {
		return nil
	}

	// Should we notify someone - e.g. service load balancer - about this?

	delete(k.networks[req.NetworkID].endpoints, req.EndpointID)

	return nil
}

func (k *FlannelNetworkPlugin) EndpointInfo(req *network.InfoRequest) (*network.InfoResponse, error) {
	log.Printf("Received EndpointOperInfo req:\n%+v\n", req)

	k.Lock()
	defer k.Unlock()

	/* Throw error (both network and endpoint) */
	if _, netOk := k.networks[req.NetworkID]; !netOk {
		return nil, types.ForbiddenErrorf("%s network does not exist", req.NetworkID)
	}

	if _, epOk := k.networks[req.NetworkID].endpoints[req.EndpointID]; !epOk {
		return nil, types.ForbiddenErrorf("%s endpoint does not exist", req.NetworkID)
	}

	endpointInfo := k.networks[req.NetworkID].endpoints[req.EndpointID]
	value := make(map[string]string)

	value["ip_address"] = ""
	value["mac_address"] = endpointInfo.macAddress.String()

	resp := &network.InfoResponse{
		Value: value,
	}

	return resp, nil
}

func (k *FlannelNetworkPlugin) Join(req *network.JoinRequest) (*network.JoinResponse, error) {
	log.Printf("Received Join req:\n%+v\n", req)

	k.Lock()
	defer k.Unlock()

	/* Throw error (both network and endpoint) */
	if _, netOk := k.networks[req.NetworkID]; !netOk {
		return nil, types.ForbiddenErrorf("%s network does not exist", req.NetworkID)
	}

	if _, epOk := k.networks[req.NetworkID].endpoints[req.EndpointID]; !epOk {
		return nil, types.ForbiddenErrorf("%s endpoint does not exist", req.NetworkID)
	}

	// TODO: What do we actually need to do here? Do we actually need to create a veth pair?

	//endpointInfo := k.networks[req.NetworkID].endpoints[req.EndpointID]
	//vethInside, vethOutside, err := createVethPair(endpointInfo.macAddress)
	//if err != nil {
	//	return nil, err
	//}
	//
	//if err := attachInterfaceToBridge(k.networks[req.NetworkID].bridgeName, vethOutside); err != nil {
	//	return nil, err
	//}
	//
	//k.networks[req.NetworkID].endpoints[req.EndpointID].vethInside = vethInside
	//k.networks[req.NetworkID].endpoints[req.EndpointID].vethOutside = vethOutside

	resp := &network.JoinResponse{
		InterfaceName: network.InterfaceName{
			//SrcName:   vethInside,
			SrcName:   "vethInside",
			DstPrefix: "eth",
		},
		DisableGatewayService: true,
	}

	return resp, nil
}

func (k *FlannelNetworkPlugin) Leave(req *network.LeaveRequest) error {
	log.Printf("Received Leave req:\n%+v\n", req)

	k.Lock()
	defer k.Unlock()

	/* Throw error (both network and endpoint) */
	if _, netOk := k.networks[req.NetworkID]; !netOk {
		return types.ForbiddenErrorf("%s network does not exist", req.NetworkID)
	}

	if _, epOk := k.networks[req.NetworkID].endpoints[req.EndpointID]; !epOk {
		return types.ForbiddenErrorf("%s endpoint does not exist", req.NetworkID)
	}

	//endpointInfo := k.networks[req.NetworkID].endpoints[req.EndpointID]

	//if err := deleteVethPair(endpointInfo.vethOutside); err != nil {
	//	return err
	//}

	return nil
}

func (k *FlannelNetworkPlugin) DiscoverNew(req *network.DiscoveryNotification) error {
	log.Printf("Received DiscoverNew req:\n%+v\n", req)

	return nil
}

func (k *FlannelNetworkPlugin) DiscoverDelete(req *network.DiscoveryNotification) error {
	log.Printf("Received DiscoverDelete req:\n%+v\n", req)

	return nil
}

func (k *FlannelNetworkPlugin) ProgramExternalConnectivity(req *network.ProgramExternalConnectivityRequest) error {
	log.Printf("Received ProgramExternalConnectivity req:\n%+v\n", req)

	return nil
}

func (k *FlannelNetworkPlugin) RevokeExternalConnectivity(req *network.RevokeExternalConnectivityRequest) error {
	log.Printf("Received RevokeExternalConnectivity req:\n%+v\n", req)

	return nil
}

func NewFlannelNetworkPlugin(scope string, networks map[string]*flannelNetwork) (*FlannelNetworkPlugin, error) {
	flannelPlugin := &FlannelNetworkPlugin{
		networks: networks,
	}

	return flannelPlugin, nil
}

func main() {
	driver, err := NewFlannelNetworkPlugin("local", map[string]*flannelNetwork{})

	if err != nil {
		log.Fatalf("ERROR: %s init failed!", PLUGIN_NAME)
	}

	requestHandler := network.NewHandler(driver)

	if err := requestHandler.ServeUnix(PLUGIN_NAME, PLUGIN_GUID); err != nil {
		log.Fatalf("ERROR: %s init failed!", PLUGIN_NAME)
	}
}
