package dns

import (
	"fmt"
	"github.com/coreos/go-iptables/iptables"
	"github.com/miekg/dns"
	"github.com/pkg/errors"
	"github.com/sovarto/FlannelNetworkPlugin/pkg/networking"
	"github.com/vishvananda/netns"
	"log"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

type Nameserver interface {
	Activate() error
}

// NamespaceDNS holds information about the DNS server in a namespace
type NamespaceDNS struct {
	Namespace string
	PortTCP   int
	PortUDP   int
	ListenIP  string
}

// dnsHandler implements the dns.Handler interface
type dnsHandler struct {
	Namespace string
}

type nameserver struct {
	networkNamespace string
}

func NewNameserver(networkNamespace string) Nameserver {
	return &nameserver{networkNamespace: networkNamespace}
}

func (n *nameserver) Activate() error {
	if _, err := os.Stat(n.networkNamespace); os.IsNotExist(err) {
		return fmt.Errorf("namespace path does not exist: %s", n.networkNamespace)
	}

	fmt.Printf("Starting nameserver in namespace %s\n", n.networkNamespace)
	go listenInNamespace(n.networkNamespace)

	return nil
}

// ServeDNS handles DNS queries
func (h *dnsHandler) ServeDNS(w dns.ResponseWriter, r *dns.Msg) {
	// Create a new response message
	msg := dns.Msg{}
	msg.SetReply(r)
	msg.Authoritative = true

	// Iterate through all questions (usually one)
	for _, q := range r.Question {
		switch q.Name {
		case "foo.", "foo":
			if q.Qtype == dns.TypeA {
				rr, err := dns.NewRR(fmt.Sprintf("%s A 10.1.2.3", q.Name))
				if err == nil {
					msg.Answer = append(msg.Answer, rr)
				}
			}
		case "bar.", "bar":
			if q.Qtype == dns.TypeA {
				rr, err := dns.NewRR(fmt.Sprintf("%s A 192.0.0.1", q.Name))
				if err == nil {
					msg.Answer = append(msg.Answer, rr)
				}
			}
		default:
			// Forward other queries to 8.8.4.4
			c := new(dns.Client)
			// Attempt UDP first
			c.Net = "udp"
			in, _, err := c.Exchange(r, "8.8.4.4:53")
			if err != nil {
				// If UDP fails, try TCP
				c.Net = "tcp"
				in, _, err = c.Exchange(r, "8.8.4.4:53")
				if err != nil {
					log.Printf("Failed to forward DNS query from namespace %s: %v", h.Namespace, err)
					msg.SetRcode(r, dns.RcodeServerFailure)
					break
				}
			}
			// Append the forwarded response
			msg.Answer = append(msg.Answer, in.Answer...)
			msg.Ns = append(msg.Ns, in.Ns...)
			msg.Extra = append(msg.Extra, in.Extra...)
		}
	}

	// Write the response
	err := w.WriteMsg(&msg)
	if err != nil {
		log.Printf("Failed to write DNS response in namespace %s: %v", h.Namespace, err)
	}
}

// setNetworkNamespace switches the current thread to the specified network namespace
func setNetworkNamespace(nsPath string) (netns.NsHandle, error) {
	// Open the namespace using the netns package
	ns, err := netns.GetFromPath(nsPath)
	if err != nil {
		return 0, fmt.Errorf("failed to open namespace %s: %v", nsPath, err)
	}
	return ns, nil
}

// startDNSServer initializes and starts the DNS server for either TCP or UDP
func startDNSServer(handler dns.Handler, connType string, listenAddr string) (int, error) {

	onStarted := func() {
		fmt.Printf("Started DNS server %s on %s\n", connType, listenAddr)
	}

	var server *dns.Server
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
		server = &dns.Server{
			PacketConn:        udpConn,
			Handler:           handler,
			NotifyStartedFunc: onStarted,
		}

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
		server = &dns.Server{
			Listener:          tcpListener,
			Handler:           handler,
			NotifyStartedFunc: onStarted,
		}

	default:
		return 0, fmt.Errorf("Unsupported connection type: %s", connType)
	}

	go func() {
		// Start the DNS server
		if err := server.ActivateAndServe(); err != nil {
			log.Printf("Failed to start DNS server (%s) on %s: %v", connType, listenAddr, err)
		}
	}()
	return port, nil
}

