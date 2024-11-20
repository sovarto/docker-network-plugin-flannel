package driver

import (
	"fmt"
	"github.com/docker/docker/libnetwork/ns"
	"github.com/vishvananda/netlink"
	"log"
	"net"
	"strings"
)

const (
	maxInterfaceNameLength = 15
)

func interfaceExists(name string, interfaceType string) (bool, error) {
	nlh := ns.NlHandle()
	link, err := nlh.LinkByName(name)

	if err != nil {
		if strings.Contains(err.Error(), "Link not found") {
			return false, nil
		}

		return false, fmt.Errorf("failed to check %s interface existence: %v", interfaceType, err)
	}

	if link.Type() == interfaceType {
		return true, nil
	}

	return true, fmt.Errorf("existing interface %s is not a %s", interfaceType, name)
}

func getInterfaceName(prefix string, separator string, flannelNetworkId string) string {
	name := prefix + separator + flannelNetworkId
	if len(name) > maxInterfaceNameLength {
		name = name[:maxInterfaceNameLength]
	}
	return name
}

func replaceIPsOfInterface(link netlink.Link, ips []string) error {
	parsedIPs := []*netlink.Addr{}
	for _, ip := range ips {
		addr, err := netlink.ParseAddr(fmt.Sprintf("%s/32", ip))
		if err != nil {
			log.Printf("Failed to parse IP address %s: %v\n", ip, err)
			return err
		}
		parsedIPs = append(parsedIPs, addr)
	}

	for _, addr := range parsedIPs {
		if err := netlink.AddrReplace(link, addr); err != nil {
			log.Printf("Failed to add IP address %s to interface: %v\n", addr.String(), err)
			return err
		}
	}

	addrs, err := netlink.AddrList(link, netlink.FAMILY_ALL)
	if err != nil {
		log.Printf("Failed to get IP addresses from interface: %v\n", err)
		return err
	}

	for _, a := range addrs {
		found := false
		for _, b := range parsedIPs {
			if addressesEqual(&a, b) {
				found = true
				break
			}
		}
		if !found {
			if err := netlink.AddrDel(link, &a); err != nil {
				log.Printf("Failed to remove IP address %v from interface: %v\n", a, err)
				return err
			}
		}
	}

	return nil
}

func ensureInterfaceListensOnAddress(link netlink.Link, ip string) error {
	addr, err := netlink.ParseAddr(fmt.Sprintf("%s/32", ip))
	if err != nil {
		log.Printf("Failed to parse IP address %s: %v\n", ip, err)
		return err
	}
	if err := netlink.AddrReplace(link, addr); err != nil {
		log.Printf("Failed to add IP address %s to interface: %v\n", addr.String(), err)
		return err
	}
	return nil
}

func addressesEqual(a1, a2 *netlink.Addr) bool {
	return a1.IPNet.String() == a2.IPNet.String()
}

func ensureInterface(name string, interfaceType string, mtu int, up bool) (netlink.Link, error) {
	exists, err := interfaceExists(name, interfaceType)
	if err != nil {
		return nil, err
	}

	if !exists {
		linkAttrs := netlink.NewLinkAttrs()
		linkAttrs.Name = name
		linkAttrs.MTU = mtu
		if up {
			linkAttrs.Flags = net.FlagUp
		}

		var link netlink.Link
		switch interfaceType {
		case "dummy":
			link = &netlink.Dummy{LinkAttrs: linkAttrs}
			break
		case "bridge":
			link = &netlink.Bridge{LinkAttrs: linkAttrs}
			break
		default:
			return nil, fmt.Errorf("unknown interface type: %s", interfaceType)
		}
		err := netlink.LinkAdd(link)
		if err != nil {
			log.Printf("Error adding %s interface %s: %v", interfaceType, name, err)
			return nil, err
		}

		fmt.Printf("Added %s interface %s\n", interfaceType, name)
	}

	link, err := netlink.LinkByName(name)
	if err != nil {
		log.Printf("Error getting %s interface %s by name: %v", interfaceType, name, err)
		return nil, err
	}

	return link, nil
}
