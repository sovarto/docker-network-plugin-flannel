package driver

import (
	"bufio"
	"errors"
	"fmt"
	"github.com/docker/go-plugins-helpers/ipam"
	"github.com/docker/go-plugins-helpers/network"
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

type FlannelEndpoint struct {
	ipAddress   string
	macAddress  net.HardwareAddr
	vethInside  string
	vethOutside string
}

type FlannelNetwork struct {
	bridgeName        string
	config            FlannelConfig
	endpoints         map[string]*FlannelEndpoint
	reservedAddresses map[string]struct{} // This will contain all addresses that have been reserved in the past, even those that have since been freed. This allows us to only re-use IP addresses when no more un-reserved addresses exist
	pid               int
	sync.Mutex
}

type FlannelConfig struct {
	Network string // The subnet of the network across all hosts
	Subnet  string // The subnet for this network on the current host. Inside the network subnet
	MTU     int
	IPMasq  bool
}

type FlannelDriver struct {
	networks                    map[string]*FlannelNetwork
	networkIdToFlannelNetworkId map[string]string
	defaultFlannelOptions       []string
	etcdClient                  *EtcdClient
	sync.Mutex
}

func ServeFlannelDriver(etcdEndPoints []string, etcdPrefix string, defaultFlannelOptions []string, availableSubnets []string, defaultHostSubnetSize int) {

	flannelDriver := &FlannelDriver{
		networks:              make(map[string]*FlannelNetwork),
		defaultFlannelOptions: defaultFlannelOptions,
		etcdClient:            NewEtcdClient(etcdEndPoints, 5*time.Second, etcdPrefix, availableSubnets, defaultHostSubnetSize),
	}

	flannelNetworkPlugin := &FlannelNetworkPlugin{
		driver: flannelDriver,
	}

	flannelIpamPlugin := &FlannelIpamPlugin{
		driver: flannelDriver,
	}

	if err := network.NewHandler(flannelNetworkPlugin).ServeUnix("flannel-np", 0); err != nil {
		log.Fatalf("ERROR: %s init Network failed, can't open socket: %v", "flannel-np", err)
	}
	if err := ipam.NewHandler(flannelIpamPlugin).ServeUnix("flannel-np", 0); err != nil {
		log.Fatalf("ERROR: %s init Ipam failed, can't open socket: %v", "flannel-np", err)
	}
}

func (d *FlannelDriver) ensureFlannelIsConfiguredAndRunning(flannelNetworkId string) (*FlannelNetwork, error) {
	d.Mutex.Lock()
	defer d.Mutex.Unlock()

	flannelNetwork, exists := d.networks[flannelNetworkId]
	if !exists {
		_, err := d.etcdClient.EnsureFlannelConfig(flannelNetworkId)
		if err != nil {
			return nil, err
		}

		flannelNetwork = &FlannelNetwork{
			endpoints: make(map[string]*FlannelEndpoint),
		}

		err = d.ensureFlannelIsRunning(flannelNetworkId, flannelNetwork)
		if err != nil {
			return nil, err
		}

		d.networks[flannelNetworkId] = flannelNetwork

		return flannelNetwork, nil
	} else {
		if flannelNetwork.pid == 0 || !isProcessRunning(flannelNetwork.pid) {
			err := d.ensureFlannelIsRunning(flannelNetworkId, flannelNetwork)
			if err != nil {
				return nil, err
			}
		}

		return flannelNetwork, nil
	}
}

func (d *FlannelDriver) ensureFlannelIsRunning(flannelNetworkId string, network *FlannelNetwork) error {
	subnetFile := fmt.Sprintf("/flannel-env/%s.env", flannelNetworkId)
	etcdPrefix := fmt.Sprintf("%s/%s", d.etcdClient.prefix, flannelNetworkId)

	args := []string{
		fmt.Sprintf("-subnet-file=%s", subnetFile),
		fmt.Sprintf("-etcd-prefix=%s", etcdPrefix),
		fmt.Sprintf("-etcd-endpoints=%s", strings.Join(d.etcdClient.endpoints, ",")),
	}
	args = append(args, d.defaultFlannelOptions...)

	cmd := exec.Command("/flanneld", args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	if err := cmd.Start(); err != nil {
		log.Println("Failed to start flanneld:", err)
		return err
	}

	log.Println("flanneld started with PID", cmd.Process.Pid)

	config, err := loadFlannelConfig(subnetFile)
	if err != nil {
		cmd.Process.Kill()
		return err
	}

	network.Mutex.Lock()
	defer network.Mutex.Unlock()

	network.pid = cmd.Process.Pid
	network.config = config

	return nil
}

func isProcessRunning(pid int) bool {
	if pid <= 0 {
		return false
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	err = proc.Signal(syscall.Signal(0))
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

func loadFlannelConfig(filename string) (FlannelConfig, error) {
	file, err := os.Open(filename)
	if err != nil {
		return FlannelConfig{}, fmt.Errorf("failed to open file: %w", err)
	}
	defer file.Close()

	var config FlannelConfig

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
			config.Network = value
		case "SUBNET":
			config.Subnet = value
		case "MTU":
			mtu, err := strconv.Atoi(value)
			if err != nil {
				return FlannelConfig{}, fmt.Errorf("invalid MTU value '%s': %w", value, err)
			}
			config.MTU = mtu
		case "IPMASQ":
			ipmasq, err := strconv.ParseBool(value)
			if err != nil {
				return FlannelConfig{}, fmt.Errorf("invalid IPMASQ value '%s': %w", value, err)
			}
			config.IPMasq = ipmasq
		default:
			fmt.Printf("Unknown configuration key: %s\n", key)
		}
	}

	if err := scanner.Err(); err != nil {
		return FlannelConfig{}, fmt.Errorf("error reading file: %w", err)
	}

	return config, nil
}
