// from https://github.com/KatharaFramework/NetworkPlugin/blob/main/bridge/go-src/src/bridge_utils.go

package driver

import (
	"fmt"
	"github.com/coreos/go-iptables/iptables"
	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netlink/nl"
	"golang.org/x/sys/unix"
	"log"
)

func getBridgeName(flannelNetworkId string) string {
	return getInterfaceName("fl", "-", flannelNetworkId)
}

func ensureBridge(network *FlannelNetwork) error {
	bridgeName := network.bridgeName

	bridge, err := ensureInterface(bridgeName, "bridge", network.config.MTU, true)
	if err != nil {
		log.Printf("Error ensuring bridge interface exists %s: %v", bridgeName, err)
		return err
	}

	ones, _ := network.config.Subnet.Mask.Size()
	ip := fmt.Sprintf("%s/%d", network.config.Gateway, ones)

	err = replaceIPsOfInterface(bridge, []string{ip})
	if err != nil {
		log.Printf("Error updating IP of bridge %s: %v", bridgeName, err)
		return err
	}

	if err := patchBridge(bridge); err != nil {
		log.Printf("Error patching bridge %v: %v", bridgeName, err)
		return err
	}

	route := &netlink.Route{
		Dst:       network.config.Subnet,
		Src:       network.config.Gateway,
		LinkIndex: bridge.Attrs().Index,
		Scope:     netlink.SCOPE_LINK,
		Protocol:  unix.RTPROT_KERNEL,
	}

	if err := netlink.RouteChange(route); err != nil {
		log.Printf("Failed to add route: %+v, err:%+v\n", route, err)
		return err
	}

	err = setupIptables(network)
	if err != nil {
		log.Printf("Error setting up iptables: %v", err)
		return err
	}

	return nil
}

func setupIptables(network *FlannelNetwork) error {
	bridgeName := network.bridgeName

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
				"-s", network.config.Subnet.String(),
				"!", "-o", bridgeName,
				"-j", "MASQUERADE",
			},
		},
		{
			table:  "nat",
			chain:  "DOCKER",
			action: iptablev4.AppendUnique,
			ruleSpec: []string{
				"-i", bridgeName,
				"-j", "RETURN",
			},
		},
		{
			table:  "filter",
			chain:  "FORWARD",
			action: iptablev4.AppendUnique,
			ruleSpec: []string{
				"-i", bridgeName,
				"-o", bridgeName,
				"-j", "ACCEPT",
			},
		},
		{
			table:  "filter",
			chain:  "FORWARD",
			action: iptablev4.AppendUnique,
			ruleSpec: []string{
				"-i", bridgeName,
				"!", "-o", bridgeName,
				"-j", "ACCEPT",
			},
		},
		{
			table:  "filter",
			chain:  "FORWARD",
			action: iptablev4.AppendUnique,
			ruleSpec: []string{
				"-o", bridgeName,
				"-j", "DOCKER",
			},
		},
		{
			table:  "filter",
			chain:  "FORWARD",
			action: iptablev4.AppendUnique,
			ruleSpec: []string{
				"-o", bridgeName,
				"-m", "conntrack",
				"--ctstate", "RELATED,ESTABLISHED",
				"-j", "ACCEPT",
			},
		},
		{
			table:  "filter",
			chain:  "DOCKER-ISOLATION-STAGE-1",
			action: iptablev4.AppendUnique,
			ruleSpec: []string{
				"-i", bridgeName,
				"!", "-o", bridgeName,
				"-j", "DOCKER-ISOLATION-STAGE-2",
			},
		},
		{
			table:  "filter",
			chain:  "DOCKER-ISOLATION-STAGE-2",
			action: iptablev4.AppendUnique,
			ruleSpec: []string{
				"-o", bridgeName,
				"-j", "DROP",
			},
		},
	}

	// Loop over the rules and apply them
	for _, rule := range rules {
		if err := rule.action(rule.table, rule.chain, rule.ruleSpec...); err != nil {
			log.Printf("Error applying iptables rule in table %s, chain %s: %v", rule.table, rule.chain, err)
			return err
		}
	}

	return nil
}

func patchBridge(bridge netlink.Link) error {
	// Creates a new RTM_NEWLINK request
	// NLM_F_ACK is used to receive acks when operations are executed
	req := nl.NewNetlinkRequest(unix.RTM_NEWLINK, unix.NLM_F_ACK)

	// Search for the bridge interface by its index (and bring it UP too)
	msg := nl.NewIfInfomsg(unix.AF_UNSPEC)
	msg.Change = unix.IFF_UP
	msg.Flags = unix.IFF_UP
	msg.Index = int32(bridge.Attrs().Index)
	req.AddData(msg)

	// Patch ageing_time and group_fwd_mask
	linkInfo := nl.NewRtAttr(unix.IFLA_LINKINFO, nil)
	linkInfo.AddRtAttr(nl.IFLA_INFO_KIND, nl.NonZeroTerminated(bridge.Type()))

	data := linkInfo.AddRtAttr(nl.IFLA_INFO_DATA, nil)
	data.AddRtAttr(nl.IFLA_BR_AGEING_TIME, nl.Uint32Attr(0))
	data.AddRtAttr(nl.IFLA_BR_GROUP_FWD_MASK, nl.Uint16Attr(0xfff8))

	req.AddData(linkInfo)

	// Execute the request. NETLINK_ROUTE is used to send link updates.
	_, err := req.Execute(unix.NETLINK_ROUTE, 0)
	if err != nil {
		return err
	}

	return nil
}

func deleteBridge(netID string) error {
	bridgeName := getBridgeName(netID)

	bridge, err := netlink.LinkByName(bridgeName)
	if err != nil {
		return err
	}

	if err := netlink.LinkDel(bridge); err != nil {
		return err
	}

	// TODO: Delete IP tables

	return nil
}

func attachInterfaceToBridge(bridgeName string, interfaceName string) error {
	bridge, err := netlink.LinkByName(bridgeName)
	if err != nil {
		return err
	}

	iface, err := netlink.LinkByName(interfaceName)
	if err != nil {
		return err
	}

	if err := netlink.LinkSetMaster(iface, bridge); err != nil {
		return err
	}
	if err := netlink.LinkSetUp(iface); err != nil {
		return err
	}

	return nil
}
