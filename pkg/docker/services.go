package docker

import (
	"fmt"
	"github.com/pkg/errors"
	"github.com/sovarto/FlannelNetworkPlugin/pkg/common"
	"github.com/sovarto/FlannelNetworkPlugin/pkg/etcd"
	"github.com/tiendc/go-deepcopy"
	clientv3 "go.etcd.io/etcd/client/v3"
	"log"
	"net"
	"strings"
)

func (d *data) servicesKey() string {
	return d.etcdClient.GetKey("services")
}

func (d *data) serviceKey(id string) string {
	return fmt.Sprintf("%s/%s", d.servicesKey(), id)
}

func (d *data) serviceVipsKey(serviceID string) string {
	return fmt.Sprintf("%s/vips", d.serviceKey(serviceID))
}

func (d *data) serviceVipKey(serviceID, networkID string) string {
	return fmt.Sprintf("%s/%s", d.serviceVipsKey(serviceID), networkID)
}

func (d *data) serviceContainersKey(serviceID string) string {
	return fmt.Sprintf("%s/containers", d.serviceKey(serviceID))
}

func (d *data) serviceContainerKey(serviceID, nodeName, containerID string) string {
	return fmt.Sprintf("%s/%s/%s", d.serviceContainersKey(serviceID), nodeName, containerID)
}

func (d *data) serviceContainerNetworksKey(serviceID, nodeName, containerID string) string {
	return fmt.Sprintf("%s/networks", d.serviceContainerKey(serviceID, nodeName, containerID))
}

func (d *data) serviceContainerNetworkKey(serviceID, nodeName, containerID, networkID string) string {
	return fmt.Sprintf("%s/%s", d.serviceContainerNetworksKey(serviceID, nodeName, containerID), networkID)
}

func (d *data) syncServicesContainers(containerInfos map[string]common.ContainerInfo, containerIDtoServiceID map[string]string, containerIDtoServiceName map[string]string) error {
	loadedServiceContainers, err := d.loadServicesInfo()
	if err != nil {
		return errors.Wrap(err, "failed to load services info")
	}

	for containerID, serviceID := range containerIDtoServiceID {
		serviceName := containerIDtoServiceName[containerID]
		err := d.addOrUpdateServiceContainer(serviceID, serviceName, containerInfos[containerID])
		if err != nil {
			log.Printf("Error updating service container for container %s and service %s. Skipping...\n", containerID, serviceID)
		}
	}

	return nil
}

func (d *data) addOrUpdateServiceContainer(serviceID, serviceName string, containerInfo common.ContainerInfo) error {
	serviceInfo, serviceExists := d.services[serviceID]
	var previousServiceInfo *common.ServiceInfo = nil
	if !serviceExists {
		serviceInfo = common.ServiceInfo{
			ID:         serviceID,
			Name:       serviceName,
			Containers: make(map[string]common.ContainerInfo),
			VIPs:       make(map[string]net.IP),
		}

		d.services[serviceID] = serviceInfo
	} else {
		previousServiceInfo = &common.ServiceInfo{}
		err := deepcopy.Copy(previousServiceInfo, serviceInfo)
		if err != nil {
			return errors.WithMessagef(err, "Error deepcopying service info for docker service %s", serviceID)
		}
	}

	serviceInfo.Containers[containerInfo.ID] = containerInfo

	err := d.storeServiceContainer(serviceID, serviceName, containerInfo)
	if err != nil {
		return errors.WithMessagef(err, "Error storing docker container %s for service %s", containerInfo.ID, serviceID)
	}
	d.invokeServiceCallback(previousServiceInfo, serviceInfo)

	return nil
}

func (d *data) storeServiceContainer(serviceID, serviceName string, containerInfo common.ContainerInfo) error {
	_, err := etcd.WithConnection(d.etcdClient, func(connection *etcd.Connection) (struct{}, error) {
		serviceKey := d.serviceKey(serviceID)
		_, err := connection.PutIfNewOrChanged(serviceKey, serviceName)

		if err != nil {
			return struct{}{}, errors.WithMessagef(err, "Failed to store service info for service %s: %+v\n", serviceID, err)
		}

		serviceContainerKey := d.serviceContainerKey(serviceID, containerInfo.ID)
		_, err = connection.PutIfNewOrChanged(serviceContainerKey, containerInfo.Name)

		if err != nil {
			return struct{}{}, errors.WithMessagef(err, "Failed to store container name for container %s and service %s: %+v\n", containerInfo.ID, serviceID, err)
		}

		for networkID, ip := range containerInfo.IPs {
			serviceContainerNetworkKey := d.serviceContainerNetworkKey(serviceID, containerInfo.ID, networkID)
			_, err = connection.PutIfNewOrChanged(serviceContainerNetworkKey, ip.String())

			if err != nil {
				return struct{}{}, errors.WithMessagef(err, "Failed to store service container info for service %s: %+v\n", serviceID, err)
			}
		}

		return struct{}{}, nil
	})

	return err
}

