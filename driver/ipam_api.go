package driver

import (
	"github.com/docker/go-plugins-helpers/ipam"
	"log"
)

type FlannelIpamPlugin struct {
	driver *FlannelDriver
}

func (f *FlannelIpamPlugin) GetCapabilities() (*ipam.CapabilitiesResponse, error) {
	log.Printf("[IPAM] Received GetCapabilities")
	response := &ipam.CapabilitiesResponse{}
	log.Printf("[IPAM] GetCapabilities response: %+v\n", response)
	return response, nil
}

func (f *FlannelIpamPlugin) GetDefaultAddressSpaces() (*ipam.AddressSpacesResponse, error) {
	log.Printf("[IPAM] Received GetCapabilities")
	response := &ipam.AddressSpacesResponse{
		LocalDefaultAddressSpace:  "FlannelLocal",
		GlobalDefaultAddressSpace: "FlannelGlobal",
	}
	log.Printf("[IPAM] GetDefaultAddressSpaces response: %+v\n", response)

	return response, nil
}

func (f *FlannelIpamPlugin) RequestPool(request *ipam.RequestPoolRequest) (*ipam.RequestPoolResponse, error) {
	log.Printf("[IPAM] Received RequestPool req: %+v\n", request)

	response, err := f.driver.RequestPool(request)
	log.Printf("[IPAM] RequestPool response: %+v; error:%+v\n", response, err)
	return response, err
}

func (f *FlannelIpamPlugin) ReleasePool(request *ipam.ReleasePoolRequest) error {
	log.Printf("[IPAM] Received ReleasePool req: %+v\n", request)

	err := f.driver.ReleasePool(request)
	log.Printf("[IPAM] ReleasePool response: %+v\n", err)
	return err
}

func (f *FlannelIpamPlugin) RequestAddress(request *ipam.RequestAddressRequest) (*ipam.RequestAddressResponse, error) {
	log.Printf("[IPAM] Received RequestAddress req: %+v\n", request)

	response, err := f.driver.RequestAddress(request)
	log.Printf("[IPAM] RequestAddress response: %+v; error:%+v\n", response, err)
	return response, err
}

func (f *FlannelIpamPlugin) ReleaseAddress(request *ipam.ReleaseAddressRequest) error {
	log.Printf("[IPAM] Received ReleaseAddress req: %+v\n", request)

	err := f.driver.ReleaseAddress(request)
	log.Printf("[IPAM] ReleaseAddress response: %+v\n", err)
	return err
}
