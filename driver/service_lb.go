package driver

import (
	"fmt"
	"github.com/coreos/go-iptables/iptables"
	"github.com/moby/ipvs"
	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
	"log"
	"net"
	"strconv"
)

func getLoadBalancerInterfaceName(flannelNetworkId string) string {
	return getInterfaceName("lb", "_", flannelNetworkId)
}

func ensureLoadBalancerInterface(flannelNetworkId string, network *FlannelNetwork) (netlink.Link, error) {
	interfaceName := getLoadBalancerInterfaceName(flannelNetworkId)

	link, err := ensureInterface(interfaceName, "dummy", network.config.MTU, true)

	if err != nil {
		log.Printf("Error ensuring load balancer interface %s for flannel network ID %s exists", interfaceName, flannelNetworkId)
		return nil, err
	}

	return link, nil
}

func (d *FlannelDriver) EnsureLoadBalancerConfigurationForService(flannelNetworkId string, network *FlannelNetwork, serviceId string) error {
	link, err := ensureLoadBalancerInterface(flannelNetworkId, network)
	if err != nil {
		return err
	}

	fwmark, err := d.etcdClient.EnsureFwmark(network, serviceId)
	if err != nil {
		return err
	}

	vip, err := d.etcdClient.EnsureServiceVip(network, serviceId)

	err = ensureInterfaceListensOnAddress(link, vip)
	if err != nil {
		return err
	}
	err = ensureServiceLoadBalancerFrontend(fwmark, vip)
	if err != nil {
		return err
	}

	return nil
}

func ensureServiceLoadBalancerFrontend(fwmark uint32, vip string) error {
	handle, err := ipvs.New("")
	if err != nil {
		return fmt.Errorf("failed to initialize IPVS handle: %v", err)
	}
	defer handle.Close()

	vipIP := net.ParseIP(vip)
	if vipIP == nil {
		return fmt.Errorf("invalid VIP address: %s", vip)
	}

	svc := &ipvs.Service{
		FWMark:    fwmark,
		SchedName: "rr",
	}

	exists := handle.IsServicePresent(svc)
	if !exists {
		fmt.Printf("IPVS service for fwmark %d and IP %s didn't exist. Creating...", fwmark, vip)
		err = handle.NewService(svc)
		if err != nil {
			return fmt.Errorf("failed to create IPVS service: %v", err)
		}
	} else {
		existingSvc, err := handle.GetService(svc)
		if err != nil {
			return fmt.Errorf("failed to get existing IPVS service: %v", err)
		}

		if existingSvc.SchedName != svc.SchedName || existingSvc.Flags != svc.Flags || existingSvc.Timeout != svc.Timeout || existingSvc.PEName != svc.PEName {
			err = handle.UpdateService(svc)
			return fmt.Errorf("failed to update existing IPVS service: %v", err)
		}
	}

	fwmarkStr := strconv.FormatUint(uint64(fwmark), 10)

	iptablev4, err := iptables.New()
	if err != nil {
		log.Printf("Error initializing iptables: %v", err)
		return err
	}

	rules := []struct {
		table    string
		chain    string
		action   func(string, string, ...string) error
		ruleSpec []string
	}{
		{
			table:  "nat",
			chain:  "POSTROUTING",
			action: iptablev4.AppendUnique,
			ruleSpec: []string{
				"-d", vip,
				"-m", "mark",
				"--mark", fwmarkStr,
				"-j", "MASQUERADE",
			},
		},
		{
			table:  "mangle",
			chain:  "PREROUTING",
			action: iptablev4.AppendUnique,
			ruleSpec: []string{
				"-d", vip,
				"-p", "udp",
				"-j", "MARK",
				"--set-mark", fwmarkStr,
			},
		},
		{
			table:  "mangle",
			chain:  "PREROUTING",
			action: iptablev4.AppendUnique,
			ruleSpec: []string{
				"-d", vip,
				"-p", "tcp",
				"-j", "MARK",
				"--set-mark", fwmarkStr,
			},
		},
	}

	for _, rule := range rules {
		if err := rule.action(rule.table, rule.chain, rule.ruleSpec...); err != nil {
			log.Printf("Error applying iptables rule in table %s, chain %s: %v", rule.table, rule.chain, err)
			return err
		}
	}

	return nil
}

