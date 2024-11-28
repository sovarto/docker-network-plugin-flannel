package service_lb

import (
	"fmt"
	"github.com/moby/ipvs"
	"github.com/pkg/errors"
	"github.com/samber/lo"
	"github.com/sovarto/FlannelNetworkPlugin/pkg/networking"
	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
	"net"
	"strconv"
)

type NetworkSpecificServiceLb interface {
	AddBackend(ip net.IP) error
	RemoveBackend(ip net.IP) error
	SetBackends(ips []net.IP) error
	Delete() error
	GetFrontendIP() net.IP
	GetFwmark() uint32
	UpdateFrontendIP(ip net.IP) error
}

type serviceLb struct {
	networkID     string
	serviceID     string
	fwmark        uint32
	frontendIP    net.IP
	backendIPs    []net.IP
	iptablesRules []networking.IptablesRule
	link          netlink.Link
}

func NewNetworkSpecificServiceLb(link netlink.Link, networkID, serviceID string, fwmark uint32, frontendIP net.IP) (NetworkSpecificServiceLb, error) {

	err := networking.EnsureInterfaceListensOnAddress(link, frontendIP.String())
	if err != nil {
		return nil, errors.Wrapf(err, "Failed to ensure load balancer interface %s listening on %s", link.Attrs().Name, frontendIP.String())
	}

	slb := &serviceLb{
		networkID:  networkID,
		serviceID:  serviceID,
		fwmark:     fwmark,
		frontendIP: frontendIP,
		backendIPs: make([]net.IP, 0),
		link:       link,
	}

	err = slb.ensureServiceLoadBalancerFrontend()
	if err != nil {
		return nil, errors.Wrapf(err, "error creating frontend of service load balancer for service %s and network %s", serviceID, networkID)
	}

	return slb, nil
}

func (slb *serviceLb) UpdateFrontendIP(ip net.IP) error {
	err := networking.EnsureInterfaceListensOnAddress(slb.link, ip.String())
	if err != nil {
		return errors.Wrapf(err, "Failed to ensure load balancer interface %s listening on %s", slb.link.Attrs().Name, ip.String())
	}

	oldFrontendIP := slb.frontendIP
	slb.frontendIP = ip
	err = slb.ensureServiceLoadBalancerFrontend()
	if err != nil {
		return errors.Wrapf(err, "error creating frontend of service load balancer for service %s and network %s", slb.serviceID, slb.networkID)
	}

	err = networking.StopListeningOnAddress(slb.link, oldFrontendIP.String())
	if err != nil {
		return errors.Wrapf(err, "error stopping interface %s listening on %s", slb.link.Attrs().Name, oldFrontendIP.String())
	}

	return nil
}

func (slb *serviceLb) GetFrontendIP() net.IP { return slb.frontendIP }
func (slb *serviceLb) GetFwmark() uint32     { return slb.fwmark }

func (slb *serviceLb) AddBackend(ip net.IP) error {
	svc := &ipvs.Service{
		FWMark:    slb.fwmark,
		SchedName: "rr",
	}
	handle, err := ipvs.New("")
	if err != nil {
		return fmt.Errorf("failed to initialize IPVS handle: %v", err)
	}
	defer handle.Close()

	exists := handle.IsServicePresent(svc)
	if !exists {
		return fmt.Errorf("IPVS service for docker service %s and network %s does not exist", slb.serviceID, slb.networkID)
	}

	err = handle.NewDestination(svc, &ipvs.Destination{
		AddressFamily:   unix.AF_INET,
		Address:         ip,
		Port:            0,
		Weight:          1,
		ConnectionFlags: 0,
	})

	if err != nil {
		err = handle.UpdateDestination(svc, &ipvs.Destination{
			AddressFamily:   unix.AF_INET,
			Address:         ip,
			Port:            0,
			Weight:          1,
			ConnectionFlags: 0,
		})
	}

	slb.backendIPs = append(slb.backendIPs, ip)

	return err
}
func (slb *serviceLb) RemoveBackend(ip net.IP) error {
	svc := &ipvs.Service{
		FWMark:    slb.fwmark,
		SchedName: "rr",
	}
	handle, err := ipvs.New("")
	if err != nil {
		return fmt.Errorf("failed to initialize IPVS handle: %v", err)
	}
	defer handle.Close()

	exists := handle.IsServicePresent(svc)
	if !exists {
		return fmt.Errorf("IPVS service for docker service %s and network %s does not exist", slb.serviceID, slb.networkID)
	}

	err = handle.DelDestination(svc, &ipvs.Destination{
		AddressFamily:   unix.AF_INET,
		Address:         ip,
		Port:            0,
		Weight:          1,
		ConnectionFlags: 0,
	})

	if err != nil {
		return errors.Wrapf(err, "error deleting backend %s from service load balancer for service %s and network %s", ip, slb.serviceID, slb.networkID)
	}

	slb.backendIPs = lo.Filter(slb.backendIPs, func(item net.IP, index int) bool {
		return !item.Equal(ip)
	})

	return nil
}

