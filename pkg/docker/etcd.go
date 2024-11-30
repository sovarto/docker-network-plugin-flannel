package docker

import (
	"encoding/json"
	"fmt"
	"github.com/pkg/errors"
	"github.com/sovarto/FlannelNetworkPlugin/pkg/common"
	"github.com/sovarto/FlannelNetworkPlugin/pkg/etcd"
	clientv3 "go.etcd.io/etcd/client/v3"
	"log"
	"net"
	"strings"
)

func networksKey(client etcd.Client) string {
	return client.GetKey("networks")
}

func networkKey(client etcd.Client, id string) string {
	return fmt.Sprintf("%s/%s", networksKey(client), id)
}

func containersKey(client etcd.Client, nodeName string) string {
	return client.GetKey("containers", nodeName)
}

func containerKey(client etcd.Client, nodeName, id string) string {
	return fmt.Sprintf("%s/%s", containersKey(client, nodeName), id)
}

func servicesKey(client etcd.Client) string {
	return client.GetKey("services")
}

func serviceKey(client etcd.Client, id string) string {
	return fmt.Sprintf("%s/%s", servicesKey(client), id)
}

func serviceVipsKey(client etcd.Client, serviceID string) string {
	return fmt.Sprintf("%s/vips", serviceKey(client, serviceID))
}

func serviceVipKey(client etcd.Client, serviceID, networkID string) string {
	return fmt.Sprintf("%s/%s", serviceVipsKey(client, serviceID), networkID)
}

func serviceContainersKey(client etcd.Client, serviceID string) string {
	return fmt.Sprintf("%s/containers", serviceKey(client, serviceID))
}

func serviceContainerKey(client etcd.Client, serviceID, containerID string) string {
	return fmt.Sprintf("%s/%s", serviceContainersKey(client, serviceID), containerID)
}

func serviceContainerNetworksKey(client etcd.Client, serviceID, containerID string) string {
	return fmt.Sprintf("%s/networks", serviceContainerKey(client, serviceID, containerID))
}

func serviceContainerNetworkKey(client etcd.Client, serviceID, containerID, networkID string) string {
	return fmt.Sprintf("%s/%s", serviceContainerNetworksKey(client, serviceID, containerID), networkID)
}

func storeNetworkInfo(client etcd.Client, dockerNetworkID, flannelNetworkID string) error {
	_, err := etcd.WithConnection(client, func(connection *etcd.Connection) (struct{}, error) {
		key := networkKey(client, dockerNetworkID)

		_, err := connection.PutIfNewOrChanged(key, flannelNetworkID)
		if err != nil {
			return struct{}{}, err
		}

		return struct{}{}, nil
	})

	return err
}

func deleteNetworkInfo(client etcd.Client, dockerNetworkID string) error {
	return deleteKey(client, networkKey(client, dockerNetworkID), false)
}

func loadNetworkInfo(client etcd.Client) (map[string]string, error) {
	return etcd.WithConnection(client, func(connection *etcd.Connection) (map[string]string, error) {
		key := networksKey(client)
		resp, err := connection.Client.Get(connection.Ctx, key, clientv3.WithPrefix())
		if err != nil {
			return nil, err
		}

		result := map[string]string{}

		for _, kv := range resp.Kvs {
			dockerNetworkID := strings.TrimLeft(strings.TrimPrefix(string(kv.Key), key), "/")
			flannelNetworkID := string(kv.Value)
			result[dockerNetworkID] = flannelNetworkID
		}

		return result, nil
	})
}

func storeContainerAndServiceInfo(client etcd.Client, hostname string, containerInfo common.ContainerInfo, serviceID, serviceName string) error {
	_, err := etcd.WithConnection(client, func(connection *etcd.Connection) (struct{}, error) {
		containerKey := containerKey(client, hostname, containerInfo.ID)

		containerInfoBytes, err := json.Marshal(containerInfo)
		if err != nil {
			return struct{}{}, errors.WithMessagef(err, "Failed to serialize container info %+v", containerInfo)
		}
		containerInfoString := string(containerInfoBytes)
		_, err = connection.PutIfNewOrChanged(containerKey, containerInfoString)

		if err != nil {
			return struct{}{}, errors.WithMessagef(err, "Failed to store container info %+v", containerInfo)
		}

		if serviceID != "" && serviceName != "" {
			serviceKey := serviceKey(client, serviceID)
			_, err = connection.PutIfNewOrChanged(serviceKey, serviceName)

			if err != nil {
				return struct{}{}, errors.WithMessagef(err, "Failed to store service info for service %s: %+v\n", serviceID, err)
			}

			serviceContainerKey := serviceContainerKey(client, serviceID, containerInfo.ID)
			_, err = connection.PutIfNewOrChanged(serviceContainerKey, containerInfo.Name)

			if err != nil {
				return struct{}{}, errors.WithMessagef(err, "Failed to store container name for container %s and service %s: %+v\n", containerInfo.ID, serviceID, err)
			}

			for networkID, ip := range containerInfo.IPs {
				serviceContainerNetworkKey := serviceContainerNetworkKey(client, serviceID, containerInfo.ID, networkID)
				_, err = connection.PutIfNewOrChanged(serviceContainerNetworkKey, ip.String())

				if err != nil {
					return struct{}{}, errors.WithMessagef(err, "Failed to store service container info for service %s: %+v\n", serviceID, err)
				}
			}
		}

		return struct{}{}, nil
	})

	return err
}

