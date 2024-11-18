package driver

import (
	"github.com/docker/go-plugins-helpers/ipam"
	"log"
)

type FlannelIpamPlugin struct {
	driver *FlannelDriver
}

func (f *FlannelIpamPlugin) GetCapabilities() (*ipam.CapabilitiesResponse, error) {
	return &ipam.CapabilitiesResponse{}, nil
}

func (f *FlannelIpamPlugin) GetDefaultAddressSpaces() (*ipam.AddressSpacesResponse, error) {
	return &ipam.AddressSpacesResponse{
		LocalDefaultAddressSpace:  "FlannelLocal",
		GlobalDefaultAddressSpace: "FlannelGlobal",
	}, nil
}

func (f *FlannelIpamPlugin) RequestPool(request *ipam.RequestPoolRequest) (*ipam.RequestPoolResponse, error) {
	log.Printf("Received RequestPool req: %+v\n", request)

	response, err := f.driver.RequestPool(request)
	log.Printf("RequestPool response: %+v; error:%+v\n", response, err)
	return response, err
}

func (f *FlannelIpamPlugin) ReleasePool(request *ipam.ReleasePoolRequest) error {
	log.Printf("Received ReleasePool req: %+v\n", request)

	err := f.driver.ReleasePool(request)
	log.Printf("ReleasePool response: %+v\n", err)
	return err
}

func (f *FlannelIpamPlugin) RequestAddress(request *ipam.RequestAddressRequest) (*ipam.RequestAddressResponse, error) {
	log.Printf("Received RequestAddress req: %+v\n", request)

	response, err := f.driver.RequestAddress(request)
	log.Printf("RequestAddress response: %+v; error:%+v\n", response, err)
	return response, err
}

func (f *FlannelIpamPlugin) ReleaseAddress(request *ipam.ReleaseAddressRequest) error {
	log.Printf("Received ReleaseAddress req: %+v\n", request)

	err := f.driver.ReleaseAddress(request)
	log.Printf("ReleaseAddress response: %+v\n", err)
	return err
}
