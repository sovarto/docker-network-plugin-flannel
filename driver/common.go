package driver

import (
	"bufio"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	dockerAPItypes "github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/events"
	dockerCliAPI "github.com/docker/docker/client"
	"github.com/docker/go-plugins-helpers/sdk"
	"golang.org/x/exp/maps"
	"io"
	"log"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

type FlannelEndpoint struct {
	ipAddress   string
	macAddress  string
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

func NewFlannelNetwork() *FlannelNetwork {
	return &FlannelNetwork{endpoints: make(map[string]*FlannelEndpoint), reservedAddresses: make(map[string]struct{})}
}

type FlannelConfig struct {
	Network *net.IPNet // The subnet of the network across all hosts
	Subnet  *net.IPNet // The subnet for this network on the current host. Inside the network subnet
	Gateway net.IP
	MTU     int
	IPMasq  bool
}

type FlannelDriver struct {
	networks                    map[string]*FlannelNetwork
	networkIdToFlannelNetworkId map[string]string
	defaultFlannelOptions       []string
	etcdClient                  *EtcdClient
	dockerClient                *dockerCliAPI.Client
	sync.Mutex
}

func NewFlannelDriver(etcdClient *EtcdClient, defaultFlannelOptions []string) (*FlannelDriver, error) {

	dockerCli, err := dockerCliAPI.NewClientWithOpts(
		dockerCliAPI.WithHost("unix:///var/run/docker.sock"),
		dockerCliAPI.WithAPIVersionNegotiation(),
	)

	if err != nil {
		log.Println("Failed to create docker client: ", err)
		return nil, err
	}

	driver := &FlannelDriver{
		networks:                    make(map[string]*FlannelNetwork),
		networkIdToFlannelNetworkId: make(map[string]string),
		defaultFlannelOptions:       defaultFlannelOptions,
		etcdClient:                  etcdClient,
		dockerClient:                dockerCli,
	}

	err = driver.restoreNetworks()

	if err != nil {
		log.Println("Failed to load networks: ", err)
		return nil, err
	}

	err = driver.BuildNetworkIdMappings()
	if err != nil {
		log.Println("Failed to build network ID mappings: ", err)
		return nil, err
	}

	driver.ensureProperSetupForAllExpectedNetworks()

	go func() {
		eventsCh, errCh := dockerCli.Events(context.Background(), dockerAPItypes.EventsOptions{})
		for {
			select {
			case err := <-errCh:
				log.Printf("Unable to connect to docker events channel, reconnecting..., err: %+v\n", err)
				time.Sleep(5 * time.Second)
				eventsCh, errCh = dockerCli.Events(context.Background(), dockerAPItypes.EventsOptions{})
			case event := <-eventsCh:
				log.Printf("Received docker event: %+v\n", event)
				if event.Type == events.NetworkEventType && event.Action == "create" {
					network, err := dockerCli.NetworkInspect(context.Background(), event.Actor.ID, dockerAPItypes.NetworkInspectOptions{})
					if err != nil {
						log.Printf("Error inspecting docker network: %+v\n", err)
						break
					}
					id, exists := network.IPAM.Options["id"]
					if !exists {
						log.Printf("Network %s has no 'id' option, it's misconfigured or not for us\n", event.Actor.ID)
						break
					}

					driver.networkIdToFlannelNetworkId[event.Actor.ID] = id
					break
				}
			}
		}
	}()

	return driver, nil
}

func ServeFlannelDriver(etcdEndPoints []string, etcdPrefix string, defaultFlannelOptions []string, availableSubnets []string, defaultHostSubnetSize int) {

	flannelDriver, err := NewFlannelDriver(NewEtcdClient(etcdEndPoints, 5*time.Second, etcdPrefix, availableSubnets, defaultHostSubnetSize), defaultFlannelOptions)
	if err != nil {
		log.Fatalf("ERROR: %s init failed, can't create driver: %v", "flannel-np", err)
	}

	handler := sdk.NewHandler(`{"Implements": ["IpamDriver", "NetworkDriver"]}`)
	initIpamMux(&handler, flannelDriver)
	initNetworkMux(&handler, flannelDriver)

	if err := handler.ServeUnix("flannel-np", 0); err != nil {
		log.Fatalf("ERROR: %s init failed, can't open socket: %v", "flannel-np", err)
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

		flannelNetwork = NewFlannelNetwork()

		err = d.startFlannel(flannelNetworkId, flannelNetwork)
		if err != nil {
			return nil, err
		}

		d.networks[flannelNetworkId] = flannelNetwork

		return flannelNetwork, nil
	} else {
		if flannelNetwork.pid == 0 || !isProcessRunning(flannelNetwork.pid) {
			err := d.startFlannel(flannelNetworkId, flannelNetwork)
			if err != nil {
				return nil, err
			}
		}

		return flannelNetwork, nil
	}
}

func readPipe(pipe io.Reader, doneChan chan struct{}) {
	scanner := bufio.NewScanner(pipe)
	for scanner.Scan() {
		line := scanner.Text()
		fmt.Fprintln(os.Stdout, line) // Write to os.Stdout
		if strings.Contains(line, "bootstrap done") {
			close(doneChan)
			return
		}
	}
	if err := scanner.Err(); err != nil {
		log.Println("Error reading pipe:", err)
	}
}

func (d *FlannelDriver) startFlannel(flannelNetworkId string, network *FlannelNetwork) error {
	subnetFile := fmt.Sprintf("/flannel-env/%s.env", flannelNetworkId)
	etcdPrefix := fmt.Sprintf("%s/%s", d.etcdClient.prefix, flannelNetworkId)

	args := []string{
		fmt.Sprintf("-subnet-file=%s", subnetFile),
		fmt.Sprintf("-etcd-prefix=%s", etcdPrefix),
		fmt.Sprintf("-etcd-endpoints=%s", strings.Join(d.etcdClient.endpoints, ",")),
	}
	args = append(args, d.defaultFlannelOptions...)

	cmd := exec.Command("/flanneld", args...)

	// Capture stdout and stderr
	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		log.Println("Failed to get stdout pipe:", err)
		return err
	}

	stderrPipe, err := cmd.StderrPipe()
	if err != nil {
		log.Println("Failed to get stderr pipe:", err)
		return err
	}

	bootstrapDoneChan := make(chan struct{})

	go readPipe(stdoutPipe, bootstrapDoneChan)
	go readPipe(stderrPipe, bootstrapDoneChan)

	// Start the process
	if err := cmd.Start(); err != nil {
		log.Println("Failed to start flanneld:", err)
		return err
	}

	log.Printf("flanneld started with PID %d for flannel network id %s\n", cmd.Process.Pid, flannelNetworkId)

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
		return fmt.Errorf("flanneld exited prematurely: %v", err)
	case <-bootstrapDoneChan:
		// "bootstrap done" was found
		fmt.Println("flanneld bootstrap completed successfully")
	case <-time.After(1500 * time.Millisecond):
		// Timeout occurred before "bootstrap done"
		log.Println("flanneld failed to bootstrap within 1.5 seconds")
		// Kill the process
		if err := cmd.Process.Kill(); err != nil {
			log.Println("Failed to kill flanneld process:", err)
		}
		return fmt.Errorf("flanneld failed to bootstrap within 1.5 seconds")
	}

	config, err := loadFlannelConfig(subnetFile)
	if err != nil {
		cmd.Process.Kill()
		return err
	}

	network.Mutex.Lock()
	defer network.Mutex.Unlock()

	err = d.etcdClient.EnsureGatewayIsMarkedAsReserved(&config)
	if err != nil {
		return err
	}

	network.pid = cmd.Process.Pid
	network.config = config
	network.reservedAddresses[config.Gateway.String()] = struct{}{}

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

func waitForFileWithContext(ctx context.Context, path string) error {
	const pollInterval = 100 * time.Millisecond

	for {
		select {
		case <-ctx.Done():
			// Context has been canceled or timed out
			return fmt.Errorf("timed out waiting for file %s: %w", path, ctx.Err())
		default:
			// Continue to check for the file
		}

		// Attempt to get file info
		_, err := os.Stat(path)
		if err == nil {
			// File exists
			return nil
		}
		if !os.IsNotExist(err) {
			// An error other than "not exists" occurred
			return fmt.Errorf("error checking file %s: %w", path, err)
		}

		// Wait for the next polling interval or context cancellation
		select {
		case <-ctx.Done():
			return fmt.Errorf("timed out waiting for file %s: %w", path, ctx.Err())
		case <-time.After(pollInterval):
			// Continue looping
		}
	}
}

func loadFlannelConfig(filename string) (FlannelConfig, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err := waitForFileWithContext(ctx, filename)
	if err != nil {
		return FlannelConfig{}, fmt.Errorf("flannel env missing: %w", err)
	}
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
			_, ipNet, err := net.ParseCIDR(value)
			if err != nil {
				return FlannelConfig{}, fmt.Errorf("invalid CIDR format for network: %v", err)
			}
			config.Network = ipNet
		case "SUBNET":
			ip, ipNet, err := net.ParseCIDR(value)
			if err != nil {
				return FlannelConfig{}, fmt.Errorf("invalid CIDR format for subnet: %v", err)
			}
			config.Subnet = ipNet
			config.Gateway = ip
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

func (d *FlannelDriver) restoreNetworks() error {

	files, err := filepath.Glob("/flannel-env/*.env")
	if err != nil {
		fmt.Printf("Error loading networks: %v", err)
		return err
	}

	if len(files) == 0 {
		fmt.Println("No previous network configurations found in /flannel-env")
		return nil
	}

	etcd, err := newEtcdConnection(d.etcdClient.endpoints, d.etcdClient.dialTimeout)
	defer etcd.Close()

	if err != nil {
		log.Println("Failed to connect to etcd:", err)
		return err
	}

	for _, file := range files {
		fmt.Println("Loading network configuration:", file)
		config, err := loadFlannelConfig(file)
		if err != nil {
			log.Printf("Error loading flanneld env file %s, skipping. err: %+v\n", file, err)
			continue
		}

		flannelNetworkId := strings.TrimSuffix(strings.TrimPrefix(file, "/flannel-env/"), ".env")

		_, found, err := d.etcdClient.readExistingNetworkConfig(etcd, d.etcdClient.flannelConfigKey(flannelNetworkId))
		if err != nil {
			log.Printf("Error while reading flanneld network configuration for flannel network with ID %s. Skipping", flannelNetworkId)
			continue
		} else if !found {
			log.Printf("no flanneld network configuration found for flannel network with ID %s. Skipping", flannelNetworkId)
			continue
		}

		reservedAddresses, err := d.etcdClient.LoadReservedAddresses(&config)

		if err != nil {
			log.Printf("Error loading reserved addresses for flanneld env %s. err: %+v\n", file, err)
		} else {
			if len(reservedAddresses) == 0 {
				fmt.Println("No reserved addresses loaded")
			} else {
				smallestIP, biggestIP, count := getInfoAboutIPList(reservedAddresses)
				fmt.Printf("Loaded %v IP addresses for network %s that are currently or have previously been reserved, with %s being the smallest and %s being the biggest", count, flannelNetworkId, smallestIP, biggestIP)
			}
		}

		network := NewFlannelNetwork()
		network.config = config
		network.reservedAddresses = reservedAddresses
		network.bridgeName = getBridgeName(flannelNetworkId)

		err = ensureBridge(network)
		if err != nil {
			log.Printf("Error ensuring flanneld bridge is created, skipping... err: %+v\n", err)
			continue
		}

		err = d.startFlannel(flannelNetworkId, network)
		if err != nil {
			log.Printf("Error starting flanneld for network %s, skipping. err: %+v\n", flannelNetworkId, err)
			continue
		}

		d.networks[flannelNetworkId] = network
	}

	return nil
}

func (d *FlannelDriver) ensureProperSetupForAllExpectedNetworks() {
	for networkId, flannelNetworkId := range d.networkIdToFlannelNetworkId {
		if d.networks[flannelNetworkId] == nil {
			fmt.Printf("Expected internal configuration for flannel network with ID %s / Docker network with ID %s. Trying to recreate it and restore functionality\n", flannelNetworkId, networkId)
			_, err := d.ensureFlannelIsConfiguredAndRunning(flannelNetworkId)
			if err != nil {
				log.Fatalf("Failed to restore functionality for flannel network with ID %s / Docker network with ID %s. Stopping startup of plugin.", flannelNetworkId, networkId)
			}
		}
	}
}

func getInfoAboutIPList(ips map[string]struct{}) (string, string, int) {
	keys := maps.Keys(ips)

	// Sort the keys numerically
	sort.Slice(keys, func(i, j int) bool {
		ip1 := net.ParseIP(keys[i]).To4()
		ip2 := net.ParseIP(keys[j]).To4()
		return binary.BigEndian.Uint32(ip1) < binary.BigEndian.Uint32(ip2)
	})

	return keys[0], keys[len(keys)-1], len(ips)
}

func (d *FlannelDriver) BuildNetworkIdMappings() error {
	networks, err := d.dockerClient.NetworkList(context.Background(), dockerAPItypes.NetworkListOptions{})

	if err != nil {
		return fmt.Errorf("failed to list docker networks: %s", err)
	}
	for _, n := range networks {
		id, exists := n.IPAM.Options["id"]
		if !exists {
			fmt.Printf("Network %s has no 'id' option, it's misconfigured or not for us\n", n.ID)
			continue
		}

		d.networkIdToFlannelNetworkId[n.ID] = id
		fmt.Printf("Network %s has flannel network id: %s\n", n.ID, id)
	}

	return nil
}