func storeServiceVIPs(client etcd.Client, serviceID string, vips map[string]net.IP) error {
	_, err := etcd.WithConnection(client, func(connection *etcd.Connection) (struct{}, error) {
		for networkID, vip := range vips {
			key := serviceVipKey(client, serviceID, networkID)
			_, err := connection.PutIfNewOrChanged(key, vip.String())

			if err != nil {
				return struct{}{}, errors.WithMessagef(err, "Failed to store vip %s for service %s: %+v\n", vip.String(), serviceID, err)
			}
		}

		return struct{}{}, nil
	})

	return err
}

func loadContainersInfo(client etcd.Client, nodeName string) (map[string]common.ContainerInfo, error) {
	return etcd.WithConnection(client, func(connection *etcd.Connection) (map[string]common.ContainerInfo, error) {
		containersKey := containersKey(client, nodeName)

		resp, err := connection.Client.Get(connection.Ctx, containersKey, clientv3.WithPrefix())
		if err != nil {
			return nil, errors.WithMessagef(err, "error reading container info for node %s: %+v", nodeName, err)
		}

		result := map[string]common.ContainerInfo{}

		for _, kv := range resp.Kvs {
			var containerInfo common.ContainerInfo
			err = json.Unmarshal(kv.Value, &containerInfo)
			if err != nil {
				return nil, errors.WithMessagef(err, "error parsing container info for node %s: err: %+v, value: %+v", nodeName, err, string(kv.Value))
			}

			result[containerInfo.ID] = containerInfo
		}

		return result, nil
	})
}

func loadServicesInfo(client etcd.Client) (map[string]common.ServiceInfo, error) {
	return etcd.WithConnection(client, func(connection *etcd.Connection) (map[string]common.ServiceInfo, error) {
		servicesKey := servicesKey(client)

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
					Containers: make(map[string]common.ContainerInfo),
					VIPs:       make(map[string]net.IP),
				}, func(existing common.ServiceInfo) {
					existing.Name = string(kv.Value)
				})
			} else if len(parts) == 3 && parts[1] == "containers" {
				serviceID := parts[0]
				containerID := parts[2]

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
					Containers: make(map[string]common.ContainerInfo),
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

func deleteServiceInfo(client etcd.Client, serviceID string) error {
	return deleteKey(client, serviceKey(client, serviceID), true)
}

func deleteContainerInfo(client etcd.Client, nodeName, containerID string) error {
	return deleteKey(client, containerKey(client, nodeName, containerID), true)
}

func deleteContainerFromServiceInfo(client etcd.Client, serviceID, containerID string) error {
	return deleteKey(client, serviceContainerKey(client, serviceID, containerID), true)
}

func deleteKey(client etcd.Client, key string, withPrefix bool) error {
	_, err := etcd.WithConnection(client, func(connection *etcd.Connection) (struct{}, error) {
		options := []clientv3.OpOption{}
		if withPrefix {
			options = append(options, clientv3.WithPrefix())
		}
		_, err := connection.Client.Delete(connection.Ctx, key, options...)
		if err != nil {
			return struct{}{}, err
		}

		return struct{}{}, nil
	})

	return err
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

func addOrUpdateContainerInfo(store map[string]common.ServiceInfo, serviceID string, valueToAdd common.ContainerInfo, update func(existing common.ContainerInfo)) {
	addOrUpdateServiceInfo(store, common.ServiceInfo{
		ID:         serviceID,
		Containers: make(map[string]common.ContainerInfo),
		VIPs:       make(map[string]net.IP),
	}, nil)
	existing, exists := store[serviceID].Containers[valueToAdd.ID]
	if exists {
		if update != nil {
			update(existing)
		}
	} else {
		existing = valueToAdd
	}
	store[serviceID].Containers[valueToAdd.ID] = existing
}
