package driver

import (
	"context"
	"fmt"
	dockerAPItypes "github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/events"
	"log"
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
	networkID := event.Actor.ID
	network, err := d.dockerClient.NetworkInspect(context.Background(), networkID, dockerAPItypes.NetworkInspectOptions{})
	if err != nil {
		log.Printf("Error inspecting docker network %s: %+v\n", networkID, err)
		return err
	}
	id, exists := network.IPAM.Options["id"]
	if !exists {
		log.Printf("Network %s has no 'id' option, it's misconfigured or not for us\n", networkID)
		return nil
	}

	d.networkIdToFlannelNetworkId[networkID] = id
	return nil
}

func (d *FlannelDriver) handleContainerConnected(event events.Message) error {
	networkID := event.Actor.ID
	networkName := event.Actor.Attributes["name"]
	containerID := event.Actor.Attributes["container"]

	flannelNetwork := d.networks[d.networkIdToFlannelNetworkId[networkID]]
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

	_, err = d.etcdClient.RegisterContainer(flannelNetwork, serviceId, serviceName, containerID, container.Name, endpoint.IPAddress)
	if err != nil {
		log.Printf("Error ensuring docker service %s is registered in our data for docker network %s. This is bad, but skipping, so we can try to register for the other networks the service belongs to...: %+v\n", serviceID, network.Target, err)
	}

	return nil
}
