package docker

import (
	"context"
	"fmt"
	"github.com/docker/docker/api/types/container"
	"github.com/pkg/errors"
	"github.com/samber/lo"
	"github.com/sovarto/FlannelNetworkPlugin/pkg/common"
	"log"
	"net"
	"strings"
	"time"
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

	if !container.State.Running {
		finishedTime, err := time.Parse(time.RFC3339Nano, container.State.FinishedAt)
		if err != nil {
			return nil, fmt.Errorf("container %s is not running. Parsing the finished date %s failed", containerID, container.State.FinishedAt)
		}
		return nil, fmt.Errorf("container %s is not running. It stopped %s ago", containerID, time.Since(finishedTime))
	}

	serviceID := container.Config.Labels["com.docker.swarm.service.id"]
	serviceName := container.Config.Labels["com.docker.swarm.service.name"]
	containerName := strings.TrimLeft(container.Name, "/")
	sandboxKey := container.NetworkSettings.SandboxKey

	ips := make(map[string]net.IP)
	ipamIPs := make(map[string]net.IP)
	endpoints := make(map[string]string)
	dnsNames := make(map[string][]string)

	containerInfo = &ContainerInfo{
		ContainerInfo: common.ContainerInfo{
			ID:          containerID,
			Name:        containerName,
			ServiceID:   serviceID,
			ServiceName: serviceName,
			SandboxKey:  sandboxKey,
			IPs:         ips,
			Endpoints:   endpoints,
			DNSNames:    dnsNames,
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
			log.Printf("Container %s had network %s with invalid IP %s", container.ID, networkID, networkData.IPAddress)
			continue
		}
		ips[networkID] = ip
		if networkData.IPAMConfig != nil && networkData.IPAMConfig.IPv4Address != "" {
			ipamIP := net.ParseIP(networkData.IPAMConfig.IPv4Address)
			ipamIPs[networkID] = ipamIP
		}
		dnsNames[networkID] = networkData.DNSNames
		endpoints[networkID] = networkData.EndpointID
	}

	return
}

func (d *data) handleContainer(containerID string) error {
	containerInfo, err := d.getContainerInfoFromDocker(containerID)
	if err != nil {
		return err
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

func (d *data) handleDisconnectedContainer(networkID, containerID string) error {
	shardKey, container, exists := d.containers.GetItem(containerID)
	if !exists {
		return fmt.Errorf("container %s does not exist", containerID)
	}
	if shardKey != d.containers.GetLocalShardKey() {
		return fmt.Errorf("container %s is not from the local node. This is a bug", containerID)
	}
	fmt.Printf("Disconnected container %s. Data before: %v\n", containerID, container)
	container.Endpoints = lo.PickBy(container.Endpoints, func(key string, value string) bool {
		return key != networkID
	})
	container.IPs = lo.PickBy(container.IPs, func(key string, value net.IP) bool {
		return key != networkID
	})
	container.IpamIPs = lo.PickBy(container.IpamIPs, func(key string, value net.IP) bool {
		return key != networkID
	})
	container.DNSNames = lo.PickBy(container.DNSNames, func(key string, value []string) bool {
		return key != networkID
	})
	fmt.Printf("Disconnected container %s. Data after: %v\n", containerID, container)
	_, containerFromStore, _ := d.containers.GetItem(containerID)
	fmt.Printf("Disconnected container %s. Data from store after: %v\n", containerID, containerFromStore)

	if err := d.containers.AddOrUpdateItem(containerID, container); err != nil {
		return errors.WithMessagef(err, "Error updating container info %s after network disconnection", containerID)
	}
	return nil
}
