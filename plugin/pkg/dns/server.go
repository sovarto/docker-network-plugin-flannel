package dns

import (
	"context"
	"fmt"
	"github.com/coreos/go-iptables/iptables"
	"github.com/davecgh/go-spew/spew"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/client"
	"github.com/miekg/dns"
	"github.com/pkg/errors"
	"github.com/samber/lo"
	"github.com/sovarto/FlannelNetworkPlugin/pkg/common"
	"github.com/sovarto/FlannelNetworkPlugin/pkg/networking"
	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netns"
	"log"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
)

const (
	SandboxesPath = "/hostfs/var/run/flannel-np/sandboxes"
	ReadyPath     = "/hostfs/var/run/flannel-np/ready"
)

type Nameserver interface {
	Activate() <-chan error
	DeactivateAndCleanup() error
	AddValidNetworkID(validNetworkID string)
	RemoveValidNetworkID(validNetworkID string)
}

type nameserver struct {
	networkNamespace string
	resolver         Resolver
	portTCP          int
	portUDP          int
	listenIP         string
	tcpServer        *dns.Server
	udpServer        *dns.Server
	validNetworkIDs  *common.ConcurrentMap[string, struct{}]
	isHookAvailable  bool
	readyFile        string
	initManager      *common.InitManager
	sync.Mutex
}

// TODO: Add support for dns options on containers, services, docker
// TODO: Add support for host resolver ???
// TODO: Check domainname config of containers whether it's relevant

// NewNameserver
// - networkNamespace: This is expected to be a docker sandbox key of the form /var/run/docker/netns/<key>
func NewNameserver(networkNamespace string, resolver Resolver, isHookAvailable bool) (Nameserver, error) {
	result := &nameserver{
		networkNamespace: adjustNamespacePath(networkNamespace),
		resolver:         resolver,
		listenIP:         "127.0.0.33",
		validNetworkIDs:  common.NewConcurrentMap[string, struct{}](),
		isHookAvailable:  isHookAvailable,
	}

	if isHookAvailable {
		parts := strings.Split(networkNamespace, "/")
		namespaceKey := parts[len(parts)-1]
		sandboxFile := filepath.Join(SandboxesPath, namespaceKey)
		if err := os.WriteFile(sandboxFile, []byte{}, 0755); err != nil {
			return nil, errors.WithMessagef(err, "Error creating sandbox file %s", sandboxFile)
		}
		result.readyFile = filepath.Join(ReadyPath, namespaceKey)
	}

	result.initManager = common.NewInitManager(result.startDnsServersInNamespace)

	return result, nil
}

func (n *nameserver) Activate() <-chan error {
	n.Lock()
	defer n.Unlock()

	errCh := make(chan error, 1)

	fmt.Printf("Starting nameserver in namespace %s\n", n.networkNamespace)
	go func() {
		defer close(errCh)
		n.initManager.Init()
		if err := n.initManager.Wait(); err != nil {
			errCh <- errors.WithMessagef(err, "Error listening in namespace %s", n.networkNamespace)
		}
		if n.isHookAvailable {
			if err := os.WriteFile(n.readyFile, []byte{}, 0755); err != nil {
				log.Printf("Error creating ready file for namespace %s: %+v", n.networkNamespace, err)
				return
			}
		}
	}()

	return errCh
}

func (n *nameserver) DeactivateAndCleanup() error {
	n.Lock()
	defer n.Unlock()

	fmt.Printf("Stopping nameserver in namespace %s\n", n.networkNamespace)
	if !n.initManager.IsDone() {
		fmt.Printf("Nameserver initialization in namespace %s not yet finished. Cancelling initialization...\n", n.networkNamespace)
		n.initManager.Cancel()
		n.initManager.Wait()
	}

	ctx, _ := context.WithTimeout(context.Background(), 1*time.Second)

	if n.udpServer != nil {
		if err := n.udpServer.ShutdownContext(ctx); err != nil {
			return errors.WithMessagef(err, "Error shutting down UDP DNS server of namespace %s", n.networkNamespace)
		}
	} else {
		fmt.Printf("No UDP DNS server of namespace %s.\n", n.networkNamespace)
	}
	if n.tcpServer != nil {
		if err := n.tcpServer.ShutdownContext(ctx); err != nil {
			return errors.WithMessagef(err, "Error shutting down TCP DNS server of namespace %s", n.networkNamespace)
		}
	} else {
		fmt.Printf("No TCP DNS server of namespace %s.\n", n.networkNamespace)
	}

	return nil
}

