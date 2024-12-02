package api

import (
	"github.com/docker/go-plugins-helpers/network"
	"github.com/docker/go-plugins-helpers/sdk"
)

const (
	capabilitiesPath    = "/NetworkDriver.GetCapabilities"
	allocateNetworkPath = "/NetworkDriver.AllocateNetwork"
	freeNetworkPath     = "/NetworkDriver.FreeNetwork"
	createNetworkPath   = "/NetworkDriver.CreateNetwork"
	deleteNetworkPath   = "/NetworkDriver.DeleteNetwork"
	createEndpointPath  = "/NetworkDriver.CreateEndpoint"
	endpointInfoPath    = "/NetworkDriver.EndpointOperInfo"
	deleteEndpointPath  = "/NetworkDriver.DeleteEndpoint"
	joinPath            = "/NetworkDriver.Join"
	leavePath           = "/NetworkDriver.Leave"
	discoverNewPath     = "/NetworkDriver.DiscoverNew"
	discoverDeletePath  = "/NetworkDriver.DiscoverDelete"
	programExtConnPath  = "/NetworkDriver.ProgramExternalConnectivity"
	revokeExtConnPath   = "/NetworkDriver.RevokeExternalConnectivity"
)

type NetworkDriver interface {
	GetCapabilities() (*network.CapabilitiesResponse, error)
	CreateNetwork(*network.CreateNetworkRequest) error
	AllocateNetwork(*network.AllocateNetworkRequest) (*network.AllocateNetworkResponse, error)
	DeleteNetwork(*network.DeleteNetworkRequest) error
	FreeNetwork(*network.FreeNetworkRequest) error
	CreateEndpoint(*network.CreateEndpointRequest) (*network.CreateEndpointResponse, error)
	DeleteEndpoint(*network.DeleteEndpointRequest) error
	EndpointInfo(*network.InfoRequest) (*network.InfoResponse, error)
	Join(*network.JoinRequest) (*network.JoinResponse, error)
	Leave(*network.LeaveRequest) error
	DiscoverNew(*network.DiscoveryNotification) error
	DiscoverDelete(*network.DiscoveryNotification) error
	ProgramExternalConnectivity(*network.ProgramExternalConnectivityRequest) error
	RevokeExternalConnectivity(*network.RevokeExternalConnectivityRequest) error
	IsInitialized() bool
}

func InitNetworkMux(h *sdk.Handler, n NetworkDriver) {
	o := CommonHandlerOptions{Api: "Network", IsInitialized: n.IsInitialized}
	h.HandleFunc(capabilitiesPath, MakeHandlerWithOutput(o, n.GetCapabilities))
	h.HandleFunc(createNetworkPath, MakeHandlerWithInput(o, n.CreateNetwork))
	h.HandleFunc(allocateNetworkPath, MakeHandlerWithInputAndOutput(o, n.AllocateNetwork))
	h.HandleFunc(deleteNetworkPath, MakeHandlerWithInput(o, n.DeleteNetwork))
	h.HandleFunc(freeNetworkPath, MakeHandlerWithInput(o, n.FreeNetwork))
	h.HandleFunc(createEndpointPath, MakeHandlerWithInputAndOutput(o, n.CreateEndpoint))
	h.HandleFunc(deleteEndpointPath, MakeHandlerWithInput(o, n.DeleteEndpoint))
	h.HandleFunc(endpointInfoPath, MakeHandlerWithInputAndOutput(o, n.EndpointInfo))
	h.HandleFunc(joinPath, MakeHandlerWithInputAndOutput(o, n.Join))
	h.HandleFunc(leavePath, MakeHandlerWithInput(o, n.Leave))
	h.HandleFunc(discoverNewPath, MakeHandlerWithInput(o, n.DiscoverNew))
	h.HandleFunc(discoverDeletePath, MakeHandlerWithInput(o, n.DiscoverDelete))
	h.HandleFunc(programExtConnPath, MakeHandlerWithInput(o, n.ProgramExternalConnectivity))
	h.HandleFunc(revokeExtConnPath, MakeHandlerWithInput(o, n.RevokeExternalConnectivity))
}
