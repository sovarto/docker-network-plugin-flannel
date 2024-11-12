package main

import (
	"context"
	"fmt"
	"github.com/docker/docker/libnetwork/types"
	"github.com/docker/go-plugins-helpers/network"
	"log"
	"net"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"

	clientv3 "go.etcd.io/etcd/client/v3"
)

var (
	PLUGIN_NAME = "flannel-np"
	PLUGIN_GUID = 0
)

type flannelEndpoint struct {
	macAddress  net.HardwareAddr
	vethInside  string
	vethOutside string
}

type flannelNetwork struct {
	bridgeName string
	subnet     string
	endpoints  map[string]*flannelEndpoint
	pid        int
}

type FlannelNetworkPlugin struct {
	networks              map[string]*flannelNetwork
	etcdPrefix            string
	etcdEndPoints         []string
	defaultFlannelOptions []string
	availableSubnets      []string // with size NETWORK_SUBNET_SIZE
	defaultHostSubnetSize int
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

	if err := detectIpTables(); err != nil {
		return err
	}

	if _, ok := k.networks[req.NetworkID]; ok {
		return types.ForbiddenErrorf("network %s exists", req.NetworkID)
	}

	cli, err := clientv3.New(clientv3.Config{
		Endpoints:   k.etcdEndPoints,
		DialTimeout: 5 * time.Second,
	})
	if err != nil {
		fmt.Println("Failed to connect to etcd:", err)
		return err
	}
	defer cli.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var hostSubnetLength int
	hostSubnetLengthString, ok := req.Options["com.sovarto.flannel.host.subnet.size"]
	if !ok || hostSubnetLengthString == nil {
		hostSubnetLength = k.defaultHostSubnetSize
	} else {
		hostSubnetLength, err = strconv.Atoi(hostSubnetLengthString.(string))
		if err != nil {
			hostSubnetLength = k.defaultHostSubnetSize
		}
	}

	subnet, err := allocateSubnetAndCreateFlannelConfig(ctx, cli, k.etcdPrefix, req.NetworkID, k.availableSubnets, hostSubnetLength)

	if err != nil {
		return err
	}

	bridgeName, err := createBridge(req.NetworkID)
	if err != nil {
		return err
	}

	// Start flannel process
	subnetFile := fmt.Sprintf("/flannel-env/%s.env", req.NetworkID)
	etcdPrefix := fmt.Sprintf("/%s/%s", k.etcdPrefix, req.NetworkID)

	args := []string{
		fmt.Sprintf("-subnet-file=%s", subnetFile),
		fmt.Sprintf("-etcd-prefix=%s", etcdPrefix),
	}
	args = append(args, k.defaultFlannelOptions...)

	cmd := exec.Command("flanneld", args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	if err := cmd.Start(); err != nil {
		fmt.Println("Failed to start flanneld:", err)
		return err
	}

	fmt.Println("flanneld started with PID", cmd.Process.Pid)

	flannelNetwork := &flannelNetwork{
		bridgeName: bridgeName,
		endpoints:  make(map[string]*flannelEndpoint),
		pid:        cmd.Process.Pid,
		subnet:     subnet,
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

	if err := detectIpTables(); err != nil {
		return err
	}

	err := deleteBridge(req.NetworkID)
	if err != nil {
		return err
	}

	// TODO:
	// Stop flannel process
	// Delete /<options.prefix>/<req.NetworkID> and all children from etcd
	// Mark subnet as free in etcd

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

	if req.Interface == nil {
		// TODO: Verify that this guarantees uniqueness. If not, use something else

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

	endpointInfo := k.networks[req.NetworkID].endpoints[req.EndpointID]
	vethInside, vethOutside, err := createVethPair(endpointInfo.macAddress)
	if err != nil {
		return nil, err
	}

	if err := attachInterfaceToBridge(k.networks[req.NetworkID].bridgeName, vethOutside); err != nil {
		return nil, err
	}

	k.networks[req.NetworkID].endpoints[req.EndpointID].vethInside = vethInside
	k.networks[req.NetworkID].endpoints[req.EndpointID].vethOutside = vethOutside

	resp := &network.JoinResponse{
		InterfaceName: network.InterfaceName{
			SrcName:   vethInside,
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

	endpointInfo := k.networks[req.NetworkID].endpoints[req.EndpointID]

	if err := deleteVethPair(endpointInfo.vethOutside); err != nil {
		return err
	}

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

func NewFlannelNetworkPlugin(etcdEndPoints []string, etcdPrefix string, defaultFlannelOptions []string, availableSubnets []string, defaultHostSubnetSize int, networks map[string]*flannelNetwork) (*FlannelNetworkPlugin, error) {
	flannelPlugin := &FlannelNetworkPlugin{
		networks:              networks,
		etcdEndPoints:         etcdEndPoints,
		etcdPrefix:            etcdPrefix,
		defaultFlannelOptions: defaultFlannelOptions,
		availableSubnets:      availableSubnets,
		defaultHostSubnetSize: defaultHostSubnetSize,
	}

	return flannelPlugin, nil
}

func main() {
	etcdEndPoints := strings.Split(os.Getenv("ETCD_ENDPOINTS"), ",")
	etcdPrefix := os.Getenv("ETCD_PREFIX")
	defaultFlannelOptions := strings.Split(os.Getenv("DEFAULT_FLANNEL_OPTIONS"), ",")
	availableSubnets := strings.Split(os.Getenv("AVAILABLE_SUBNETS"), ",")
	networkSubnetSize := getEnvAsInt("NETWORK_SUBNET_SIZE", 20)
	defaultHostSubnetSize := getEnvAsInt("DEFAULT_HOST_SUBNET_SIZE", 25)
	allSubnets, err := generateAllSubnets(availableSubnets, networkSubnetSize)

	if err != nil {
		log.Fatalf("ERROR: %s init failed, invalid subnets configuration: %v", PLUGIN_NAME, err)
	}

	driver, err := NewFlannelNetworkPlugin(etcdEndPoints, etcdPrefix, defaultFlannelOptions,
		allSubnets,
		defaultHostSubnetSize,
		map[string]*flannelNetwork{})

	if err != nil {
		log.Fatalf("ERROR: %s init failed: %v", PLUGIN_NAME, err)
	}

	requestHandler := network.NewHandler(driver)

	if err := requestHandler.ServeUnix(PLUGIN_NAME, PLUGIN_GUID); err != nil {
		log.Fatalf("ERROR: %s init failed, can't open socket: %v", PLUGIN_NAME, err)
	}
}
