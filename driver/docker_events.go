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
	} else if event.Type == events.ServiceEventType && event.Action == events.ActionCreate {
		return d.handleServiceCreated(event)
	} else if event.Type == events.NetworkEventType && event.Action = events.ActionConnect {
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

func (d *FlannelDriver) handleServiceCreated(event events.Message) error {
	serviceID := event.Actor.ID
	service, _, err := d.dockerClient.ServiceInspectWithRaw(context.Background(), serviceID, dockerAPItypes.ServiceInspectOptions{InsertDefaults: false})
	if err != nil {
		log.Printf("Error inspecting docker service %s: %+v\n", serviceID, err)
		return err
	}

	for _, network := range service.Spec.TaskTemplate.Networks {
		flannelNetwork := d.networks[d.networkIdToFlannelNetworkId[network.Target]]
		if flannelNetwork == nil {
			fmt.Printf("Skipping registration of docker service %s for network %s, because we don't know about it, so it's not one of ours.", serviceID, network.Target)
			continue
		}
		_, err := d.etcdClient.EnsureServiceRegistered(flannelNetwork, serviceID, event.Actor.Attributes["name"])
		if err != nil {
			log.Printf("Error ensuring docker service %s is registered in our data for docker network %s. This is bad, but skipping, so we can try to register for the other networks the service belongs to...: %+v\n", serviceID, network.Target, err)
		}
	}

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

	_, err = d.etcdClient.EnsureContainerRegistered(flannelNetwork, containerID, container.Name, endpoint.IPAddress)
	if err != nil {
		log.Printf("Error ensuring docker service %s is registered in our data for docker network %s. This is bad, but skipping, so we can try to register for the other networks the service belongs to...: %+v\n", serviceID, network.Target, err)
	}

	return nil
}
