package docker

import (
	"context"
	"fmt"
	"github.com/docker/docker/api/types/container"
	"github.com/pkg/errors"
	"github.com/sovarto/FlannelNetworkPlugin/pkg/common"
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

	containerInfos, containerIDtoServiceID, containerIDtoServiceName, err := d.getContainersInfosFromDocker()

	err = d.containers.Sync(containerInfos)
	if err != nil {
		return errors.WithMessage(err, "Error syncing containers")
	}

	err = d.syncServicesContainers(containerInfos, containerIDtoServiceID, containerIDtoServiceName)
	if err != nil {
		return errors.WithMessage(err, "Error syncing services containers")
	}

	return nil
}

func (d *data) getContainersInfosFromDocker() (containerInfos map[string]common.ContainerInfo, containerIDtoServiceID map[string]string, containerIDtoServiceName map[string]string, err error) {
	rawContainers, err := d.dockerClient.ContainerList(context.Background(), container.ListOptions{})
	if err != nil {
		return nil, nil, nil, errors.WithMessage(err, "Error listing docker containers")
	}

	containerIDtoServiceID = map[string]string{}
	containerIDtoServiceName = map[string]string{}
	containerInfos = map[string]common.ContainerInfo{}

	for _, container := range rawContainers {
		containerInfo, serviceID, serviceName, err := d.getContainerInfoFromDocker(container.ID)
		if err != nil {
			log.Printf("Error getting container info for container with ID %s. Skipping...\n", container.ID)
			continue
		}
		containerInfos[containerInfo.ID] = *containerInfo
		containerIDtoServiceID[container.ID] = serviceID
		containerIDtoServiceName[container.ID] = serviceName
	}

	return
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

	return
}
