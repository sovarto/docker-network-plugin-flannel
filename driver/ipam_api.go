package driver

import (
	"fmt"
	"github.com/docker/go-plugins-helpers/ipam"
	"github.com/docker/go-plugins-helpers/sdk"
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
		fmt.Printf("[IPAM] Received GetCapabilities\n")
		res := &ipam.CapabilitiesResponse{RequiresMACAddress: true}
		fmt.Printf("[IPAM] GetCapabilities response: %+v\n", res)
		sdk.EncodeResponse(w, res, false)
	})
	h.HandleFunc(addressSpacesPath, func(w http.ResponseWriter, r *http.Request) {
		fmt.Printf("[IPAM] Received GetDefaultAddressSpaces\n")
		res := &ipam.AddressSpacesResponse{
			LocalDefaultAddressSpace:  "FlannelLocal",
			GlobalDefaultAddressSpace: "FlannelGlobal",
		}
		fmt.Printf("[IPAM] GetDefaultAddressSpaces response: %+v\n", res)
		sdk.EncodeResponse(w, res, false)
	})
	h.HandleFunc(requestPoolPath, func(w http.ResponseWriter, r *http.Request) {
		req := &ipam.RequestPoolRequest{}
		err := sdk.DecodeRequest(w, r, req)
		if err != nil {
			return
		}
		fmt.Printf("[IPAM] Received RequestPool req: %+v\n", req)
		res, err := flannelDriver.RequestPool(req)
		fmt.Printf("[IPAM] RequestPool response: %+v; error:%+v\n", res, err)
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
		fmt.Printf("[IPAM] Received ReleasePool req: %+v\n", req)
		err = flannelDriver.ReleasePool(req)
		fmt.Printf("[IPAM] ReleasePool response: %+v\n", err)
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
		fmt.Printf("[IPAM] Received RequestAddress req: %+v\n", req)
		res, err := flannelDriver.RequestAddress(req)
		fmt.Printf("[IPAM] RequestAddress res: %+v; error:%+v\n", res, err)
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
		fmt.Printf("[IPAM] Received ReleaseAddress req: %+v\n", req)
		err = flannelDriver.ReleaseAddress(req)
		fmt.Printf("[IPAM] ReleaseAddress response: %+v\n", err)
		if err != nil {
			sdk.EncodeResponse(w, ipam.NewErrorResponse(err.Error()), true)
			return
		}
		sdk.EncodeResponse(w, struct{}{}, false)
	})
}