// listenInNamespace sets up DNS servers within the specified network namespace
func listenInNamespace(nsPath string) {
	// Lock the goroutine to its current OS thread
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	// Save the original network namespace to revert back later
	origNS, err := netns.Get()
	if err != nil {
		log.Printf("Error getting original namespace: %v", err)
		return
	}
	defer origNS.Close()

	// Switch to the target network namespace
	targetNS, err := setNetworkNamespace(nsPath)
	if err != nil {
		log.Printf("Error setting namespace for %s: %v", nsPath, err)
		return
	}
	defer targetNS.Close()

	if err := netns.Set(targetNS); err != nil {
		log.Printf("Failed to set namespace %s: %v", nsPath, err)
		return
	}
	defer netns.Set(origNS) // Revert back to original namespace when done

	listenIP := "127.0.0.33"
	// Define the address to listen on
	address := fmt.Sprintf("%s:0", listenIP) // Port 0 means to choose a random available port

	// Create a DNS handler
	handler := &dnsHandler{
		Namespace: filepath.Base(nsPath),
	}

	fmt.Printf("Starting DNS server TCP in namespace %s\n", nsPath)
	// Start TCP DNS server
	portTCP, err := startDNSServer(handler, "tcp", address)
	if err != nil {
		log.Fatal(err)
	}

	fmt.Printf("Starting DNS server UDP in namespace %s\n", nsPath)
	// Start UDP DNS server
	portUDP, err := startDNSServer(handler, "udp", address)
	if err != nil {
		log.Fatal(err)
	}

	fmt.Printf("Both servers for %s have been started\n", nsPath)

	server := NamespaceDNS{
		Namespace: filepath.Base(nsPath),
		PortTCP:   portTCP,
		PortUDP:   portUDP,
		ListenIP:  listenIP,
	}

	err = replaceDNATSNATRules(server)
	if err != nil {
		log.Fatal(err)
	}

	// Send the DNS server information back to the main goroutine
	//dnsServersChan <- server

	log.Printf("Namespace %s DNS servers listening on TCP: 127.0.0.33:%d, UDP: 127.0.0.33:%d", filepath.Base(nsPath), portTCP, portUDP)
}

// replaceDNATSNATRules replaces existing DNAT and SNAT iptables rules with new ones
// that route DNS traffic to the specified ports of the DNS servers.
func replaceDNATSNATRules(server NamespaceDNS) error {
	ipt, err := iptables.New()
	if err != nil {
		return errors.WithMessage(err, "Error initializing iptables")
	}

	table := "nat"
	chains := []string{"DOCKER_OUTPUT", "DOCKER_POSTROUTING"}
	targetIP := "127.0.0.11"

	// Iterate over each chain to delete existing rules
	for _, chain := range chains {
		// List all rules in the chain
		rules, err := ipt.List(table, chain)
		if err != nil {
			log.Printf("Failed to list rules in chain %s: %v", chain, err)
			continue
		}

		for _, rule := range rules {
			fmt.Printf("Found rule %s", rule)

			ruleArgs := strings.Fields(rule)

			shouldDelete := false

			if chain == "DOCKER_OUTPUT" {
				// Check if '-d 127.0.0.11' is present
				for i := 0; i < len(ruleArgs)-1; i++ {
					if ruleArgs[i] == "-d" && ruleArgs[i+1] == targetIP {
						shouldDelete = true
						break
					}
				}
			} else if chain == "DOCKER_POSTROUTING" {
				// Check if '-s 127.0.0.11' is present
				for i := 0; i < len(ruleArgs)-1; i++ {
					if ruleArgs[i] == "-s" && ruleArgs[i+1] == targetIP {
						shouldDelete = true
						break
					}
				}
			}

			if shouldDelete {
				// Attempt to delete the rule
				err = ipt.Delete(table, chain, ruleArgs...)
				if err != nil {
					log.Printf("Failed to delete rule in chain %s: %v", chain, err)
				} else {
					log.Printf("Deleted rule in chain %s: %v", chain, ruleArgs)
				}
			}
		}
	}

	rules := []networking.IptablesRule{
		{
			Table: "nat",
			Chain: "DOCKER_OUTPUT",
			RuleSpec: []string{
				"-j", "DNAT",
				"-p", "tcp",
				"-d", "127.0.0.11",
				"--dport", "53",
				"--to-destination", fmt.Sprintf("%s:%d", server.ListenIP, server.PortTCP),
			},
		},
		{
			Table: "nat",
			Chain: "DOCKER_OUTPUT",
			RuleSpec: []string{
				"-j", "DNAT",
				"-p", "udp",
				"-d", "127.0.0.11",
				"--dport", "53",
				"--to-destination", fmt.Sprintf("%s:%d", server.ListenIP, server.PortUDP),
			},
		},
		{
			Table: "nat",
			Chain: "DOCKER_POSTROUTING",
			RuleSpec: []string{
				"-j", "SNAT",
				"-p", "tcp",
				"-s", server.ListenIP,
				"--sport", fmt.Sprintf("%d", server.PortTCP),
				"--to-source", "127.0.0.11:53",
			},
		},
		{
			Table: "nat",
			Chain: "DOCKER_POSTROUTING",
			RuleSpec: []string{
				"-j", "SNAT",
				"-p", "udp",
				"-s", server.ListenIP,
				"--sport", fmt.Sprintf("%d", server.PortUDP),
				"--to-source", "127.0.0.11:53",
			},
		},
	}

	namespace := server.Namespace
	for _, rule := range rules {
		fmt.Printf("Applying iptables rule %+v\n", rule)
		if err := ipt.InsertUnique(rule.Table, rule.Chain, 0, rule.RuleSpec...); err != nil {
			log.Printf("Error applying iptables rule in namespace%s, table %s, chain %s: %v", namespace, rule.Table, rule.Chain, err)
			return err
		}
	}

	return nil
}
