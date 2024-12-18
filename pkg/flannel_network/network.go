package flannel_network

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"github.com/pkg/errors"
	"github.com/samber/lo"
	"github.com/sovarto/FlannelNetworkPlugin/pkg/bridge"
	"github.com/sovarto/FlannelNetworkPlugin/pkg/common"
	"github.com/sovarto/FlannelNetworkPlugin/pkg/etcd"
	"github.com/sovarto/FlannelNetworkPlugin/pkg/ipam"
	"github.com/sovarto/FlannelNetworkPlugin/pkg/networking"
	"github.com/vishvananda/netlink"
	clientv3 "go.etcd.io/etcd/client/v3"
	"golang.org/x/exp/maps"
	"log"
	"net"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

type Network interface {
	Ensure() error
	GetInfo() common.FlannelNetworkInfo
	Delete() error
	GetPool() ipam.AddressPool
	AddEndpoint(id string, ip net.IP, mac string) (Endpoint, error)
	GetEndpoint(id string) Endpoint
	DeleteEndpoint(id string) error
}

type network struct {
	flannelID             string
	flannelDaemonProcess  *os.Process
	etcdClient            etcd.Client
	networkSubnet         net.IPNet
	hostSubnet            net.IPNet
	hostSubnetSize        int
	localGateway          net.IP
	mtu                   int
	defaultFlannelOptions []string
	pool                  ipam.AddressPool
	bridge                bridge.BridgeInterface
	endpoints             map[string]Endpoint // endpoint ID -> endpoint
	endpointsEtcdClient   etcd.Client
	vni                   int
	sync.Mutex
}

func NewNetwork(etcdClient etcd.Client, flannelID string, networkSubnet net.IPNet, hostSubnetSize int, defaultFlannelOptions []string, vni int) Network {
	return &network{
		flannelID:             flannelID,
		etcdClient:            etcdClient,
		networkSubnet:         networkSubnet,
		defaultFlannelOptions: defaultFlannelOptions,
		hostSubnetSize:        hostSubnetSize,
		endpoints:             make(map[string]Endpoint),
		endpointsEtcdClient:   etcdClient.CreateSubClient(flannelID, "endpoints"),
		vni:                   vni,
	}
}

func (n *network) Init(existingLocalEndpoints []string) error {
	err := n.Ensure()
	if err != nil {
		return err
	}

	endpoints, err := loadEndpointsFromEtcd(n.endpointsEtcdClient, n.bridge)
	if err != nil {
		return err
	}

	for _, endpoint := range endpoints {
		endpointInfo := endpoint.GetInfo()
		if lo.Some(existingLocalEndpoints, []string{endpointInfo.ID}) {
			n.endpoints[endpointInfo.ID] = endpoint
		} else {
			if err := endpoint.Delete(); err != nil {
				log.Printf("Error deleting endpoint %s: %v", endpointInfo.ID, err)
			}
		}
	}

	return nil

}

func (n *network) GetInfo() common.FlannelNetworkInfo {
	return common.FlannelNetworkInfo{
		FlannelID:    n.flannelID,
		MTU:          n.mtu,
		Network:      &n.networkSubnet,
		HostSubnet:   &n.hostSubnet,
		LocalGateway: n.localGateway,
	}
}

func (n *network) GetPool() ipam.AddressPool {
	return n.pool
}

func (n *network) Delete() error {
	n.Lock()
	defer n.Unlock()

	if err := n.endFlannelDaemonProcess(); err != nil {
		return err
	}

	n.cleanupFlannelEnvFile()

	if err := n.cleanupEtcdData(); err != nil {
		return err
	}

	if err := n.cleanupInterfaces(); err != nil {
		return err
	}

	if err := n.cleanupEndpoints(); err != nil {
		return err
	}

	n.endpoints = map[string]Endpoint{}
	n.localGateway = nil
	n.hostSubnet = net.IPNet{}
	n.mtu = 0

	return nil
}

func (n *network) cleanupEndpoints() error {
	for endpointID, endpoint := range n.endpoints {
		if err := endpoint.Delete(); err != nil {
			return errors.WithMessagef(err, "error deleting endpoint %s for network %s", endpointID, n.flannelID)
		}
	}
	return nil
}

