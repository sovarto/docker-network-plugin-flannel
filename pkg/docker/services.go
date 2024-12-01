package docker

import (
	"context"
	"fmt"
	"github.com/docker/docker/api/types"
	"github.com/pkg/errors"
	"github.com/sovarto/FlannelNetworkPlugin/pkg/common"
	"github.com/sovarto/FlannelNetworkPlugin/pkg/etcd"
	"log"
	"net"
)

func (d *data) initServices() error {
	d.Lock()
	defer d.Unlock()

	if d.isManagerNode {
		servicesInfos, err := d.getServicesInfosFromDocker()

		err = d.services.(etcd.WriteOnlyStore[common.ServiceInfo]).Init(servicesInfos)
		if err != nil {
			return errors.WithMessage(err, "Error initializing services")
		}
	} else {
		err := d.services.(etcd.ReadOnlyStore[common.ServiceInfo]).Init()
		if err != nil {
			return errors.WithMessage(err, "Error initializing services")
		}
	}

	return nil
}

func (d *data) syncServices() error {
	d.Lock()
	defer d.Unlock()

	fmt.Println("Syncing services...")

	if d.isManagerNode {
		servicesInfos, err := d.getServicesInfosFromDocker()

		err = d.services.(etcd.WriteOnlyStore[common.ServiceInfo]).Sync(servicesInfos)
		if err != nil {
			return errors.WithMessage(err, "Error syncing services")
		}
	} else {
		err := d.services.(etcd.ReadOnlyStore[common.ServiceInfo]).Sync()
		if err != nil {
			return errors.WithMessage(err, "Error syncing services")
		}
	}

	return nil
}

func (d *data) getServicesInfosFromDocker() (servicesInfos map[string]common.ServiceInfo, err error) {
	rawServices, err := d.dockerClient.ServiceList(context.Background(), types.ServiceListOptions{})
	if err != nil {
		return nil, errors.WithMessage(err, "Error listing docker services")
	}

	servicesInfos = map[string]common.ServiceInfo{}

	for _, service := range rawServices {
		serviceInfo, err := d.getServiceInfoFromDocker(service.ID)
		if err != nil {
			log.Printf("Error getting service info for service  with ID %s. Skipping...\n", service.ID)
			continue
		}
		servicesInfos[serviceInfo.ID] = *serviceInfo
	}

	return
}

func (d *data) getServiceInfoFromDocker(serviceID string) (serviceInfo *common.ServiceInfo, err error) {
	service, _, err := d.dockerClient.ServiceInspectWithRaw(context.Background(), serviceID, types.ServiceInspectOptions{})
	if err != nil {
		return nil, errors.WithMessagef(err, "Error inspecting docker service %s", serviceID)
	}

	ipamVIPs := make(map[string]net.IP)

	serviceInfo = &common.ServiceInfo{
		ID:       serviceID,
		Name:     service.Spec.Name,
		IpamVIPs: ipamVIPs,
	}

	for _, endpoint := range service.Endpoint.VirtualIPs {
		ip, _, err := net.ParseCIDR(endpoint.Addr)
		if err != nil {
			return nil, errors.WithMessagef(err, "error parsing IP address %s for service %s", endpoint.Addr, serviceID)
		}
		serviceInfo.IpamVIPs[endpoint.NetworkID] = ip
	}

	return
}

func (d *data) handleService(serviceID string) error {
	serviceInfo, err := d.getServiceInfoFromDocker(serviceID)
	if err != nil {
		return errors.WithMessagef(err, "Error inspecting docker service %s", serviceID)
	}

	if len(serviceInfo.IpamVIPs) == 0 {
		return nil
	}
	err = d.services.(etcd.WriteOnlyStore[common.ServiceInfo]).AddOrUpdateItem(serviceID, *serviceInfo)
	if err != nil {
		return errors.WithMessagef(err, "Error adding or updating service info %s", serviceID)
	}
	return nil
}

func (d *data) handleDeletedService(serviceID string) error {
	return d.services.(etcd.WriteOnlyStore[common.ServiceInfo]).DeleteItem(serviceID)
}
