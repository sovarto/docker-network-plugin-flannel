package dns

import (
	"context"
	"fmt"
	"github.com/coreos/go-iptables/iptables"
	"github.com/miekg/dns"
	"github.com/pkg/errors"
	"github.com/samber/lo"
	"github.com/sovarto/FlannelNetworkPlugin/pkg/networking"
	"github.com/vishvananda/netns"
	"log"
	"net"
	"os"
	"runtime"
	"strings"
	"sync"
	"time"
)

type Nameserver interface {
	Activate() error
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
	validNetworkIDs  []string
	sync.Mutex
}

// TODO: Add support for dns options on containers, services, docker
// TODO: Add support for host resolver ???
// TODO: Check domainname config of containers whether it's relevant

func NewNameserver(networkNamespace string, resolver Resolver) Nameserver {
	return &nameserver{
		networkNamespace: networkNamespace,
		resolver:         resolver,
		listenIP:         "127.0.0.33",
		validNetworkIDs:  make([]string, 0),
	}
}

func (n *nameserver) Activate() error {
	n.Lock()
	defer n.Unlock()

	if _, err := os.Stat(n.networkNamespace); os.IsNotExist(err) {
		return fmt.Errorf("namespace path does not exist: %s", n.networkNamespace)
	}

	fmt.Printf("Starting nameserver in namespace %s\n", n.networkNamespace)
	go func() {
		err := n.startDnsServersInNamespace()
		if err != nil {
			log.Printf("Error listening in namespace %s: %+v\n", n.networkNamespace, err)
		}
	}()

	return nil
}

func (n *nameserver) DeactivateAndCleanup() error {
	n.Lock()
	defer n.Unlock()

	ctx, _ := context.WithTimeout(context.Background(), 1*time.Second)
	err := n.udpServer.ShutdownContext(ctx)
	if err != nil {
		return errors.WithMessagef(err, "Error shutting down UDP DNS server of namespace %s", n.networkNamespace)
	}
	err = n.tcpServer.ShutdownContext(ctx)
	if err != nil {
		return errors.WithMessagef(err, "Error shutting down TCP DNS server of namespace %s", n.networkNamespace)
	}

	return nil
}

func (n *nameserver) AddValidNetworkID(validNetworkID string) {
	n.Lock()
	defer n.Unlock()

	n.validNetworkIDs = append(n.validNetworkIDs, validNetworkID)
}

func (n *nameserver) RemoveValidNetworkID(validNetworkID string) {
	n.Lock()
	defer n.Unlock()

	n.validNetworkIDs = lo.Filter(n.validNetworkIDs, func(item string, index int) bool {
		return item != validNetworkID
	})
}

// ServeDNS handles DNS queries
func (n *nameserver) ServeDNS(w dns.ResponseWriter, r *dns.Msg) {
	msg := dns.Msg{}
	msg.SetReply(r)
	msg.Authoritative = true

	// Iterate through all questions (usually one)
	for _, q := range r.Question {
		if q.Qtype == dns.TypeA {
			msg.Answer = append(msg.Answer, n.resolver.ResolveName(q.Name, n.validNetworkIDs)...)
		} else if q.Qtype == dns.TypePTR {
			msg.Answer = append(msg.Answer, n.resolver.ResolveIP(q.Name, n.validNetworkIDs)...)
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
func (n *nameserver) startDnsServersInNamespace() error {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	origNS, err := netns.Get()
	if err != nil {
		return errors.WithMessage(err, "Failed to get original namespace")
	}
	defer origNS.Close()

	// Switch to the target network namespace
	targetNS, err := setNetworkNamespace(n.networkNamespace)
	if err != nil {
		return errors.WithMessagef(err, "Failed to set namespace for %s", n.networkNamespace)
	}
	defer targetNS.Close()

	if err := netns.Set(targetNS); err != nil {
		return errors.WithMessagef(err, "Failed to set namespace %s", n.networkNamespace)
	}
	defer netns.Set(origNS)

	fmt.Printf("Starting DNS server TCP in namespace %s\n", n.networkNamespace)
	portTCP, err := n.startDNSServer("tcp")
	if err != nil {
		return errors.WithMessagef(err, "Failed to start DNS server TCP in namespace %s", n.networkNamespace)
	}

	fmt.Printf("Starting DNS server UDP in namespace %s\n", n.networkNamespace)
	portUDP, err := n.startDNSServer("udp")
	if err != nil {
		return errors.WithMessagef(err, "Failed to start DNS server UDP in namespace %s", n.networkNamespace)
	}

	fmt.Printf("Both servers for %s have been started\n", n.networkNamespace)

	err = n.replaceDNATSNATRules()
	if err != nil {
		return errors.WithMessagef(err, "Failed to replace DNAT SNAT rules in namespace %s", n.networkNamespace)
	}

	fmt.Printf("Namespace %s DNS servers listening on TCP: 127.0.0.33:%d, UDP: 127.0.0.33:%d\n", n.networkNamespace, portTCP, portUDP)
	return nil
}

// startDNSServer initializes and starts the DNS server for either TCP or UDP
func (n *nameserver) startDNSServer(connType string) (int, error) {

	onStarted := func() {
		fmt.Printf("Started DNS server %s on %s\n", connType, n.listenIP)
	}

	listenAddr := fmt.Sprintf("%s:0", n.listenIP)

	server := &dns.Server{
		Handler:           n,
		NotifyStartedFunc: onStarted,
	}
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
func (n *nameserver) replaceDNATSNATRules() error {
	ipt, err := iptables.New()
	if err != nil {
		return errors.WithMessage(err, "Error initializing iptables")
	}

	table := "nat"
	// We wait for either:
	// - The docker chains to have any rules in them. Scenario: A container is started while the plugin runs
	// - or, our own output chain to have any rules in them. Scenario: The plugin was restarted and needs to re-inject the nameserver into already running containers
	dockerChains := []string{"DOCKER_OUTPUT", "DOCKER_POSTROUTING"}
	err = waitForChainsWithRules(ipt, table, [][]string{dockerChains, {"FLANNEL_DNS_OUTPUT"}}, 30*time.Second)

	if err != nil {
		return err
	} else {
		fmt.Println("Chains exist and have at least one rule")
	}

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
	}

	for _, chain := range dockerChains {
		rules, err := ipt.List("nat", chain)
		if err != nil {
			log.Printf("Error listing iptables rules in namespace %s, table %s, chain %s", n.networkNamespace, table, chain)
		}
		rulesToDelete[chain] = rules
	}

	for chain, rules := range rulesToDelete {
		for _, rawRule := range rules {
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
func waitForChainsWithRules(ipt *iptables.IPTables, table string, chainsGroups [][]string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)

	for {
		for _, chainsGroup := range chainsGroups {
			allChainsReady := true

			for _, chain := range chainsGroup {
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

// setNetworkNamespace switches the current thread to the specified network namespace
func setNetworkNamespace(nsPath string) (netns.NsHandle, error) {
	ns, err := netns.GetFromPath(nsPath)
	if err != nil {
		return 0, fmt.Errorf("failed to open namespace %s: %v", nsPath, err)
	}
	return ns, nil
}