func (n *network) cleanupFlannelEnvFile() {
	if err := deleteFileIfExists(n.getFlannelEnvFilename()); err != nil {
		// Don't fail if we can't delete the flannel env file
		log.Println(err)
	}
}

func (n *network) cleanupEtcdData() error {
	_, err := etcd.WithConnection(n.etcdClient, func(connection *etcd.Connection) (struct{}, error) {
		_, err := connection.Client.Delete(connection.Ctx, n.flannelConfigSubnetKey(n.hostSubnet))
		if err != nil {
			return struct{}{}, errors.WithMessagef(err, "error deleting flannel host subnet config for network %s", n.flannelID)
		}

		networkConfigKey := n.flannelConfigKey()

		result, err := n.readNetworkConfig()
		if err != nil {
			return struct{}{}, errors.WithMessagef(err, "error reading existing flannel network config for network %s", n.flannelID)
		}
		if result.found {
			if result.config.Network != n.networkSubnet.String() {
				return struct{}{}, fmt.Errorf("the flannel config for network %s has unexpected network %s instead of the expected %s", n.flannelID, result.config.Network, n.networkSubnet.String())
			}
		}

		resp, err := connection.Client.Txn(connection.Ctx).
			If(clientv3.Compare(clientv3.ModRevision(networkConfigKey), "=", result.revision)).
			Then(clientv3.OpDelete(networkConfigKey)).
			Commit()

		if err != nil {
			return struct{}{}, errors.WithMessagef(err, "error deleting flannel network config for network %s", n.flannelID)
		}

		if !resp.Succeeded {
			resp, err := connection.Client.Get(connection.Ctx, networkConfigKey)
			if err != nil {
				return struct{}{}, errors.WithMessagef(err, "error deleting flannel network config for network %s, and error during check if it has since been deleted", n.flannelID)
			}
			if resp.Kvs != nil && len(resp.Kvs) > 0 {
				return struct{}{}, fmt.Errorf("error deleting flannel network config for network %s. Got mod revision %d, expected %d", n.flannelID, resp.Kvs[0].ModRevision, result.revision)
			}
		}

		return struct{}{}, nil
	})

	return err
}

func (n *network) cleanupInterfaces() error {
	flannelLinkName := fmt.Sprintf("flannel.%d", n.vni)
	flannelLink, err := netlink.LinkByName(flannelLinkName)
	if err != nil {
		return errors.WithMessagef(err, "Error getting flannel interface %s by name", flannelLinkName)
	}

	flannelNetworkInterfaceIP := n.hostSubnet.IP.String()
	if err := networking.StopListeningOnAddress(flannelLink, flannelNetworkInterfaceIP); err != nil {
		return errors.WithMessagef(err, "error removing IP %s from flannel network interface %s", flannelNetworkInterfaceIP, flannelLinkName)
	}

	addresses, err := netlink.AddrList(flannelLink, netlink.FAMILY_V4)
	if err != nil {
		return errors.WithMessagef(err, "Error getting remaining addresses for flannel interface %s", flannelLinkName)
	}

	if len(addresses) == 0 {
		if err := netlink.LinkDel(flannelLink); err != nil {
			return errors.WithMessagef(err, "Error deleting flannel interface %s", flannelLinkName)
		}
	}

	if err := n.bridge.Delete(); err != nil {
		return errors.WithMessagef(err, "error deleting bridge interface for network %s", n.flannelID)
	}

	if err := n.pool.ReleaseAllIPs(); err != nil {
		return errors.WithMessagef(err, "error releasing all IPs for network %s", n.flannelID)
	}
	return nil
}

func (n *network) endFlannelDaemonProcess() error {
	if n.flannelDaemonProcess != nil {
		err := n.flannelDaemonProcess.Signal(syscall.SIGTERM)
		if err != nil {
			return errors.WithMessagef(err, "error killing flanneld process of network %s", n.flannelID)
		}
		//_, err = n.flannelDaemonProcess.Wait()
		//if err != nil {
		//	return errors.WithMessagef(err, "error waiting for exit for killed flanneld process of network %s", n.flannelID)
		//}

		n.flannelDaemonProcess = nil
	}
	return nil
}

func (n *network) Ensure() error {
	n.Lock()
	defer n.Unlock()

	_, err := n.ensureFlannelConfig()
	if err != nil {
		return err
	}

	if !n.isFlannelDaemonProcessRunning() {
		err = n.startFlannel()
		return err
	}

	return nil
}