func (n *nameserver) AddValidNetworkID(validNetworkID string) {
	n.Lock()
	defer n.Unlock()

	n.validNetworkIDs.Set(validNetworkID, struct{}{})
}

func (n *nameserver) RemoveValidNetworkID(validNetworkID string) {
	n.Lock()
	defer n.Unlock()

	n.validNetworkIDs.Remove(validNetworkID)
}

// ServeDNS handles DNS queries
func (n *nameserver) ServeDNS(w dns.ResponseWriter, r *dns.Msg) {
	msg := dns.Msg{}
	msg.SetReply(r)
	msg.Authoritative = true

	// Iterate through all questions (usually one)
	for _, q := range r.Question {
		if q.Qtype == dns.TypeA {
			// TODO: This results in a parse error on the DNS client side if more than a single result
			//   is being returned. Can be tested by creating a service with a fixed hostname
			msg.Answer = append(msg.Answer, n.resolver.ResolveName(q.Name, n.validNetworkIDs.Keys())...)
		} else if q.Qtype == dns.TypePTR {
			msg.Answer = append(msg.Answer, n.resolver.ResolveIP(q.Name, n.validNetworkIDs.Keys())...)
		}

		if len(msg.Answer) == 0 {
			// TODO: Use DNS servers specified in daemon and container
			c := new(dns.Client)
			c.Net = "udp"
			in, _, err := c.Exchange(r, "8.8.4.4:53")
			if err != nil {
				c.Net = "tcp"
				in, _, err = c.Exchange(r, "8.8.4.4:53")
				if err != nil {
					log.Printf("Failed to forward DNS query from namespace %s: %v", n.networkNamespace, err)
					msg.SetRcode(r, dns.RcodeServerFailure)
					break
				}
			}
			msg.Answer = append(msg.Answer, in.Answer...)
			msg.Ns = append(msg.Ns, in.Ns...)
			msg.Extra = append(msg.Extra, in.Extra...)
			msg.Authoritative = false
		}
	}

	err := w.WriteMsg(&msg)
	if err != nil {
		log.Printf("Failed to write DNS response in namespace %s: %v", n.networkNamespace, err)
	}
}

// startDnsServersInNamespace sets up DNS servers within the specified network namespace
func (n *nameserver) startDnsServersInNamespace(ctx context.Context) error {
	runtime.LockOSThread()

	if err := n.setNamespace(ctx); err != nil {
		return errors.WithMessagef(err, "Error setting namespace to %s", n.networkNamespace)
	}

	if !n.isHookAvailable {
		go func() {
			if err := n.setAllNetworksAsValid(); err != nil {
				log.Printf("Error setting all networks as valid of container with namespace %s: %v", n.networkNamespace, err)
			}
		}()
	}

	portTCP, err := n.startDNSServer("tcp", ctx)
	if err != nil {
		return errors.WithMessagef(err, "Failed to start DNS server TCP in namespace %s", n.networkNamespace)
	}

	portUDP, err := n.startDNSServer("udp", ctx)
	if err != nil {
		return errors.WithMessagef(err, "Failed to start DNS server UDP in namespace %s", n.networkNamespace)
	}

	fmt.Printf("Both servers for %s have been started\n", n.networkNamespace)

	if err = n.replaceDNATSNATRules(ctx); err != nil {
		return errors.WithMessagef(err, "Failed to replace DNAT SNAT rules in namespace %s", n.networkNamespace)
	}

	if n.isHookAvailable {
		if err := n.setAllNetworksAsValid(); err != nil {
			log.Printf("Error setting all networks as valid of container with namespace %s: %v", n.networkNamespace, err)
		}
	}

	fmt.Printf("Namespace %s DNS servers listening on TCP: 127.0.0.33:%d, UDP: 127.0.0.33:%d\n", n.networkNamespace, portTCP, portUDP)
	return nil
}

