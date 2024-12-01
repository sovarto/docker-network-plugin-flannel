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

func (d *data) networksKey() string {
	return d.etcdClient.GetKey("networks")
}

func (d *data) networkKey(id string) string {
	return fmt.Sprintf("%s/%s", d.networksKey(), id)
}

func (d *data) storeNetworkInfo(client etcd.Client, dockerNetworkID, flannelNetworkID string) error {
	_, err := etcd.WithConnection(client, func(connection *etcd.Connection) (struct{}, error) {
		key := d.networkKey(dockerNetworkID)

		_, err := connection.PutIfNewOrChanged(key, flannelNetworkID)
		if err != nil {
			return struct{}{}, err
		}

		return struct{}{}, nil
	})

	return err
}

func (d *data) deleteNetworkInfo(client etcd.Client, dockerNetworkID string) error {
	return deleteKey(client, networkKey(client, dockerNetworkID), false)
}

func (d *data) loadNetworkInfo(client etcd.Client) (map[string]string, error) {
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

func (d *data) storeContainerAndServiceInfo(client etcd.Client, hostname string, containerInfo common.ContainerInfo, serviceID, serviceName string) error {
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

func (d *data) storeServiceVIPs(client etcd.Client, serviceID string, vips map[string]net.IP) error {
	_, err := etcd.WithConnection(client, func(connection *etcd.Connection) (struct{}, error) {
		for networkID, vip := range vips {
			key := d.serviceVipKey(serviceID, networkID)
			_, err := connection.PutIfNewOrChanged(key, vip.String())

			if err != nil {
				return struct{}{}, errors.WithMessagef(err, "Failed to store vip %s for service %s: %+v\n", vip.String(), serviceID, err)
			}
		}

		return struct{}{}, nil
	})

	return err
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

func (d *data) deleteKey(key string, withPrefix bool) error {
	_, err := etcd.WithConnection(d.etcdClient, func(connection *etcd.Connection) (struct{}, error) {
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
