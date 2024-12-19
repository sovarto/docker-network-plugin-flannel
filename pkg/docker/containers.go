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

func (d *data) initContainers() error {
	d.Lock()
	defer d.Unlock()

	fmt.Println("Initializing containers...")
	containerInfos, err := d.getContainersInfosFromDocker()

	err = d.containers.Init(containerInfos)
	if err != nil {
		return errors.WithMessage(err, "Error initializing containers")
	}

	fmt.Println("Containers initialized")
	return nil
}

func (d *data) syncContainers() error {
	d.Lock()
	defer d.Unlock()

	fmt.Println("Syncing containers...")

	containerInfos, err := d.getContainersInfosFromDocker()

	err = d.containers.Sync(containerInfos)
	if err != nil {
		return errors.WithMessage(err, "Error syncing containers")
	}

	return nil
}

func (d *data) getContainersInfosFromDocker() (containerInfos map[string]ContainerInfo, err error) {
	rawContainers, err := d.dockerClient.ContainerList(context.Background(), container.ListOptions{})
	if err != nil {
		return nil, errors.WithMessage(err, "Error listing docker containers")
	}

	containerInfos = map[string]ContainerInfo{}

	for _, container := range rawContainers {
		containerInfo, err := d.getContainerInfoFromDocker(container.ID)
		if err != nil {
			log.Printf("Error getting container info for container with ID %s. Skipping...\n", container.ID)
			continue
		}
		if len(containerInfo.IPs) == 0 {
			continue
		}
		containerInfos[containerInfo.ID] = *containerInfo
	}

	return
}

func (d *data) getContainerInfoFromDocker(containerID string) (containerInfo *ContainerInfo, err error) {
	container, err := d.dockerClient.ContainerInspect(context.Background(), containerID)
	if err != nil {
		return nil, errors.WithMessagef(err, "Error inspecting docker container %s", containerID)
	}

	serviceID := container.Config.Labels["com.docker.swarm.service.id"]
	serviceName := container.Config.Labels["com.docker.swarm.service.name"]
	containerName := strings.TrimLeft(container.Name, "/")
	sandboxKey := container.NetworkSettings.SandboxKey

	ips := make(map[string]net.IP)
	ipamIPs := make(map[string]net.IP)
	endpoints := make(map[string]string)
	aliases := make(map[string][]string)

	containerInfo = &ContainerInfo{
		ContainerInfo: common.ContainerInfo{
			ID:          containerID,
			Name:        containerName,
			ServiceID:   serviceID,
			ServiceName: serviceName,
			SandboxKey:  sandboxKey,
			IPs:         ips,
			Endpoints:   endpoints,
			Aliases:     aliases,
		},
		IpamIPs: ipamIPs,
	}

	for networkName, networkData := range container.NetworkSettings.Networks {
		if networkName == "host" {
			continue
		}
		networkID := networkData.NetworkID
		if networkData.IPAddress == "" {
			log.Printf("Container %s had network %s without IP", container.ID, networkID)
			continue
		}
		ip := net.ParseIP(networkData.IPAddress)
		if ip == nil {
			log.Printf("Found network %s with invalid IP %s", networkID, networkData.IPAddress)
			continue
		}
		ips[networkID] = ip
		if networkData.IPAMConfig != nil && networkData.IPAMConfig.IPv4Address != "" {
			ipamIP := net.ParseIP(networkData.IPAMConfig.IPv4Address)
			ipamIPs[networkID] = ipamIP
		}
		aliases[networkID] = networkData.Aliases
		endpoints[networkID] = networkData.EndpointID
	}

	return
}

func (d *data) handleContainer(containerID string) error {
	containerInfo, err := d.getContainerInfoFromDocker(containerID)
	if err != nil {
		return errors.WithMessagef(err, "Error inspecting docker container %s", containerID)
	}

	if len(containerInfo.IPs) == 0 {
		return nil
	}
	err = d.containers.AddOrUpdateItem(containerID, *containerInfo)
	if err != nil {
		return errors.WithMessagef(err, "Error adding or updating container info %s", containerID)
	}
	return nil
}

func (d *data) handleDeletedContainer(containerID string) error {
	return d.containers.DeleteItem(containerID)
}
