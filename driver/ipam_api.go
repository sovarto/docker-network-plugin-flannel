package driver

import (
	"github.com/docker/go-plugins-helpers/ipam"
	"github.com/docker/go-plugins-helpers/sdk"
	"log"
	"net/http"
)

const (
	ipamCapabilitiesPath = "/IpamDriver.GetCapabilities"
	addressSpacesPath    = "/IpamDriver.GetDefaultAddressSpaces"
	requestPoolPath      = "/IpamDriver.RequestPool"
	releasePoolPath      = "/IpamDriver.ReleasePool"
	requestAddressPath   = "/IpamDriver.RequestAddress"
	releaseAddressPath   = "/IpamDriver.ReleaseAddress"
)

func initIpamMux(h *sdk.Handler, flannelDriver *FlannelDriver) {
	h.HandleFunc(ipamCapabilitiesPath, func(w http.ResponseWriter, r *http.Request) {
		log.Printf("[IPAM] Received GetCapabilities")
		res := &ipam.CapabilitiesResponse{}
		log.Printf("[IPAM] GetCapabilities response: %+v\n", res)
		sdk.EncodeResponse(w, res, false)
	})
	h.HandleFunc(addressSpacesPath, func(w http.ResponseWriter, r *http.Request) {
		log.Printf("[IPAM] Received GetDefaultAddressSpaces")
		res := &ipam.AddressSpacesResponse{
			LocalDefaultAddressSpace:  "FlannelLocal",
			GlobalDefaultAddressSpace: "FlannelGlobal",
		}
		log.Printf("[IPAM] GetDefaultAddressSpaces response: %+v\n", res)
		sdk.EncodeResponse(w, res, false)
	})
	h.HandleFunc(requestPoolPath, func(w http.ResponseWriter, r *http.Request) {
		req := &ipam.RequestPoolRequest{}
		err := sdk.DecodeRequest(w, r, req)
		if err != nil {
			return
		}
		log.Printf("[IPAM] Received RequestPool req: %+v\n", req)
		res, err := flannelDriver.RequestPool(req)
		log.Printf("[IPAM] RequestPool response: %+v; error:%+v\n", res, err)
		if err != nil {
			sdk.EncodeResponse(w, ipam.NewErrorResponse(err.Error()), true)
			return
		}
		sdk.EncodeResponse(w, res, false)
	})
	h.HandleFunc(releasePoolPath, func(w http.ResponseWriter, r *http.Request) {
		req := &ipam.ReleasePoolRequest{}
		err := sdk.DecodeRequest(w, r, req)
		if err != nil {
			return
		}
		log.Printf("[IPAM] Received ReleasePool req: %+v\n", req)
		err = flannelDriver.ReleasePool(req)
		log.Printf("[IPAM] ReleasePool response: %+v\n", err)
		if err != nil {
			sdk.EncodeResponse(w, ipam.NewErrorResponse(err.Error()), true)
			return
		}
		sdk.EncodeResponse(w, struct{}{}, false)
	})
	h.HandleFunc(requestAddressPath, func(w http.ResponseWriter, r *http.Request) {
		req := &ipam.RequestAddressRequest{}
		err := sdk.DecodeRequest(w, r, req)
		if err != nil {
			return
		}
		log.Printf("[IPAM] Received RequestAddress req: %+v\n", req)
		res, err := flannelDriver.RequestAddress(req)
		log.Printf("[IPAM] RequestAddress res: %+v; error:%+v\n", res, err)
		if err != nil {
			sdk.EncodeResponse(w, ipam.NewErrorResponse(err.Error()), true)
			return
		}
		sdk.EncodeResponse(w, res, false)
	})
	h.HandleFunc(releaseAddressPath, func(w http.ResponseWriter, r *http.Request) {
		req := &ipam.ReleaseAddressRequest{}
		err := sdk.DecodeRequest(w, r, req)
		if err != nil {
			return
		}
		log.Printf("[IPAM] Received ReleaseAddress req: %+v\n", req)
		err = flannelDriver.ReleaseAddress(req)
		log.Printf("[IPAM] ReleaseAddress response: %+v\n", err)
		if err != nil {
			sdk.EncodeResponse(w, ipam.NewErrorResponse(err.Error()), true)
			return
		}
		sdk.EncodeResponse(w, struct{}{}, false)
	})
}
