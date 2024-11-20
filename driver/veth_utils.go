// from https://github.com/KatharaFramework/NetworkPlugin/blob/main/bridge/go-src/src/veth_utils.go

package driver

import (
	"net"
	"strings"

	"github.com/google/uuid"
	"github.com/vishvananda/netlink"
)

const (
	vethPrefix = "veth"
	vethLen    = 8
)

func randomVethName() string {
	randomUuid, _ := uuid.NewRandom()

	return vethPrefix + strings.Replace(randomUuid.String(), "-", "", -1)[:vethLen]
}

func createVethPair(network *FlannelNetwork, macAddress string) (string, string, error) {
	vethName1 := randomVethName()
	vethName2 := randomVethName()

	parsedMacAddress, err := net.ParseMAC(macAddress)

	if err != nil {
		return "", "", err
	}

	linkAttrs := netlink.NewLinkAttrs()
	linkAttrs.Name = vethName1
	linkAttrs.HardwareAddr = parsedMacAddress
	linkAttrs.MTU = network.config.MTU

	err = netlink.LinkAdd(&netlink.Veth{
		LinkAttrs: linkAttrs,
		PeerName:  vethName2,
	})

	if err != nil {
		return "", "", err
	}

	return vethName1, vethName2, nil
}

func deleteVethPair(vethOutside string) error {
	iface, err := netlink.LinkByName(vethOutside)
	if err != nil {
		return err
	}

	if err := netlink.LinkDel(iface); err != nil {
		return err
	}

	return nil
}