func (d *data) loadServicesInfo() (map[string]common.ServiceInfo, error) {
	return etcd.WithConnection(d.etcdClient, func(connection *etcd.Connection) (map[string]common.ServiceInfo, error) {
		servicesKey := d.servicesKey()

		resp, err := connection.Client.Get(connection.Ctx, servicesKey, clientv3.WithPrefix(), clientv3.WithSort(clientv3.SortByKey, clientv3.SortAscend))
		if err != nil {
			return nil, errors.WithMessagef(err, "error reading service info: %+v", err)
		}

		result := map[string]common.ServiceInfo{}

		for _, kv := range resp.Kvs {
			key := strings.TrimLeft(strings.TrimPrefix(string(kv.Key), servicesKey), "/")
			parts := strings.Split(key, "/")
			if len(parts) == 1 {
				addOrUpdateServiceInfo(result, common.ServiceInfo{
					ID:         key,
					Name:       string(kv.Value),
					Containers: make(map[string]map[string]common.ContainerInfo),
					VIPs:       make(map[string]net.IP),
				}, func(existing common.ServiceInfo) {
					existing.Name = string(kv.Value)
				})
			} else if len(parts) == 3 && parts[1] == "containers" {
				serviceID := parts[0]
				nodeName := parts[2]
				containerID := parts[3]

				addOrUpdateContainerInfo(result, serviceID, common.ContainerInfo{
					ID:   containerID,
					Name: string(kv.Value),
					IPs:  make(map[string]net.IP),
				}, func(existing common.ContainerInfo) { existing.Name = string(kv.Value) })
			} else if len(parts) == 3 && parts[1] == "vips" {
				serviceID := parts[0]
				networkID := parts[2]

				ip := net.ParseIP(string(kv.Value))

				addOrUpdateServiceInfo(result, common.ServiceInfo{
					ID:         serviceID,
					Name:       string(kv.Value),
					Containers: make(map[string]map[string]common.ContainerInfo),
					VIPs:       map[string]net.IP{networkID: ip},
				}, func(existing common.ServiceInfo) {
					existing.VIPs[networkID] = ip
				})
			} else if len(parts) == 5 {
				serviceID := parts[0]
				containerID := parts[2]
				networkID := parts[4]

				ip := net.ParseIP(string(kv.Value))
				if ip == nil {
					return nil, fmt.Errorf("expect service info entry for %s to have an IP address, but got %s", key, string(kv.Value))
				}

				addOrUpdateContainerInfo(result, serviceID, common.ContainerInfo{
					ID:  containerID,
					IPs: map[string]net.IP{networkID: ip},
				}, func(existing common.ContainerInfo) { existing.IPs[networkID] = ip })
			} else {
				log.Printf("Read unknown key %s", string(kv.Key))
			}
		}

		return result, nil
	})
}

func (d *data) invokeServiceCallback(previous *common.ServiceInfo, current common.ServiceInfo) {
	invokeItemCallback(previous, current, d.callbacks.ServiceAdded, d.callbacks.ServiceChanged)
}

func addOrUpdateServiceInfo(store map[string]common.ServiceInfo, valueToAdd common.ServiceInfo, update func(existing common.ServiceInfo)) {
	existing, exists := store[valueToAdd.ID]
	if exists {
		if update != nil {
			update(existing)
		}
	} else {
		existing = valueToAdd
	}
	store[valueToAdd.ID] = existing
}

func addOrUpdateContainerInfo(store map[string]common.ServiceInfo, serviceID string, nodeName string, valueToAdd common.ContainerInfo, update func(existing common.ContainerInfo)) {
	addOrUpdateServiceInfo(store, common.ServiceInfo{
		ID:         serviceID,
		Containers: make(map[string]map[string]common.ContainerInfo),
		VIPs:       make(map[string]net.IP),
	}, nil)
	nodeContainers, exist := store[serviceID].Containers[nodeName]
	if !exist {
		nodeContainers = make(map[string]common.ContainerInfo)
		store[serviceID].Containers[nodeName] = nodeContainers
	}
	existing, exists := nodeContainers[valueToAdd.ID]
	if exists {
		if update != nil {
			update(existing)
		}
	} else {
		existing = valueToAdd
	}
	nodeContainers[valueToAdd.ID] = existing
}
