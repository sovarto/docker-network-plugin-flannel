package docker

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/docker/docker/api/types/container"
	"github.com/pkg/errors"
	"github.com/samber/lo"
	"github.com/sovarto/FlannelNetworkPlugin/pkg/common"
	"github.com/sovarto/FlannelNetworkPlugin/pkg/etcd"
	clientv3 "go.etcd.io/etcd/client/v3"
	"log"
	"net"
	"strings"
)

// syncContainers
// The truth here is not etcd but Docker:
// - The internal state will be populated from Docker
// - New, changed and deleted items wrt the internal state will be reported via callbacks
// - etcd will be brought in sync with the internal state
func (d *data) syncContainers() error {
	d.Lock()
	defer d.Unlock()

	fmt.Println("Syncing containers...")

	rawContainers, err := d.dockerClient.ContainerList(context.Background(), container.ListOptions{})
	if err != nil {
		return errors.WithMessage(err, "Error listing docker containers")
	}

	containerToServiceID := map[string]string{}
	containerToServiceName := map[string]string{}
	containers := map[string]common.ContainerInfo{}

	for _, container := range rawContainers {
		containerInfo, serviceID, serviceName, err := d.getContainerInfoFromDocker(container.ID)
		if err != nil {
			log.Printf("Error getting container info for container with ID %s. Skipping...\n", container.ID)
			continue
		}
		containers[containerInfo.ID] = *containerInfo
		containerToServiceID[container.ID] = serviceID
		containerToServiceName[container.ID] = serviceName
	}

	toBeDeletedFromInternalState, _ := lo.Difference(lo.Keys(d.containers), lo.Keys(containers))

	for containerID, containerInfo := range containers {
		previousContainerInfo := getPtrFromMap(d.containers, containerID)
		d.containers[containerID] = containerInfo

		err = d.storeContainerInfo(containerInfo)
		if err != nil {
			log.Printf("Error storing container info for container with ID %s. Skipping...\n", containerID)
			continue
		}

		d.invokeContainerCallback(previousContainerInfo, containerInfo)
	}

	for _, containerID := range toBeDeletedFromInternalState {
		containerInfo := d.containers[containerID]
		delete(d.containers, containerID)
		err = d.deleteKey(d.localContainerKey(containerID), true)
		if err != nil {
			log.Printf("Error deleting container info for container with ID %s. Skipping...\n", containerID)
			continue
		}

		if d.callbacks.ContainerRemoved != nil {
			d.callbacks.ContainerRemoved(containerInfo)
		}
	}

	loadedContainers, err := d.loadLocalContainersInfo()
	if err != nil {
		return errors.WithMessage(err, "Error loading docker containers info from etcd")
	}

	toBeDeletedFromEtcd, _ := lo.Difference(lo.Keys(loadedContainers), lo.Keys(containers))

	for _, containerID := range toBeDeletedFromEtcd {
		err = d.deleteKey(d.localContainerKey(containerID), true)
		if err != nil {
			log.Printf("Error deleting container info for container with ID %s. Skipping...\n", containerID)
			continue
		}
	}

	err = d.syncServicesContainers(d.containers, containerToServiceID, containerToServiceName)
	if err != nil {
		return errors.WithMessage(err, "Error syncing services containers")
	}

	return nil
}

func (d *data) getContainersInfosFromDocker() {

}

func (d *data) getContainerInfoFromDocker(containerID string) (containerInfo *common.ContainerInfo, serviceID string, serviceName string, err error) {
	container, err := d.dockerClient.ContainerInspect(context.Background(), containerID)
	if err != nil {
		return nil, "", "", errors.WithMessagef(err, "Error inspecting docker container %s", containerID)
	}

	serviceID = container.Config.Labels["com.docker.swarm.service.id"]
	serviceName = container.Config.Labels["com.docker.swarm.service.name"]
	containerName := strings.TrimLeft(container.Name, "/")

	ips := make(map[string]net.IP)
	ipamIPs := make(map[string]net.IP)

	containerInfo = &common.ContainerInfo{
		ID:      containerID,
		Name:    containerName,
		IPs:     ips,
		IpamIPs: ipamIPs,
	}

	for networkName, networkData := range container.NetworkSettings.Networks {
		if networkName == "host" {
			continue
		}
		networkID := networkData.NetworkID
		if networkData.IPAddress == "" {
			log.Printf("Found network %s without IP", networkID)
		}
		ip := net.ParseIP(networkData.IPAddress)
		if ip == nil {
			log.Printf("Found network %s with invalid IP %s", networkID, networkData.IPAddress)
		}
		ips[networkID] = ip
		if networkData.IPAMConfig != nil && networkData.IPAMConfig.IPv4Address != "" {
			ipamIP := net.ParseIP(networkData.IPAMConfig.IPv4Address)
			ipamIPs[networkID] = ipamIP
		}
	}

	return containerInfo, serviceID, serviceName, nil
}

func (d *data) localContainersKey() string {
	return d.etcdClient.GetKey("containers", d.hostname)
}

func (d *data) localContainerKey(id string) string {
	return fmt.Sprintf("%s/%s", d.localContainersKey(), id)
}

func (d *data) storeContainerInfo(containerInfo common.ContainerInfo) error {
	_, err := etcd.WithConnection(d.etcdClient, func(connection *etcd.Connection) (struct{}, error) {
		containerKey := d.localContainerKey(containerInfo.ID)

		containerInfoBytes, err := json.Marshal(containerInfo)
		if err != nil {
			return struct{}{}, errors.WithMessagef(err, "Failed to serialize container info %+v", containerInfo)
		}
		containerInfoString := string(containerInfoBytes)
		_, err = connection.PutIfNewOrChanged(containerKey, containerInfoString)

		if err != nil {
			return struct{}{}, errors.WithMessagef(err, "Failed to store container info %+v", containerInfo)
		}

		return struct{}{}, nil
	})

	return err
}

func (d *data) loadLocalContainersInfo() (map[string]common.ContainerInfo, error) {
	return etcd.WithConnection(d.etcdClient, func(connection *etcd.Connection) (map[string]common.ContainerInfo, error) {
		containersKey := d.localContainersKey()

		resp, err := connection.Client.Get(connection.Ctx, containersKey, clientv3.WithPrefix())
		if err != nil {
			return nil, errors.WithMessagef(err, "error reading container info for node %s: %+v", d.hostname, err)
		}

		result := map[string]common.ContainerInfo{}

		for _, kv := range resp.Kvs {
			var containerInfo common.ContainerInfo
			err = json.Unmarshal(kv.Value, &containerInfo)
			if err != nil {
				return nil, errors.WithMessagef(err, "error parsing container info for node %s: err: %+v, value: %+v", d.hostname, err, string(kv.Value))
			}

			result[containerInfo.ID] = containerInfo
		}

		return result, nil
	})
}

func (d *data) invokeContainerCallback(previous *common.ContainerInfo, current common.ContainerInfo) {
	invokeItemCallback(previous, current, d.callbacks.ContainerAdded, d.callbacks.ContainerChanged)
}
