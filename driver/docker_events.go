package driver

import (
	"context"
	"fmt"
	dockerAPItypes "github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/events"
	"log"
	"strings"
	"time"
)

func (d *FlannelDriver) handleEvent(event events.Message) error {
	fmt.Printf("Received docker event: %+v\n", event)
	if event.Type == events.NetworkEventType && event.Action == events.ActionCreate {
		return d.handleNetworkCreated(event)
	} else if event.Type == events.NetworkEventType && event.Action == events.ActionConnect {
		return d.handleContainerConnected(event)
	}

	return nil
}

func (d *FlannelDriver) handleNetworkCreated(event events.Message) error {
	time.Sleep(5 * time.Second)
	fmt.Printf("Received docker event: %+v\n", event)
	networkID := event.Actor.ID
	network, err := d.dockerClient.NetworkInspect(context.Background(), networkID, dockerAPItypes.NetworkInspectOptions{})
	if err != nil {
		log.Printf("Error inspecting docker network %s: %+v\n", networkID, err)
		return err
	}
	id, exists := network.IPAM.Options["flannel-id"]
	if !exists {
		log.Printf("Network %s has no 'flannel-id' option, it's misconfigured or not for us.\n", networkID)
		log.Printf("Available options: %+v\n", network.IPAM.Options)
		return nil
	}

	d.networkIdToFlannelNetworkId[networkID] = id
	return nil
}

func (d *FlannelDriver) handleContainerConnected(event events.Message) error {
	networkID := event.Actor.ID
	networkName := event.Actor.Attributes["name"]
	containerID := event.Actor.Attributes["container"]

	flannelNetworkId := d.networkIdToFlannelNetworkId[networkID]
	flannelNetwork := d.networks[flannelNetworkId]
	if flannelNetwork == nil {
		fmt.Printf("Skipping registration of docker container %s for network %s, because we don't know about the network, so it's not one of ours.", containerID, networkID)
		return nil
	}

	container, err := d.dockerClient.ContainerInspect(context.Background(), containerID)
	if err != nil {
		log.Printf("Error inspecting docker container %s: %+v\n", containerID, err)
		return err
	}

	endpoint := container.NetworkSettings.Networks[networkName]
	if endpoint == nil {
		endpoint = container.NetworkSettings.Networks[networkID]
	}

	if endpoint == nil {
		fmt.Printf("Skipping registration of docker container %s for network %s, because the container has no endpoint for the network. This shouldn't happen. Race Condition?", containerID, networkID)
		return nil
	}

	// These may be unset, that's fine. EtcdClient.RegisterContainer handles this case
	serviceId := container.Config.Labels["com.docker.swarm.service.id"]
	serviceName := container.Config.Labels["com.docker.swarm.service.name"]
	containerName := strings.TrimLeft(container.Name, "/")

	_, err = d.etcdClient.RegisterContainer(flannelNetwork, serviceId, serviceName, containerID, containerName, endpoint.IPAddress)
	if err != nil {
		log.Printf("Error ensuring docker container %s is registered in our data for docker network %s: %+v\n", containerID, networkID, err)
		return err
	}

	if serviceId != "" {
		err = d.EnsureLoadBalancerConfigurationForService(flannelNetworkId, flannelNetwork, serviceId)
		if err != nil {
			log.Printf("Error ensuring load balancer for service %s exists for network %s: %+v\n", serviceId, flannelNetworkId, err)
			return err
		}
		err = d.EnsureServiceLoadBalancerBackend(flannelNetwork, serviceId, endpoint.IPAddress)
		if err != nil {
			log.Printf("Error ensuring load balancer backend with IP %s exists for service %s and network %s: %+v\n", endpoint.IPAddress, serviceId, networkID, err)
			return err
		}
	}

	return nil
}
