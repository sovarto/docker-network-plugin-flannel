package driver

import (
	"fmt"
	"github.com/docker/go-plugins-helpers/network"
	"github.com/docker/go-plugins-helpers/sdk"
	"log"
	"net/http"
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

func initNetworkMux(h *sdk.Handler, flannelDriver *FlannelDriver) {
	h.HandleFunc(capabilitiesPath, func(w http.ResponseWriter, r *http.Request) {
		fmt.Printf("[Network] Received GetCapabilities req")

		res := &network.CapabilitiesResponse{
			Scope:             "global",
			ConnectivityScope: "global",
		}
		fmt.Printf("[Network] GetCapabilities response: %+v\n", res)

		sdk.EncodeResponse(w, res, false)
	})
	h.HandleFunc(createNetworkPath, func(w http.ResponseWriter, r *http.Request) {
		req := &network.CreateNetworkRequest{}
		err := sdk.DecodeRequest(w, r, req)
		if err != nil {
			log.Println("[Network] CreateNetwork decode request err:", err)
			sdk.EncodeResponse(w, network.NewErrorResponse(err.Error()), true)
			return
		}
		fmt.Printf("[Network] Received CreateNetwork req: %+v\n", req)
		err = flannelDriver.CreateNetwork(req)
		fmt.Printf("[Network] CreateNetwork response: %+v\n", err)

		if err != nil {
			sdk.EncodeResponse(w, network.NewErrorResponse(err.Error()), true)
			return
		}
		sdk.EncodeResponse(w, struct{}{}, false)
	})
	h.HandleFunc(allocateNetworkPath, func(w http.ResponseWriter, r *http.Request) {
		req := &network.AllocateNetworkRequest{}
		err := sdk.DecodeRequest(w, r, req)
		if err != nil {
			log.Println("[Network] AllocateNetwork decode request err:", err)
			sdk.EncodeResponse(w, network.NewErrorResponse(err.Error()), true)
			return
		}
		fmt.Printf("[Network] Received AllocateNetwork req: %+v\n", req)
		res, err := flannelDriver.AllocateNetwork(req)
		fmt.Printf("[Network] AllocateNetwork response: %+v; error:%+v\n", res, err)

		if err != nil {
			sdk.EncodeResponse(w, network.NewErrorResponse(err.Error()), true)
			return
		}
		sdk.EncodeResponse(w, res, false)
	})
	h.HandleFunc(deleteNetworkPath, func(w http.ResponseWriter, r *http.Request) {
		req := &network.DeleteNetworkRequest{}
		err := sdk.DecodeRequest(w, r, req)
		if err != nil {
			log.Println("[Network] DeleteNetwork decode request err:", err)
			sdk.EncodeResponse(w, network.NewErrorResponse(err.Error()), true)
			return
		}
		fmt.Printf("[Network] Received DeleteNetwork req: %+v\n", req)
		err = flannelDriver.DeleteNetwork(req)
		fmt.Printf("[Network] DeleteNetwork response: %+v\n", err)

		if err != nil {
			sdk.EncodeResponse(w, network.NewErrorResponse(err.Error()), true)
			return
		}
		sdk.EncodeResponse(w, struct{}{}, false)
	})
	h.HandleFunc(freeNetworkPath, func(w http.ResponseWriter, r *http.Request) {
		req := &network.FreeNetworkRequest{}
		err := sdk.DecodeRequest(w, r, req)
		if err != nil {
			log.Println("[Network] FreeNetwork decode request err:", err)
			sdk.EncodeResponse(w, network.NewErrorResponse(err.Error()), true)
			return
		}
		fmt.Printf("[Network] Received FreeNetwork req: %+v\n", req)
		err = flannelDriver.FreeNetwork(req)
		fmt.Printf("[Network] FreeNetwork response: %+v\n", err)
		if err != nil {
			sdk.EncodeResponse(w, network.NewErrorResponse(err.Error()), true)
			return
		}
		sdk.EncodeResponse(w, struct{}{}, false)
	})
	h.HandleFunc(createEndpointPath, func(w http.ResponseWriter, r *http.Request) {
		req := &network.CreateEndpointRequest{}
		err := sdk.DecodeRequest(w, r, req)
		if err != nil {
			log.Println("[Network] CreateEndpoint decode request err:", err)
			sdk.EncodeResponse(w, network.NewErrorResponse(err.Error()), true)
			return
		}
		fmt.Printf("[Network] Received CreateEndpoint req: %+v\n", req)
		res, err := flannelDriver.CreateEndpoint(req)
		fmt.Printf("[Network] CreateEndpoint response: %+v; error:%+v\n", res, err)

		if err != nil {
			sdk.EncodeResponse(w, network.NewErrorResponse(err.Error()), true)
			return
		}
		sdk.EncodeResponse(w, res, false)
	})
	h.HandleFunc(deleteEndpointPath, func(w http.ResponseWriter, r *http.Request) {
		req := &network.DeleteEndpointRequest{}
		err := sdk.DecodeRequest(w, r, req)
		if err != nil {
			log.Println("[Network] DeleteEndpoint decode request err:", err)
			sdk.EncodeResponse(w, network.NewErrorResponse(err.Error()), true)
			return
		}
		fmt.Printf("[Network] Received DeleteEndpoint req: %+v\n", req)
		err = flannelDriver.DeleteEndpoint(req)
		fmt.Printf("[Network] DeleteEndpoint response: %+v\n", err)

		if err != nil {
			sdk.EncodeResponse(w, network.NewErrorResponse(err.Error()), true)
			return
		}
		sdk.EncodeResponse(w, struct{}{}, false)
	})
	h.HandleFunc(endpointInfoPath, func(w http.ResponseWriter, r *http.Request) {
		req := &network.InfoRequest{}
		err := sdk.DecodeRequest(w, r, req)
		if err != nil {
			log.Println("[Network] EndpointInfo decode request err:", err)
			sdk.EncodeResponse(w, network.NewErrorResponse(err.Error()), true)
			return
		}
		fmt.Printf("[Network] Received EndpointOperInfo req: %+v\n", req)
		res, err := flannelDriver.EndpointInfo(req)
		fmt.Printf("[Network] EndpointInfo response: %+v; error:%+v\n", res, err)

		if err != nil {
			sdk.EncodeResponse(w, network.NewErrorResponse(err.Error()), true)
			return
		}
		sdk.EncodeResponse(w, res, false)
	})
	h.HandleFunc(joinPath, func(w http.ResponseWriter, r *http.Request) {
		req := &network.JoinRequest{}
		err := sdk.DecodeRequest(w, r, req)
		if err != nil {
			log.Println("[Network] Join decode request err:", err)
			sdk.EncodeResponse(w, network.NewErrorResponse(err.Error()), true)
			return
		}
		fmt.Printf("[Network] Received Join req: %+v\n", req)
		res, err := flannelDriver.Join(req)
		fmt.Printf("[Network] Join response: %+v; error:%+v\n", res, err)

		if err != nil {
			sdk.EncodeResponse(w, network.NewErrorResponse(err.Error()), true)
			return
		}
		sdk.EncodeResponse(w, res, false)
	})
	h.HandleFunc(leavePath, func(w http.ResponseWriter, r *http.Request) {
		req := &network.LeaveRequest{}
		err := sdk.DecodeRequest(w, r, req)
		if err != nil {
			log.Println("[Network] Leave decode request err:", err)
			sdk.EncodeResponse(w, network.NewErrorResponse(err.Error()), true)
			return
		}
		fmt.Printf("[Network] Received Leave req: %+v\n", req)
		err = flannelDriver.Leave(req)
		fmt.Printf("[Network] Leave response: %+v\n", err)

		if err != nil {
			sdk.EncodeResponse(w, network.NewErrorResponse(err.Error()), true)
			return
		}
		sdk.EncodeResponse(w, struct{}{}, false)
	})
	h.HandleFunc(discoverNewPath, func(w http.ResponseWriter, r *http.Request) {
		req := &network.DiscoveryNotification{}
		err := sdk.DecodeRequest(w, r, req)
		if err != nil {
			log.Println("[Network] DiscoverNew decode request err:", err)
			sdk.EncodeResponse(w, network.NewErrorResponse(err.Error()), true)
			return
		}
		fmt.Printf("[Network] Received DiscoverNew req: %+v\n", req)
		err = flannelDriver.DiscoverNew(req)
		fmt.Printf("[Network] DiscoverNew response: %+v\n", err)
		if err != nil {
			sdk.EncodeResponse(w, network.NewErrorResponse(err.Error()), true)
			return
		}
		sdk.EncodeResponse(w, struct{}{}, false)
	})
	h.HandleFunc(discoverDeletePath, func(w http.ResponseWriter, r *http.Request) {
		req := &network.DiscoveryNotification{}
		err := sdk.DecodeRequest(w, r, req)
		if err != nil {
			log.Println("[Network] DiscoverDelete decode request err:", err)
			sdk.EncodeResponse(w, network.NewErrorResponse(err.Error()), true)
			return
		}
		fmt.Printf("[Network] Received DiscoverDelete req: %+v\n", req)
		err = flannelDriver.DiscoverDelete(req)
		fmt.Printf("[Network] DiscoverDelete response: %+v\n", err)
		if err != nil {
			sdk.EncodeResponse(w, network.NewErrorResponse(err.Error()), true)
			return
		}
		sdk.EncodeResponse(w, struct{}{}, false)
	})
	h.HandleFunc(programExtConnPath, func(w http.ResponseWriter, r *http.Request) {
		req := &network.ProgramExternalConnectivityRequest{}
		err := sdk.DecodeRequest(w, r, req)
		if err != nil {
			log.Println("[Network] ProgramExternalConnectivity decode request err:", err)
			sdk.EncodeResponse(w, network.NewErrorResponse(err.Error()), true)
			return
		}
		fmt.Printf("[Network] Received ProgramExternalConnectivity req: %+v\n", req)
		err = flannelDriver.ProgramExternalConnectivity(req)
		fmt.Printf("[Network] ProgramExternalConnectivity response: %+v\n", err)
		if err != nil {
			sdk.EncodeResponse(w, network.NewErrorResponse(err.Error()), true)
			return
		}
		sdk.EncodeResponse(w, struct{}{}, false)
	})
	h.HandleFunc(revokeExtConnPath, func(w http.ResponseWriter, r *http.Request) {
		req := &network.RevokeExternalConnectivityRequest{}
		err := sdk.DecodeRequest(w, r, req)
		if err != nil {
			log.Println("[Network] RevokeExternalConnectivity decode request err:", err)
			sdk.EncodeResponse(w, network.NewErrorResponse(err.Error()), true)
			return
		}
		fmt.Printf("[Network] Received RevokeExternalConnectivity req: %+v\n", req)
		err = flannelDriver.RevokeExternalConnectivity(req)
		fmt.Printf("[Network] RevokeExternalConnectivity response: %+v\n", err)
		if err != nil {
			sdk.EncodeResponse(w, network.NewErrorResponse(err.Error()), true)
			return
		}
		sdk.EncodeResponse(w, struct{}{}, false)
	})
}