func (slb *serviceLb) SetBackends(ips []net.IP) error {
	svc := &ipvs.Service{
		FWMark:    slb.fwmark,
		SchedName: "rr",
	}
	handle, err := ipvs.New("")
	if err != nil {
		return fmt.Errorf("failed to initialize IPVS handle: %v", err)
	}
	defer handle.Close()

	exists := handle.IsServicePresent(svc)
	if !exists {
		return fmt.Errorf("IPVS service for docker service %s and network %s does not exist", slb.serviceID, slb.networkID)
	}

	existingDests, err := handle.GetDestinations(svc)
	if err != nil {
		return fmt.Errorf("failed to get existing destinations: %v", err)
	}

	// Create maps for efficient lookup
	existingIPs := make(map[string]*ipvs.Destination)
	for _, dest := range existingDests {
		existingIPs[dest.Address.String()] = dest
	}

	desiredIPs := make(map[string]net.IP)
	for _, ip := range ips {
		desiredIPs[ip.String()] = ip
	}

	// Add new destinations
	for ipStr, ip := range desiredIPs {
		if _, found := existingIPs[ipStr]; !found {
			dest := &ipvs.Destination{
				Address:         ip,
				Port:            0,
				Weight:          1,
				ConnectionFlags: ipvs.ConnectionFlagMasq,
			}
			err = handle.NewDestination(svc, dest)
			if err != nil {
				return errors.Wrapf(err, "failed to add backend ip %s to service load balancer for service %s and networks %s", ip, slb.serviceID, slb.networkID)
			}
		}
	}

	// Remove destinations that are no longer desired
	for ipStr, dest := range existingIPs {
		if _, found := desiredIPs[ipStr]; !found {
			err = handle.DelDestination(svc, dest)
			if err != nil {
				return errors.Wrapf(err, "failed to delete backend ip %s from service load balancer for service %s and networks %s", ipStr, slb.serviceID, slb.networkID)
			}
		}
	}

	slb.backendIPs = ips

	return nil
}

func (slb *serviceLb) ensureServiceLoadBalancerFrontend() error {
	vip := slb.frontendIP.String()
	vipIP := net.ParseIP(vip)
	if vipIP == nil {
		return fmt.Errorf("invalid VIP address: %s", vip)
	}

	handle, err := ipvs.New("")
	if err != nil {
		return fmt.Errorf("failed to initialize IPVS handle: %v", err)
	}
	defer handle.Close()

	svc := &ipvs.Service{
		FWMark:    slb.fwmark,
		SchedName: "rr",
	}

	exists := handle.IsServicePresent(svc)
	if !exists {
		fmt.Printf("IPVS service for fwmark %d and IP %s didn't exist. Creating...\n", slb.fwmark, vip)
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

	fwmarkStr := strconv.FormatUint(uint64(slb.fwmark), 10)

	var previousIptablesRules []networking.IptablesRule

	if slb.iptablesRules != nil && len(slb.iptablesRules) > 0 {
		copy(previousIptablesRules, slb.iptablesRules)
	}

	slb.iptablesRules = []networking.IptablesRule{
		{
			Table: "nat",
			Chain: "POSTROUTING",
			RuleSpec: []string{
				"-d", vip,
				"-m", "mark",
				"--mark", fwmarkStr,
				"-j", "MASQUERADE",
			},
		},
		{
			Table: "mangle",
			Chain: "PREROUTING",
			RuleSpec: []string{
				"-d", vip,
				"-p", "udp",
				"-j", "MARK",
				"--set-mark", fwmarkStr,
			},
		},
		{
			Table: "mangle",
			Chain: "PREROUTING",
			RuleSpec: []string{
				"-d", vip,
				"-p", "tcp",
				"-j", "MARK",
				"--set-mark", fwmarkStr,
			},
		},
	}

	err = networking.ApplyIpTablesRules(slb.iptablesRules, "create")
	if err != nil {
		return errors.Wrapf(err, "failed to setup IP Tables rules for service load balancer for service %s in network %s", slb.serviceID, slb.networkID)
	}

	if len(previousIptablesRules) > 0 {
		err = networking.ApplyIpTablesRules(previousIptablesRules, "delete")
		if err != nil {
			return errors.Wrapf(err, "failed to setup IP Tables rules for service load balancer for service %s in network %s", slb.serviceID, slb.networkID)
		}
	}

	return nil
}

func (slb *serviceLb) Delete() error {
	err := networking.ApplyIpTablesRules(slb.iptablesRules, "delete")
	if err != nil {
		return errors.Wrapf(err, "failed to remove IP Tables rules for service load balancer for service %s in network %s", slb.serviceID, slb.networkID)
	}

	handle, err := ipvs.New("")
	if err != nil {
		return fmt.Errorf("failed to initialize IPVS handle: %v", err)
	}
	defer handle.Close()

	svc := &ipvs.Service{
		FWMark:    slb.fwmark,
		SchedName: "rr",
	}

	err = handle.DelService(svc)
	if err != nil {
		return errors.Wrapf(err, "failed to delete IPVS service of service load balancer of service %s and network %s", slb.serviceID, slb.networkID)
	}

	err = networking.StopListeningOnAddress(slb.link, slb.frontendIP.String())
	if err != nil {
		return errors.Wrapf(err, "failed to remove IP %s from interface %s", slb.frontendIP, slb.link.Attrs().Name)
	}

	return nil
}