type Config struct {
	Network   string        `json:"Network"`
	SubnetLen int           `json:"SubnetLen"`
	Backend   BackendConfig `json:"Backend"`
}

type BackendConfig struct {
	Type string `json:"Type"`
	VNI  int    `json:"VNI"`
}

type SubnetConfig struct {
	PublicIP    string      `json:"PublicIP"`
	BackendType string      `json:"BackendType"`
	BackendData BackendData `json:"BackendData"`
}

type BackendData struct {
	VNI     int    `json:"VNI"`
	VtepMAC string `json:"VtepMAC"`
}

func (n *network) flannelConfigPrefixKey() string {
	return n.etcdClient.GetKey(n.flannelID)
}

func (n *network) flannelConfigKey() string {
	return fmt.Sprintf("%s/config", n.flannelConfigPrefixKey())
}

func (n *network) flannelConfigSubnetKey(subnet net.IPNet) string {
	return fmt.Sprintf("%s/subnets/%s", n.flannelConfigPrefixKey(), common.SubnetToKey(subnet.String()))
}

func (n *network) flannelLockKey() string {
	return fmt.Sprintf("%s/lock", n.flannelConfigPrefixKey())
}

func (n *network) ensureFlannelConfig() (struct{}, error) {
	return etcd.WithConnection(n.etcdClient, func(connection *etcd.Connection) (struct{}, error) {
		networkConfigKey := n.flannelConfigKey()

		result, err := n.readNetworkConfig()
		if err != nil {
			return struct{}{}, errors.WithMessagef(err, "error reading existing flannel network config for network %s", n.flannelID)
		}
		if result.found {
			if result.config.Network == n.networkSubnet.String() {
				return struct{}{}, nil
			}
			return struct{}{}, fmt.Errorf("there already is a flannel config for network %s but it is for network %s instead of the expected %s", n.flannelID, result.config.Network, n.networkSubnet.String())
		}

		configData := Config{
			Network:   n.networkSubnet.String(),
			SubnetLen: n.hostSubnetSize,
			Backend: BackendConfig{
				Type: "vxlan",
				VNI:  n.vni,
			},
		}

		// Serialize the configuration to a JSON string
		configBytes, err := json.Marshal(configData)
		if err != nil {
			log.Println("Failed to serialize configuration:", err)
			return struct{}{}, err
		}

		configString := string(configBytes)
		fmt.Printf("Flannel config: %s\n", configString)
		txn := connection.Client.Txn(connection.Ctx).
			If(clientv3.Compare(clientv3.CreateRevision(networkConfigKey), "=", 0)).
			Then(clientv3.OpPut(networkConfigKey, configString))

		resp, err := txn.Commit()
		if err != nil {
			log.Println("Transaction failed:", err)
			return struct{}{}, err
		}

		if !resp.Succeeded {
			fmt.Printf("flannel network config for network %s was created by another node. Trying to reuse\n", n.flannelID)
			result, err := n.readNetworkConfig()
			if err != nil {
				return struct{}{}, errors.WithMessagef(err, "error reading existing flannel network config for network %s", n.flannelID)
			}
			if result.found {
				if result.config.Network == n.networkSubnet.String() {
					return struct{}{}, nil
				}
				return struct{}{}, fmt.Errorf("there already is a flannel config for network %s but it is for network %s instead of the expected %s", n.flannelID, result.config.Network, n.networkSubnet.String())
			}
		}

		return struct{}{}, nil
	})
}

type ReadNetworkConfigResult struct {
	config   Config
	revision int64
	found    bool
}

func (n *network) readNetworkConfig() (ReadNetworkConfigResult, error) {
	return etcd.WithConnection(n.etcdClient, func(connection *etcd.Connection) (ReadNetworkConfigResult, error) {
		networkConfigKey := n.flannelConfigKey()

		resp, err := connection.Client.Get(connection.Ctx, networkConfigKey)
		if err != nil {
			return ReadNetworkConfigResult{found: false}, errors.WithMessagef(err, "error reading network config for network %s at %s", n.flannelID, networkConfigKey)
		}
		if len(resp.Kvs) > 0 {
			var configData Config
			err := json.Unmarshal(resp.Kvs[0].Value, &configData)
			if err != nil {
				return ReadNetworkConfigResult{found: true, revision: resp.Header.Revision}, errors.WithMessage(err, "error deserializing configuration")
			}

			return ReadNetworkConfigResult{config: configData, found: true, revision: resp.Kvs[0].ModRevision}, nil
		}

		return ReadNetworkConfigResult{found: false}, nil
	})
}

