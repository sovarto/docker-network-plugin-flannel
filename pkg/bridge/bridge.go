package bridge

import (
	"fmt"
	"github.com/pkg/errors"
	"github.com/sovarto/FlannelNetworkPlugin/pkg/common"
	"github.com/sovarto/FlannelNetworkPlugin/pkg/networking"
	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netlink/nl"
	"golang.org/x/sys/unix"
	"log"
	"net"
	"strings"
)

type BridgeInterface interface {
	Ensure() error
	Delete() error
	GetNetworkInfo() common.FlannelNetworkInfo
	CreateAttachedVethPair(mac string) (VethPair, error)
}

type bridgeInterface struct {
	interfaceName string
	iptablesRules []networking.IptablesRule
	network       common.FlannelNetworkInfo
	route         netlink.Route
}

func NewBridgeInterface(network common.FlannelNetworkInfo) BridgeInterface {
	interfaceName := GetBridgeInterfaceName(network.FlannelID)
	return &bridgeInterface{
		interfaceName: interfaceName,
		iptablesRules: getIptablesRules(interfaceName, network.HostSubnet),
		network:       network,
	}
}

func GetBridgeInterfaceName(flannelNetworkID string) string {
	return networking.GetInterfaceName("fl", "-", flannelNetworkID)
}

func Hydrate(network common.FlannelNetworkInfo) (BridgeInterface, error) {
	bridgeInterface := NewBridgeInterface(network).(*bridgeInterface)

	link, err := netlink.LinkByName(bridgeInterface.interfaceName)
	if err != nil {
		return nil, errors.WithMessagef(err, "cannot find bridge interface %s", bridgeInterface.interfaceName)
	}

	bridgeInterface.route = *getRoute(network, link)

	return bridgeInterface, nil
}

func (b *bridgeInterface) GetNetworkInfo() common.FlannelNetworkInfo {
	return b.network
}

func (b *bridgeInterface) Ensure() error {

	bridge, err := networking.EnsureInterface(b.interfaceName, "bridge", b.network.MTU, true)
	if err != nil {
		log.Printf("Error ensuring bridge interface exists %s: %v", b.interfaceName, err)
		return err
	}

	ones, _ := b.network.HostSubnet.Mask.Size()
	ip := fmt.Sprintf("%s/%d", b.network.LocalGateway, ones)

	if err := networking.ReplaceIPsOfInterface(bridge, []string{ip}); err != nil {
		log.Printf("Error updating IP of bridge %s: %v", b.interfaceName, err)
		return err
	}

	if err := patchBridge(bridge); err != nil {
		log.Printf("Error patching bridge %v: %v", b.interfaceName, err)
		return err
	}

	route := getRoute(b.network, bridge)

	if err := netlink.RouteAdd(route); err != nil {
		if strings.Contains(err.Error(), "file exists") {
			if err := netlink.RouteReplace(route); err != nil {
				log.Printf("Failed to set route %+v for interface %s. err:%+v\n", route, b.interfaceName, err)
				return err
			}
		}
	}

	b.route = *route

	if err := networking.ApplyIpTablesRules(b.iptablesRules, "create"); err != nil {
		return errors.WithMessagef(err, "failed to setup IP Tables rules for bridge interface %s for network %s", b.interfaceName, b.network.FlannelID)
	}

	return nil
}

func getRoute(network common.FlannelNetworkInfo, bridge netlink.Link) *netlink.Route {
	return &netlink.Route{
		Dst:       network.HostSubnet,
		Src:       network.LocalGateway,
		LinkIndex: bridge.Attrs().Index,
		Scope:     netlink.SCOPE_LINK,
		Protocol:  unix.RTPROT_KERNEL,
	}
}

func (b *bridgeInterface) Delete() error {
	bridge, err := netlink.LinkByName(b.interfaceName)
	if err != nil {
		return err
	}

	if err := networking.ApplyIpTablesRules(b.iptablesRules, "delete"); err != nil {
		return errors.WithMessagef(err, "failed to delete IP Tables rules for bridge interface for network %s", b.interfaceName)
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

func getIptablesRules(interfaceName string, hostSubnet *net.IPNet) []networking.IptablesRule {
	return []networking.IptablesRule{
		{
			Table: "nat",
			Chain: "POSTROUTING",
			RuleSpec: []string{
				"-s", hostSubnet.String(),
				"!", "-o", interfaceName,
				"-j", "MASQUERADE",
			},
		},
		{
			Table: "nat",
			Chain: "DOCKER",
			RuleSpec: []string{
				"-i", interfaceName,
				"-j", "RETURN",
			},
		},
		{
			Table: "filter",
			Chain: "FORWARD",
			RuleSpec: []string{
				"-i", interfaceName,
				"-o", interfaceName,
				"-j", "ACCEPT",
			},
		},
		{
			Table: "filter",
			Chain: "FORWARD",
			RuleSpec: []string{
				"-i", interfaceName,
				"!", "-o", interfaceName,
				"-j", "ACCEPT",
			},
		},
		{
			Table: "filter",
			Chain: "FORWARD",
			RuleSpec: []string{
				"-o", interfaceName,
				"-j", "DOCKER",
			},
		},
		{
			Table: "filter",
			Chain: "FORWARD",
			RuleSpec: []string{
				"-o", interfaceName,
				"-m", "conntrack",
				"--ctstate", "RELATED,ESTABLISHED",
				"-j", "ACCEPT",
			},
		},
		{
			Table: "filter",
			Chain: "DOCKER-ISOLATION-STAGE-1",
			RuleSpec: []string{
				"-i", interfaceName,
				"!", "-o", interfaceName,
				"-j", "DOCKER-ISOLATION-STAGE-2",
			},
		},
		{
			Table: "filter",
			Chain: "DOCKER-ISOLATION-STAGE-2",
			RuleSpec: []string{
				"-o", interfaceName,
				"-j", "DROP",
			},
		},
	}
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
