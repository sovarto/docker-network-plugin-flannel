package bridge

import (
	"github.com/google/uuid"
	"github.com/pkg/errors"
	"github.com/vishvananda/netlink"
	"net"
	"strings"
)

const (
	vethPrefix = "veth"
	vethLen    = 8
)

type VethPair interface {
	GetOutside() string
	GetInside() string
	Delete() error
}

type vethPair struct {
	insideName  string
	outsideName string
}

func randomVethName() string {
	randomUuid, _ := uuid.NewRandom()

	return vethPrefix + strings.Replace(randomUuid.String(), "-", "", -1)[:vethLen]
}

func (b *bridgeInterface) CreateAttachedVethPair(macAddress string) (VethPair, error) {
	vethInsideName := randomVethName()
	vethOutsideName := randomVethName()

	parsedMacAddress, err := net.ParseMAC(macAddress)

	if err != nil {
		return nil, err
	}

	linkAttrs := netlink.NewLinkAttrs()
	linkAttrs.Name = vethInsideName
	linkAttrs.HardwareAddr = parsedMacAddress
	linkAttrs.MTU = b.network.MTU

	err = netlink.LinkAdd(&netlink.Veth{
		LinkAttrs: linkAttrs,
		PeerName:  vethOutsideName,
	})

	if err != nil {
		return nil, errors.WithMessagef(err, "failed to create veth pair %s / %s for bridge %s", vethOutsideName, vethInsideName, b.interfaceName)
	}

	err = b.attachInterfaceToBridge(vethOutsideName)
	if err != nil {
		return nil, errors.WithMessagef(err, "failed to attach veth %s to bridge %s", vethOutsideName, b.interfaceName)
	}

	return &vethPair{
		insideName:  vethInsideName,
		outsideName: vethOutsideName,
	}, nil
}

func (b *bridgeInterface) attachInterfaceToBridge(interfaceName string) error {
	bridge, err := netlink.LinkByName(b.interfaceName)
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

func (v *vethPair) GetOutside() string {
	return v.outsideName
}

func (v *vethPair) GetInside() string {
	return v.insideName
}

func (v *vethPair) Delete() error {
	iface, err := netlink.LinkByName(v.outsideName)
	if err != nil {
		return err
	}

	if err := netlink.LinkDel(iface); err != nil {
		return err
	}

	return nil
}