func (n *network) startFlannel() error {
	subnetFile := n.getFlannelEnvFilename()
	etcdPrefix := n.flannelConfigPrefixKey()

	args := []string{
		fmt.Sprintf("-subnet-file=%s", subnetFile),
		fmt.Sprintf("-etcd-prefix=%s", etcdPrefix),
		fmt.Sprintf("-etcd-endpoints=%s", strings.Join(n.etcdClient.GetEndpoints(), ",")),
	}
	args = append(args, n.defaultFlannelOptions...)

	cmd, err := etcd.WithConnection(n.etcdClient, func(connection *etcd.Connection) (*exec.Cmd, error) {
		lockKey := n.flannelLockKey()
		fmt.Printf("trying to acquire lock for flannel network %s at %s\n", n.flannelID, lockKey)
		// TODO: Is this hardcoded value a bad idea?
		mutex, err := connection.LockNewMutex(lockKey, 15*time.Second)
		if err != nil {
			return nil, errors.WithMessagef(err, "error acquiring lock for flannel network %s at %s", n.flannelID, lockKey)
		}
		defer mutex.UnlockAndCloseSession()

		cmd := exec.Command("/flanneld", args...)

		// Capture stdout and stderr
		stdoutPipe, err := cmd.StdoutPipe()
		if err != nil {
			log.Println("Failed to get stdout pipe:", err)
			return nil, err
		}

		stderrPipe, err := cmd.StderrPipe()
		if err != nil {
			log.Println("Failed to get stderr pipe:", err)
			return nil, err
		}

		bootstrapDoneChan := make(chan struct{})

		go readPipe(stdoutPipe, bootstrapDoneChan)
		go readPipe(stderrPipe, bootstrapDoneChan)

		// Start the process
		if err := cmd.Start(); err != nil {
			log.Println("Failed to start flanneld:", err)
			return nil, err
		}

		fmt.Printf("flanneld started with PID %d for flannel network id %s\n", cmd.Process.Pid, n.flannelID)

		exitChan := make(chan error, 1)

		// Goroutine to wait for the process to exit
		go func() {
			err := cmd.Wait()
			exitChan <- err
		}()

		// Wait for "bootstrap done", process exit, or timeout
		select {
		case err := <-exitChan:
			// Process exited before "bootstrap done" or timeout
			log.Printf("flanneld process exited prematurely: %v", err)
			return nil, errors.WithMessagef(err, "flanneld exited prematurely for network %s", n.flannelID)
		case <-bootstrapDoneChan:
			// "bootstrap done" was found
			fmt.Println("flanneld bootstrap completed successfully")
		case <-time.After(1500 * time.Millisecond):
			// Timeout occurred before "bootstrap done"
			log.Printf("flanneld failed to bootstrap within 1.5 seconds for network %s\n", n.flannelID)
			if err := n.endFlannelDaemonProcess(); err != nil {
				log.Println("Failed to kill flanneld process:", err)
			}
			return nil, fmt.Errorf("flanneld failed to bootstrap within 1.5 seconds for network %s", n.flannelID)
		}

		return cmd, nil
	})

	if err != nil {
		return err
	}

	err = n.loadFlannelConfig(subnetFile)

	n.flannelDaemonProcess = cmd.Process
	if err != nil {
		if err := n.endFlannelDaemonProcess(); err != nil {
			log.Println("Failed to kill flanneld process:", err)
		}
		return err
	}

	return nil
}

func (n *network) getFlannelEnvFilename() string {
	return fmt.Sprintf("/flannel-env/%s.env", n.flannelID)
}