func (d *FlannelDriver) EnsureServiceLoadBalancerBackend(network *FlannelNetwork, serviceID, ip string) error {
	fwmark, err := d.etcdClient.EnsureFwmark(network, serviceID)
	if err != nil {
		return err
	}
	svc := &ipvs.Service{
		FWMark:    fwmark,
		SchedName: "rr",
	}
	handle, err := ipvs.New("")
	if err != nil {
		return fmt.Errorf("failed to initialize IPVS handle: %v", err)
	}
	defer handle.Close()

	exists := handle.IsServicePresent(svc)
	if !exists {
		return fmt.Errorf("IPVS service for docker service %s does not exist", serviceID)
	}
	backendIP := net.ParseIP(ip)
	if backendIP == nil {
		return fmt.Errorf("invalid backend IP address: %s", ip)
	}
	err = handle.NewDestination(svc, &ipvs.Destination{
		AddressFamily:   unix.AF_INET,
		Address:         backendIP,
		Port:            0,
		Weight:          1,
		ConnectionFlags: 0,
	})

	if err != nil {
		err = handle.UpdateDestination(svc, &ipvs.Destination{
			AddressFamily:   unix.AF_INET,
			Address:         backendIP,
			Port:            0,
			Weight:          1,
			ConnectionFlags: 0,
		})
	}

	return err
}

func configureIPVS(fwmark int, vip string, backendIPs []string) error {
	handle, err := ipvs.New("")
	if err != nil {
		return fmt.Errorf("failed to initialize IPVS handle: %v", err)
	}
	defer handle.Close()

	vipIP := net.ParseIP(vip)
	if vipIP == nil {
		return fmt.Errorf("invalid VIP IP address: %s", vip)
	}

	svc := &ipvs.Service{
		FWMark:    uint32(fwmark),
		SchedName: "rr",
	}

	exists := handle.IsServicePresent(svc)
	if !exists {
		err = handle.NewService(svc)
		if err != nil {
			return fmt.Errorf("failed to create IPVS service: %v", err)
		}
	} else {
		existingSvc, err := handle.GetService(svc)
		if err != nil {
			return fmt.Errorf("failed to get existing IPVS service: %v", err)
		}

		if existingSvc.SchedName != svc.SchedName || existingSvc.Flags != svc.Flags || existingSvc.Timeout != svc.Timeout || existingSvc.PEName != svc.PEName {
			err = handle.UpdateService(svc)
			return fmt.Errorf("failed to update existing IPVS service: %v", err)
		}
	}

	existingDestinations, err := handle.GetDestinations(svc)
	if err != nil {
		return fmt.Errorf("failed to get destinations for IPVS service: %v", err)
	}

	desiredBackends := make(map[string]struct{})
	for _, ipStr := range backendIPs {
		desiredBackends[ipStr] = struct{}{}
	}

	existingBackends := make(map[string]*ipvs.Destination)
	for _, dest := range existingDestinations {
		destIP := dest.Address.String()
		existingBackends[destIP] = dest
	}

	for _, ipStr := range backendIPs {
		if _, found := existingBackends[ipStr]; !found {
			backendIP := net.ParseIP(ipStr)
			if backendIP == nil {
				return fmt.Errorf("invalid backend IP address: %s", ipStr)
			}

			dest := &ipvs.Destination{
				AddressFamily:   unix.AF_INET,
				Address:         backendIP,
				Port:            0, // Port (0 means all ports)
				Weight:          1, // Weight for load balancing
				ConnectionFlags: 0,
			}

			err = handle.NewDestination(svc, dest)
			if err != nil {
				return fmt.Errorf("failed to add destination %s: %v", ipStr, err)
			}
		}
	}

	for ipStr, dest := range existingBackends {
		if _, found := desiredBackends[ipStr]; !found {
			err = handle.DelDestination(svc, dest)
			if err != nil {
				return fmt.Errorf("failed to delete destination %s: %v", ipStr, err)
			}
		}
	}

	return nil
}
