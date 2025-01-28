package docker

import (
	"context"
	"fmt"
	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/swarm"
	"github.com/pkg/errors"
	"github.com/samber/lo"
	"github.com/sovarto/FlannelNetworkPlugin/pkg/common"
	"github.com/sovarto/FlannelNetworkPlugin/pkg/etcd"
	"log"
	"net"
)

func (d *data) initServices() error {
	d.Lock()
	defer d.Unlock()

	if d.isManagerNode {
		fmt.Println("Initializing services on manager node...")
		servicesInfos, err := d.getServicesInfosFromDocker()

		err = d.services.(etcd.WriteOnlyStore[ServiceInfo]).Init(servicesInfos)
		if err != nil {
			return errors.WithMessage(err, "Error initializing services")
		}
	} else {
		fmt.Println("Initializing services on worker node...")
		err := d.services.(etcd.ReadOnlyStore[ServiceInfo]).Init()
		if err != nil {
			return errors.WithMessage(err, "Error initializing services")
		}
	}

	fmt.Println("Services initialized")

	return nil
}

func (d *data) syncServices() error {
	d.Lock()
	defer d.Unlock()

	fmt.Println("Syncing services...")

	if d.isManagerNode {
		servicesInfos, err := d.getServicesInfosFromDocker()

		err = d.services.(etcd.WriteOnlyStore[ServiceInfo]).Sync(servicesInfos)
		if err != nil {
			return errors.WithMessage(err, "Error syncing services")
		}
	} else {
		err := d.services.(etcd.ReadOnlyStore[ServiceInfo]).Sync()
		if err != nil {
			return errors.WithMessage(err, "Error syncing services")
		}
	}

	return nil
}

func (d *data) getServicesInfosFromDocker() (servicesInfos map[string]ServiceInfo, err error) {
	rawServices, err := d.dockerClient.ServiceList(context.Background(), types.ServiceListOptions{})
	if err != nil {
		return nil, errors.WithMessage(err, "Error listing docker services")
	}

	servicesInfos = map[string]ServiceInfo{}

	for _, service := range rawServices {
		serviceInfo, ignored, err := d.getServiceInfoFromDocker(service.ID)
		if err != nil {
			log.Printf("Error getting service info for service with ID %s. Skipping... Error: %+v\n", service.ID, err)
			continue
		}
		if ignored {
			continue
		}
		servicesInfos[serviceInfo.ID] = *serviceInfo
	}

	return
}

func (d *data) getServiceInfoFromDocker(serviceID string) (serviceInfo *ServiceInfo, ignored bool, err error) {
	service, _, err := d.dockerClient.ServiceInspectWithRaw(context.Background(), serviceID, types.ServiceInspectOptions{})
	if err != nil {
		return nil, false, errors.WithMessagef(err, "Error inspecting docker service %s", serviceID)
	}

	ipamVIPs := make(map[string]net.IP)

	networks := lo.Map(
		service.Spec.TaskTemplate.Networks,
		func(item swarm.NetworkAttachmentConfig, index int) string { return item.Target })

	serviceInfo = &ServiceInfo{
		ID:           serviceID,
		Name:         service.Spec.Name,
		EndpointMode: string(service.Endpoint.Spec.Mode),
		Networks:     networks,
		IpamVIPs:     ipamVIPs,
	}

	for _, endpoint := range service.Endpoint.VirtualIPs {
		if endpoint.Addr == "" { // This is the case for an attachment to the host network
			continue
		}
		ip, _, err := net.ParseCIDR(endpoint.Addr)
		if err != nil {
			return nil, false, errors.WithMessagef(err, "error parsing IP address %s on network %s for service %s", endpoint.Addr, endpoint.NetworkID, serviceID)
		}
		serviceInfo.IpamVIPs[endpoint.NetworkID] = ip
	}

	if len(serviceInfo.IpamVIPs) == 0 && serviceInfo.EndpointMode == common.ServiceEndpointModeVip {
		return nil, true, nil
	}

	return
}

func (d *data) handleService(serviceID string) error {
	serviceInfo, ignored, err := d.getServiceInfoFromDocker(serviceID)
	if err != nil {
		return errors.WithMessagef(err, "Error inspecting docker service %s", serviceID)
	}

	if ignored {
		return nil
	}

	err = d.services.(etcd.WriteOnlyStore[ServiceInfo]).AddOrUpdateItem(serviceID, *serviceInfo)
	if err != nil {
		return errors.WithMessagef(err, "Error adding or updating service info %s", serviceID)
	}
	return nil
}

func (d *data) handleDeletedService(serviceID string) error {
	return d.services.(etcd.WriteOnlyStore[ServiceInfo]).DeleteItem(serviceID)
}