func (n *network) loadFlannelConfig(filename string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err := waitForFileWithContext(ctx, filename)
	if err != nil {
		return errors.WithMessagef(err, "flannel env %s missing", filename)
	}
	file, err := os.Open(filename)
	if err != nil {
		return errors.WithMessagef(err, "failed to open file: %s", filename)
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())

		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}

		parts := strings.SplitN(line, "=", 2)
		if len(parts) != 2 {
			fmt.Printf("Skipping invalid line: %s\n", line)
			continue
		}

		key := parts[0]
		value := parts[1]

		if !strings.HasPrefix(key, "FLANNEL_") {
			fmt.Printf("Skipping unrecognized key: %s\n", key)
			continue
		}

		key = strings.TrimPrefix(key, "FLANNEL_")

		switch key {
		case "NETWORK":
			_, ipNet, err := net.ParseCIDR(value)
			if err != nil {
				return errors.WithMessagef(err, "invalid CIDR format for network: %s", value)
			}
			n.networkSubnet = *ipNet
		case "SUBNET":
			ip, ipNet, err := net.ParseCIDR(value)
			if err != nil {
				return errors.WithMessagef(err, "invalid CIDR format for subnet: %s", value)
			}
			pool, err := ipam.NewEtcdBasedAddressPool(n.flannelID,
				*ipNet, n.etcdClient.CreateSubClient(n.flannelID, "host-subnets", common.SubnetToKey(ipNet.String())))
			if err != nil {
				return errors.WithMessagef(err, "can't create address pool for network %s and subnet %s", n.flannelID, ipNet.String())
			}
			n.hostSubnet = *ipNet
			n.localGateway = ip
			n.pool = pool

		case "MTU":
			mtu, err := strconv.Atoi(value)
			if err != nil {
				return errors.WithMessagef(err, "invalid MTU value '%s'", value)
			}
			n.mtu = mtu
		case "IPMASQ":
			// Ignore
			break
		default:
			fmt.Printf("Unknown configuration key hwile loading flannel env %s: %s\n", filename, key)
		}
	}

	if err := scanner.Err(); err != nil {
		return errors.WithMessagef(err, "error reading file: %s", filename)
	}

	b := bridge.NewBridgeInterface(n.GetInfo())
	if err := b.Ensure(); err != nil {
		return errors.WithMessagef(err, "error creating bridge interface")
	}

	n.bridge = b

	return nil
}

func (n *network) AddEndpoint(id string, ip net.IP, mac string) (Endpoint, error) {
	endpoint, err := NewEndpoint(n.endpointsEtcdClient, id, ip, mac, n.bridge)
	if err != nil {
		return nil, errors.WithMessagef(err, "error creating endpoint for network %s", n.flannelID)
	}
	n.endpoints[id] = endpoint

	return endpoint, nil
}

func (n *network) DeleteEndpoint(id string) error {
	endpoint, exists := n.endpoints[id]
	if !exists {
		return errors.Errorf("endpoint %s does not exist", id)
	}

	err := endpoint.Delete()
	if err != nil {
		return errors.WithMessagef(err, "error deleting endpoint for network %s", n.flannelID)
	}

	delete(n.endpoints, id)
	return nil
}

func (n *network) GetEndpoint(id string) Endpoint {
	return n.endpoints[id]
}

func deleteFileIfExists(filename string) error {
	if _, err := os.Stat(filename); err == nil {
		if err := os.Remove(filename); err != nil {
			return fmt.Errorf("error deleting file: %w", err)
		}
	} else if os.IsNotExist(err) {
		fmt.Printf("File %s does not exist.\n", filename)
	} else {
		// An error occurred while checking if the file exists
		return fmt.Errorf("error checking file: %w", err)
	}
	return nil
}

func (n *network) isFlannelDaemonProcessRunning() bool {
	if n.flannelDaemonProcess == nil {
		return false
	}
	err := n.flannelDaemonProcess.Signal(syscall.Signal(0))
	if err == nil {
		return true
	}
	if err.Error() == "os: process already finished" {
		return false
	}
	var errno syscall.Errno
	ok := errors.As(err, &errno)
	if !ok {
		return false
	}
	switch {
	case errors.Is(errno, syscall.ESRCH):
		return false
	case errors.Is(errno, syscall.EPERM):
		return true
	}
	return false
}

