package bridge

import (
	"fmt"
	"github.com/coreos/go-iptables/iptables"
	"github.com/pkg/errors"
	"github.com/sovarto/FlannelNetworkPlugin/pkg/common"
	"github.com/sovarto/FlannelNetworkPlugin/pkg/interface_management"
	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netlink/nl"
	"golang.org/x/sys/unix"
	"log"
	"net"
)

type BridgeInterface interface {
	Ensure() error
	Delete() error
}

type iptablesRule struct {
	table    string
	chain    string
	ruleSpec []string
}

type bridgeInterface struct {
	interfaceName string
	iptablesRules []iptablesRule
	network       common.NetworkInfo
	route         netlink.Route
}

func NewBridgeInterface(network common.NetworkInfo) (BridgeInterface, error) {
	interfaceName := interface_management.GetInterfaceName("fl", "-", network.ID)
	result := &bridgeInterface{
		interfaceName: interfaceName,
		iptablesRules: getIptablesRules(interfaceName, network.HostSubnet),
	}

	err := result.Ensure()

	if err != nil {
		return nil, errors.Wrapf(err, "failed to ensure bridge interface for network %s", network.ID)
	}

	return result, nil
}

func (b *bridgeInterface) Ensure() error {

	bridge, err := interface_management.EnsureInterface(b.interfaceName, "bridge", b.network.MTU, true)
	if err != nil {
		log.Printf("Error ensuring bridge interface exists %s: %v", b.interfaceName, err)
		return err
	}

	ones, _ := b.network.HostSubnet.Mask.Size()
	ip := fmt.Sprintf("%s/%d", b.network.LocalGateway, ones)

	if err := interface_management.ReplaceIPsOfInterface(bridge, []string{ip}); err != nil {
		log.Printf("Error updating IP of bridge %s: %v", b.interfaceName, err)
		return err
	}

	if err := patchBridge(bridge); err != nil {
		log.Printf("Error patching bridge %v: %v", b.interfaceName, err)
		return err
	}

	route := &netlink.Route{
		Dst:       &b.network.HostSubnet,
		Src:       b.network.LocalGateway,
		LinkIndex: bridge.Attrs().Index,
		Scope:     netlink.SCOPE_LINK,
		Protocol:  unix.RTPROT_KERNEL,
	}

	if err := netlink.RouteChange(route); err != nil {
		log.Printf("Failed to add route: %+v, err:%+v\n", route, err)
		return err
	}

	b.route = *route

	if err := applyIpTablesRules(b.iptablesRules, "create"); err != nil {
		return errors.Wrapf(err, "failed to setup IP Tables rules for bridge interface for network %s", b.interfaceName)
	}

	return nil
}

func (b *bridgeInterface) Delete() error {
	bridge, err := netlink.LinkByName(b.interfaceName)
	if err != nil {
		return err
	}

	if err := applyIpTablesRules(b.iptablesRules, "delete"); err != nil {
		return errors.Wrapf(err, "failed to delete IP Tables rules for bridge interface for network %s", b.interfaceName)
	}

	if err := netlink.RouteDel(&b.route); err != nil {
		log.Printf("Failed to delete route: %+v, err:%+v\n", b.route, err)
		return err
	}

	if err := netlink.LinkDel(bridge); err != nil {
		return err
	}

	return nil
}

func getIptablesRules(interfaceName string, hostSubnet net.IPNet) []iptablesRule {
	return []iptablesRule{
		{
			table: "nat",
			chain: "POSTROUTING",
			ruleSpec: []string{
				"-s", hostSubnet.String(),
				"!", "-o", interfaceName,
				"-j", "MASQUERADE",
			},
		},
		{
			table: "nat",
			chain: "DOCKER",
			ruleSpec: []string{
				"-i", interfaceName,
				"-j", "RETURN",
			},
		},
		{
			table: "filter",
			chain: "FORWARD",
			ruleSpec: []string{
				"-i", interfaceName,
				"-o", interfaceName,
				"-j", "ACCEPT",
			},
		},
		{
			table: "filter",
			chain: "FORWARD",
			ruleSpec: []string{
				"-i", interfaceName,
				"!", "-o", interfaceName,
				"-j", "ACCEPT",
			},
		},
		{
			table: "filter",
			chain: "FORWARD",
			ruleSpec: []string{
				"-o", interfaceName,
				"-j", "DOCKER",
			},
		},
		{
			table: "filter",
			chain: "FORWARD",
			ruleSpec: []string{
				"-o", interfaceName,
				"-m", "conntrack",
				"--ctstate", "RELATED,ESTABLISHED",
				"-j", "ACCEPT",
			},
		},
		{
			table: "filter",
			chain: "DOCKER-ISOLATION-STAGE-1",
			ruleSpec: []string{
				"-i", interfaceName,
				"!", "-o", interfaceName,
				"-j", "DOCKER-ISOLATION-STAGE-2",
			},
		},
		{
			table: "filter",
			chain: "DOCKER-ISOLATION-STAGE-2",
			ruleSpec: []string{
				"-o", interfaceName,
				"-j", "DROP",
			},
		},
	}
}

func applyIpTablesRules(rules []iptablesRule, action string) error {
	iptablev4, err := iptables.New()
	if err != nil {
		log.Printf("Error initializing iptables: %v", err)
		return err
	}

	var a func(table string, chain string, rulespec ...string) error
	switch action {
	case "create":
		a = iptablev4.AppendUnique
		break
	case "delete":
		a = iptablev4.Delete
		break
	default:
		return fmt.Errorf("invalid action. specify 'create' or 'delete'")
	}

	for _, rule := range rules {
		if err := a(rule.table, rule.chain, rule.ruleSpec...); err != nil {
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
