package api

import (
	"github.com/docker/go-plugins-helpers/ipam"
	"github.com/docker/go-plugins-helpers/sdk"
)

const (
	ipamCapabilitiesPath = "/IpamDriver.GetCapabilities"
	addressSpacesPath    = "/IpamDriver.GetDefaultAddressSpaces"
	requestPoolPath      = "/IpamDriver.RequestPool"
	releasePoolPath      = "/IpamDriver.ReleasePool"
	requestAddressPath   = "/IpamDriver.RequestAddress"
	releaseAddressPath   = "/IpamDriver.ReleaseAddress"
)

type IpamDriver interface {
	GetIpamCapabilities() (*ipam.CapabilitiesResponse, error)
	GetDefaultAddressSpaces() (*ipam.AddressSpacesResponse, error)
	RequestPool(*ipam.RequestPoolRequest) (*ipam.RequestPoolResponse, error)
	ReleasePool(*ipam.ReleasePoolRequest) error
	RequestAddress(*ipam.RequestAddressRequest) (*ipam.RequestAddressResponse, error)
	ReleaseAddress(*ipam.ReleaseAddressRequest) error
	IsInitialized() bool
}

func InitIpamMux(h *sdk.Handler, i IpamDriver) {
	o := CommonHandlerOptions{Api: "IPAM", IsInitialized: func() bool { return true }}
	h.HandleFunc(ipamCapabilitiesPath, MakeHandlerWithOutput(o, i.GetIpamCapabilities))
	o = CommonHandlerOptions{Api: "IPAM", IsInitialized: i.IsInitialized}
	h.HandleFunc(addressSpacesPath, MakeHandlerWithOutput(o, i.GetDefaultAddressSpaces))
	h.HandleFunc(requestPoolPath, MakeHandlerWithInputAndOutput(o, i.RequestPool))
	h.HandleFunc(releasePoolPath, MakeHandlerWithInput(o, i.ReleaseAddress))
	h.HandleFunc(requestAddressPath, MakeHandlerWithInputAndOutput(o, i.RequestAddress))
	h.HandleFunc(releaseAddressPath, MakeHandlerWithInput(o, i.ReleaseAddress))
}