func CleanupStaleNetworks(etcdClient etcd.Client, existingNetworks []common.NetworkInfo) error {
	localIPs, err := getLocalIPsMap()
	if err != nil {
		return err
	}

	knownNetworksVNIs := map[int]string{}
	_, err = etcd.WithConnection(etcdClient, func(connection *etcd.Connection) (struct{}, error) {
		resp, err := connection.Client.Get(connection.Ctx, etcdClient.GetKey(), clientv3.WithPrefix())
		if err != nil {
			return struct{}{}, errors.WithMessagef(err, "error retrieving existing networks data from etcd")
		}

		networksToDelete := map[string]struct{}{}
		for _, kv := range resp.Kvs {
			key := strings.TrimLeft(strings.TrimLeft(string(kv.Key), etcdClient.GetKey()), "/")
			keyParts := strings.Split(key, "/")
			flannelNetworkID := keyParts[0]
			if !lo.SomeBy(existingNetworks, func(item common.NetworkInfo) bool {
				return item.FlannelID == flannelNetworkID
			}) {
				networksToDelete[flannelNetworkID] = struct{}{}
				continue
			}
			if len(keyParts) == 2 && keyParts[1] == "config" {
				var configData Config
				err := json.Unmarshal(resp.Kvs[0].Value, &configData)
				if err != nil {
					log.Printf("error deserializing configuration of network %s: %+v", flannelNetworkID, err)
					continue
				}
				knownNetworksVNIs[configData.Backend.VNI] = flannelNetworkID
			} else if len(keyParts) == 3 && keyParts[1] == "subnets" {
				var configData SubnetConfig
				if err := json.Unmarshal(resp.Kvs[0].Value, &configData); err != nil {
					log.Printf("error deserializing subnet %s configuration of network %s: %+v", keyParts[2], flannelNetworkID, err)
					continue
				}

				_, isLocalIP := localIPs[configData.PublicIP]
				if !isLocalIP {
					continue
				}

				knownNetworksVNIs[configData.BackendData.VNI] = flannelNetworkID
			}
		}

		for flannelNetworkID, _ := range networksToDelete {
			fmt.Printf("Deleting data of stale network %s\n", flannelNetworkID)
			_, err := connection.Client.Delete(connection.Ctx, etcdClient.GetKey(flannelNetworkID), clientv3.WithPrefix())
			if err != nil {
				log.Printf("error deleting flannel network %s: %v", flannelNetworkID, err)
			}
		}

		return struct{}{}, nil
	})

	if err != nil {
		return err
	}

	links, err := netlink.LinkList()
	if err != nil {
		return errors.WithMessage(err, "error listing network interfaces when cleaning up stale networks")
	}

	validFlannelInterfaces := lo.Map(maps.Keys(knownNetworksVNIs), func(item int, index int) string {
		return fmt.Sprintf("flannel.%d", item)
	})

	for _, link := range links {
		if strings.Index(link.Attrs().Name, "flannel") == 0 && !lo.Some(validFlannelInterfaces, []string{link.Attrs().Name}) {
			fmt.Printf("Deleting stale flannel network interface %s\n", link.Attrs().Name)
			err := netlink.LinkDel(link)
			if err != nil {
				log.Printf("error deleting flannel network interface %s: %+v", link.Attrs().Name, err)
			}
		}
	}

	// TODO: Clean up ip tables rules created by flannel

	if err := bridge.CleanUpStaleInterfaces(maps.Values(knownNetworksVNIs)); err != nil {
		return errors.WithMessage(err, "error cleaning up stale network interfaces")
	}

	return nil
}

func getLocalIPsMap() (map[string]struct{}, error) {
	localIPs := make(map[string]struct{})

	// Get all network interfaces
	interfaces, err := net.Interfaces()
	if err != nil {
		return nil, errors.WithMessage(err, "failed to get network interfaces")
	}

	// Iterate over all interfaces and gather IPs
	for _, iface := range interfaces {
		addrs, err := iface.Addrs()
		if err != nil {
			return nil, errors.WithMessagef(err, "failed to get addresses for interface %s", iface.Name)
		}

		for _, addr := range addrs {
			var ip net.IP
			switch v := addr.(type) {
			case *net.IPNet:
				ip = v.IP
			case *net.IPAddr:
				ip = v.IP
			}

			// Skip invalid or zero IPs
			if ip == nil || ip.IsUnspecified() {
				continue
			}

			// Add the IP string to the map
			localIPs[ip.String()] = struct{}{}
		}
	}

	return localIPs, nil
}
