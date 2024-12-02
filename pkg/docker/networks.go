package docker

import (
	"context"
	"fmt"
	"github.com/docker/docker/api/types/network"
	"github.com/pkg/errors"
	"github.com/sovarto/FlannelNetworkPlugin/pkg/etcd"
	"log"
)

func (d *data) initNetworks() error {
	d.Lock()
	defer d.Unlock()

	if d.isManagerNode {
		networksInfos, err := d.getNetworksInfosFromDocker()

		err = d.networks.(etcd.WriteOnlyStore[NetworkInfo]).Init(networksInfos)
		if err != nil {
			return errors.WithMessage(err, "Error initializing networks")
		}
	} else {
		err := d.networks.(etcd.ReadOnlyStore[NetworkInfo]).Init()
		if err != nil {
			return errors.WithMessage(err, "Error initializing networks")
		}
	}

	return nil
}

func (d *data) syncNetworks() error {
	d.Lock()
	defer d.Unlock()

	fmt.Println("Syncing networks...")

	if d.isManagerNode {
		networksInfos, err := d.getNetworksInfosFromDocker()

		err = d.networks.(etcd.WriteOnlyStore[NetworkInfo]).Sync(networksInfos)
		if err != nil {
			return errors.WithMessage(err, "Error syncing networks")
		}
	} else {
		err := d.networks.(etcd.ReadOnlyStore[NetworkInfo]).Sync()
		if err != nil {
			return errors.WithMessage(err, "Error syncing networks")
		}
	}

	return nil
}

func (d *data) getNetworksInfosFromDocker() (networkInfos map[string]NetworkInfo, err error) {
	rawNetworks, err := d.dockerClient.NetworkList(context.Background(), network.ListOptions{})
	if err != nil {
		return nil, errors.WithMessage(err, "Error listing docker services")
	}

	networkInfos = map[string]NetworkInfo{}

	for _, network := range rawNetworks {
		networkInfo, ignored, err := d.getNetworkInfoFromDocker(network.ID)
		if err != nil {
			log.Printf("Error getting network info for network with ID %s. Skipping...\n", network.ID)
			continue
		}
		if ignored {
			continue
		}
		networkInfos[networkInfo.DockerID] = *networkInfo
	}

	return
}

func (d *data) getNetworkInfoFromDocker(dockerNetworkID string) (networkInfo *NetworkInfo, ignored bool, err error) {
	network, err := d.dockerClient.NetworkInspect(context.Background(), dockerNetworkID, network.InspectOptions{})
	if err != nil {
		return nil, false, errors.WithMessagef(err, "Error inspecting docker network %s", dockerNetworkID)
	}

	flannelNetworkID, exists := network.IPAM.Options["flannel-id"]
	if !exists {
		// Ignore, it's not for us, or it's misconfigured
		return nil, true, nil
	}

	return &NetworkInfo{
		DockerID:  dockerNetworkID,
		FlannelID: flannelNetworkID,
		Name:      network.Name,
	}, false, nil
}

func (d *data) handleNetwork(dockerNetworkID string) error {
	fmt.Printf("Handling docker network %s\n", dockerNetworkID)
	networkInfo, ignored, err := d.getNetworkInfoFromDocker(dockerNetworkID)
	if err != nil {
		return errors.WithMessagef(err, "Error inspecting docker network %s", dockerNetworkID)
	}

	if ignored {
		return nil
	}
	err = d.networks.(etcd.WriteOnlyStore[NetworkInfo]).AddOrUpdateItem(dockerNetworkID, *networkInfo)
	if err != nil {
		return errors.WithMessagef(err, "Error adding or updating network info %s", dockerNetworkID)
	}
	return nil
}

func (d *data) handleDeletedNetwork(dockerNetworkID string) error {
	fmt.Printf("Deleting network %s\n", dockerNetworkID)
	return d.networks.(etcd.WriteOnlyStore[NetworkInfo]).DeleteItem(dockerNetworkID)
}
