package service_lb

import (
	"net"
)

type NetworkSpecificServiceLb interface {
	AddBackend(ip net.IP) error
	RemoveBackend(ip net.IP) error
	SetBackends(ips []net.IP) error
}

type serviceLb struct {
	networkID  string
	serviceID  string
	fwmark     uint32
	frontendIP net.IP
	backendIPs []net.IP
}

func NewNetworkSpecificServiceLb(networkID, serviceID string, fwmark uint32, frontendIP net.IP) NetworkSpecificServiceLb {
	return &serviceLb{
		networkID:  networkID,
		serviceID:  serviceID,
		fwmark:     fwmark,
		frontendIP: frontendIP,
		backendIPs: make([]net.IP, 0),
	}
}

func (slb *serviceLb) AddBackend(ip net.IP) error     {}
func (slb *serviceLb) RemoveBackend(ip net.IP) error  {}
func (slb *serviceLb) SetBackends(ips []net.IP) error {}