func (n *nameserver) setAllNetworksAsValid() error {
	dockerClient, err := client.NewClientWithOpts(
		client.WithHost("unix:///var/run/docker.sock"),
		client.WithAPIVersionNegotiation(),
	)

	if err != nil {
		return errors.WithMessage(err, "Error creating docker client")
	}

	links, err := netlink.LinkList()
	if err != nil {
		return errors.WithMessagef(err, "error listing network interfaces when setting valid networks in namespace %s", n.networkNamespace)
	}

	linkAddresses := lo.Map(links, func(item netlink.Link, _ int) struct {
		name  string
		addrs []netlink.Addr
	} {
		addrs, _ := netlink.AddrList(item, netlink.FAMILY_ALL)
		return struct {
			name  string
			addrs []netlink.Addr
		}{item.Attrs().Name, addrs}
	})
	fmt.Printf("Found %d interfaces in container: %v\n", len(links), spew.Sdump(linkAddresses))
	networks, err := dockerClient.NetworkList(context.Background(), network.ListOptions{})
	for _, network := range networks {
		if network.EnableIPv6 {
			continue
		}
		for _, addresses := range linkAddresses {
			for _, address := range addresses.addrs {
				if network.IPAM.Config != nil {
					for _, ipamConfig := range network.IPAM.Config {
						_, subnet, err := net.ParseCIDR(ipamConfig.Subnet)
						if err != nil {
							log.Printf("Error parsing CIDR IPAM subnet %s: %v", ipamConfig.Subnet, err)
						} else {
							if subnet.Contains(address.IP) {
								n.AddValidNetworkID(network.ID)
							}
						}
					}
				}
			}
		}
	}
	return nil
}

// startDNSServer initializes and starts the DNS server for either TCP or UDP
func (n *nameserver) startDNSServer(connType string, ctx context.Context) (int, error) {

	listenAddr := fmt.Sprintf("%s:0", n.listenIP)

	server := &dns.Server{Handler: n}
	var port int

	switch connType {
	case "udp":
		// Create UDP listener
		udpConn, err := net.ListenPacket("udp", listenAddr)
		if err != nil {
			return 0, errors.WithMessagef(err, "Failed to create UDP listener on %s: %v", listenAddr)
		}

		// Retrieve the assigned port
		udpPort := udpConn.LocalAddr().(*net.UDPAddr).Port
		port = udpPort

		fmt.Printf("Started UDP on port %d\n", udpPort)

		// Initialize DNS server with the UDP connection
		server.PacketConn = udpConn
		select {
		case <-ctx.Done():
			udpConn.Close()
			return 0, ctx.Err()
		default:
		}
		n.udpServer = server
		n.portUDP = port
	case "tcp":
		// Create TCP listener
		tcpListener, err := net.Listen("tcp", listenAddr)
		if err != nil {
			return 0, errors.WithMessagef(err, "Failed to create TCP listener on %s: %v", listenAddr)
		}

		// Retrieve the assigned port
		tcpPort := tcpListener.Addr().(*net.TCPAddr).Port
		port = tcpPort

		fmt.Printf("Started TCP on port %d\n", tcpPort)

		// Initialize DNS server with the TCP listener
		server.Listener = tcpListener
		select {
		case <-ctx.Done():
			tcpListener.Close()
			return 0, ctx.Err()
		default:
		}
		n.tcpServer = server
		n.portTCP = port
	default:
		return 0, fmt.Errorf("unsupported connection type: %s", connType)
	}

	go func() {
		// Start the DNS server
		if err := server.ActivateAndServe(); err != nil {
			log.Printf("Failed to start DNS server (%s) on %s: %v", connType, listenAddr, err)
		}
	}()
	return port, nil
}

// replaceDNATSNATRules replaces existing DNAT and SNAT iptables rules with new ones
// that route DNS traffic to the specified ports of the DNS servers.
func (n *nameserver) replaceDNATSNATRules(ctx context.Context) error {
	ipt, err := iptables.New()
	if err != nil {
		return errors.WithMessage(err, "Error initializing iptables")
	}

	table := "nat"

	rulesToDelete := map[string][]string{}
	flannelDnsOutputExists, err := ipt.ChainExists(table, "FLANNEL_DNS_OUTPUT")
	if err != nil {
		return errors.WithMessagef(err, "Error checking if chain FLANNEL_DNS_OUTPUT exists in table %s", table)
	}

	if flannelDnsOutputExists {
		rules, err := ipt.List("nat", "FLANNEL_DNS_OUTPUT")
		if err != nil {
			log.Printf("Error listing iptables rules in namespace %s, table %s, chain FLANNEL_DNS_OUTPUT", n.networkNamespace, table)
		} else {
			rulesToDelete["FLANNEL_DNS_OUTPUT"] = rules
		}
	}

	rules := []networking.IptablesRule{
		{
			Chain: "FLANNEL_DNS_OUTPUT",
			RuleSpec: []string{
				"-d", "127.0.0.11/32",
				"-p", "tcp",
				"-m", "tcp",
				"--dport", "53",
				"-j", "DNAT",
				"--to-destination", fmt.Sprintf("%s:%d", n.listenIP, n.portTCP),
			},
		},
		{
			Chain: "FLANNEL_DNS_OUTPUT",
			RuleSpec: []string{
				"-d", "127.0.0.11/32",
				"-p", "udp",
				"-m", "udp",
				"--dport", "53",
				"-j", "DNAT",
				"--to-destination", fmt.Sprintf("%s:%d", n.listenIP, n.portUDP),
			},
		},
		{
			Chain: "FLANNEL_DNS_POSTROUTING",
			RuleSpec: []string{
				"-s", fmt.Sprintf("%s/32", n.listenIP),
				"-p", "tcp",
				"-m", "tcp",
				"--sport", fmt.Sprintf("%d", n.portTCP),
				"-j", "SNAT",
				"--to-source", ":53",
			},
		},
		{
			Chain: "FLANNEL_DNS_POSTROUTING",
			RuleSpec: []string{
				"-s", fmt.Sprintf("%s/32", n.listenIP),
				"-p", "udp",
				"-m", "udp",
				"--sport", fmt.Sprintf("%d", n.portUDP),
				"-j", "SNAT",
				"--to-source", ":53",
			},
		},
		{
			Chain: "OUTPUT",
			RuleSpec: []string{
				"-d", "127.0.0.11",
				"-j", "FLANNEL_DNS_OUTPUT",
			},
		},
		{
			Chain: "POSTROUTING",
			RuleSpec: []string{
				"-d", "127.0.0.11",
				"-j", "FLANNEL_DNS_POSTROUTING",
			},
		},
	}

	for _, rule := range rules {
		fmt.Printf("Applying iptables rule %+v\n", rule)
		if err := createChainIfNecessary(ipt, table, rule.Chain); err != nil {
			return errors.WithMessagef(err, "Error in namespace %s", n.networkNamespace)
		}
		if err := ipt.Insert(table, rule.Chain, 1, rule.RuleSpec...); err != nil {
			return errors.WithMessagef(err, "Error applying iptables rule in namespace %s, table %s, chain %s", n.networkNamespace, rule.Table, rule.Chain)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
	}

	if !flannelDnsOutputExists {
		dockerChains := []string{"DOCKER_OUTPUT", "DOCKER_POSTROUTING"}

		start := time.Now()
		if err := waitForChainsWithRules(ipt, table, [][]string{dockerChains}, 30*time.Second, ctx); err != nil {
			return err
		} else {
			fmt.Printf("Chains exist and have at least one rule in namespace %s after %s\n", n.networkNamespace, time.Since(start))
		}

		for _, chain := range dockerChains {
			rules, err := ipt.List("nat", chain)
			if err != nil {
				log.Printf("Error listing iptables rules in namespace %s, table %s, chain %s", n.networkNamespace, table, chain)
			}
			rulesToDelete[chain] = rules
		}
	}

	for chain, rules := range rulesToDelete {
		for _, rawRule := range rules {
			select {
			case <-ctx.Done():
				return ctx.Err()
			default:
			}
			rule := strings.Fields(rawRule)[2:]
			if len(rule) == 0 {
				continue
			}
			err = ipt.Delete(table, chain, rule...)
			if err != nil {
				log.Printf("Failed to delete rule in chain %s: %v, err:%v", chain, rule, err)
			} else {
				fmt.Printf("Deleted rule in chain %s: %v\n", chain, rule)
			}
		}
	}

	return nil
}

func createChainIfNecessary(ipt *iptables.IPTables, table, chain string) error {
	exists, err := ipt.ChainExists(table, chain)
	if err != nil {
		return errors.WithMessagef(err, "Error checking if chain %s exists in table %s", chain, table)
	}

	if !exists {
		if err := ipt.NewChain(table, chain); err != nil {
			return errors.WithMessagef(err, "Error creating chain %s in table %s", chain, table)
		}
	}

	return nil
}

// Waits until at least one chains group has rules for each chain in the group
func waitForChainsWithRules(ipt *iptables.IPTables, table string, chainsGroups [][]string, timeout time.Duration, ctx context.Context) error {
	start := time.Now()
	deadline := start.Add(timeout)

	for {
		for _, chainsGroup := range chainsGroups {
			allChainsReady := true

			for _, chain := range chainsGroup {
				select {
				case <-ctx.Done():
					return ctx.Err()
				default:
				}
				// Check if the chain exists
				chainExists, err := ipt.ChainExists(table, chain)
				if err != nil {
					return fmt.Errorf("error checking if chain exists: %w", err)
				}
				if !chainExists {
					allChainsReady = false
					break
				}

				// Check if the chain has at least one rule
				rules, err := ipt.List(table, chain)
				if err != nil {
					return fmt.Errorf("error listing rules for chain %s: %w", chain, err)
				}
				if len(rules) <= 1 { // The first line is usually a header
					allChainsReady = false
					break
				}
			}

			if allChainsReady {
				return nil
			}

			if time.Now().After(deadline) {
				return errors.New("timeout waiting for chains with rules")
			}
		}
		time.Sleep(5 * time.Millisecond) // Wait before retrying
	}
}

func (n *nameserver) setNamespace(ctx context.Context) error {
	// Retry mechanism for setting namespace
	// Wait for 6 seconds max
	// TODO: And then what? Shouldn't we wait indefinitely? Or somehow crash the
	//   container if we can't set the namespace and therefore the DNS server?)
	var lastErr error
	maxWaitTime := 6 * time.Second
	start := time.Now()
	deadline := start.Add(maxWaitTime)
	delay := 10 * time.Millisecond
	if !n.isHookAvailable {
		delay = 1 * time.Millisecond
	}
	for {
		targetNS, err := netns.GetFromPath(n.networkNamespace)
		if err == nil {
			err = netns.Set(targetNS)
			targetNS.Close()
		}
		if err == nil {
			fmt.Printf("Successfully set namespace %s after %s\n", n.networkNamespace, time.Since(start))
			return nil
		} else {
			lastErr = err
			time.Sleep(delay)
			select {
			case <-ctx.Done():
				return ctx.Err()
			default:
			}
		}

		if time.Now().After(deadline) {
			break
		}
	}

	// Log final error details before returning.
	return errors.WithMessagef(lastErr, "failed to set namespace %s after %s", n.networkNamespace, maxWaitTime)
}

// The node path /var/run/docker is mounted to /hostfs/var/run/docker and the sandbox keys
// are of form /var/run/docker/netns/<key>
func adjustNamespacePath(namespacePath string) string {
	if strings.Index(namespacePath, "/hostfs") == 0 {
		return namespacePath
	}

	return "/hostfs" + namespacePath
}
