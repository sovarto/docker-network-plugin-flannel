package docker

import (
	"context"
	"fmt"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/client"
	"github.com/pkg/errors"
	"github.com/sovarto/FlannelNetworkPlugin/pkg/common"
	"github.com/sovarto/FlannelNetworkPlugin/pkg/etcd"
	clientv3 "go.etcd.io/etcd/client/v3"
	"log"
	"os"
	"sync"
)

type Data interface {
	GetFlannelNetworkID(dockerNetworkID string) (flannelNetworkID string, exists bool)
	GetDockerNetworkID(flannelNetworkID string) (dockerNetworkID string, exists bool)
	Init() error
	GetContainers() etcd.ShardedDistributedStore[common.ContainerInfo]
	GetServices() etcd.Store[common.ServiceInfo]
}

type Callbacks struct {
	NetworkAdded   func(dockerNetworkID string, flannelNetworkID string)
	NetworkChanged func(dockerNetworkID string, flannelNetworkID string)
	NetworkRemoved func(dockerNetworkID string, flannelNetworkID string)
}

type data struct {
	dockerClient                      *client.Client
	dockerNetworkIDtoFlannelNetworkID map[string]string
	flannelNetworkIDtoDockerNetworkID map[string]string
	etcdClient                        etcd.Client
	hostname                          string
	containers                        etcd.ShardedDistributedStore[common.ContainerInfo]
	services                          etcd.Store[common.ServiceInfo]
	isManagerNode                     bool
	callbacks                         Callbacks
	sync.Mutex
}

func NewData(etcdClient etcd.Client,
	containerHandlers etcd.ShardItemsHandlers[common.ContainerInfo],
	serviceHandlers etcd.ItemsHandlers[common.ServiceInfo],
	callbacks Callbacks) (Data, error) {
	hostname, err := os.Hostname()
	if err != nil {
		return nil, errors.WithMessage(err, "error getting hostname")
	}

	dockerClient, err := client.NewClientWithOpts(
		client.WithHost("unix:///var/run/docker.sock"),
		client.WithAPIVersionNegotiation(),
	)

	if err != nil {
		return nil, errors.WithMessage(err, "error creating docker client")
	}

	info, err := dockerClient.Info(context.Background())
	if err != nil {
		return nil, errors.WithMessage(err, "Error getting docker info")
	}
	isManagerNode := info.Swarm.ControlAvailable

	containers := etcd.NewShardedDistributedStore(etcdClient.CreateSubClient("containers"), hostname, containerHandlers)
	var services etcd.Store[common.ServiceInfo]
	servicesEtcdClient := etcdClient.CreateSubClient("services", "ipam-vips")
	if isManagerNode {
		services = etcd.NewWriteOnlyStore(servicesEtcdClient, serviceHandlers)
	} else {
		services = etcd.NewReadOnlyStore(servicesEtcdClient, serviceHandlers)
	}

	result := &data{
		etcdClient:                        etcdClient,
		dockerClient:                      dockerClient,
		dockerNetworkIDtoFlannelNetworkID: make(map[string]string),
		flannelNetworkIDtoDockerNetworkID: make(map[string]string),
		containers:                        containers,
		services:                          services,
		hostname:                          hostname,
		callbacks:                         callbacks,
		isManagerNode:                     isManagerNode,
	}

	return result, nil
}

func (d *data) GetContainers() etcd.ShardedDistributedStore[common.ContainerInfo] {
	return d.containers
}

func (d *data) GetServices() etcd.Store[common.ServiceInfo] { return d.services }

func (d *data) GetFlannelNetworkID(dockerNetworkID string) (flannelNetworkID string, exists bool) {
	flannelNetworkID, exists = d.dockerNetworkIDtoFlannelNetworkID[dockerNetworkID]
	return flannelNetworkID, exists
}
func (d *data) GetDockerNetworkID(flannelNetworkID string) (dockerNetworkID string, exists bool) {
	dockerNetworkID, exists = d.flannelNetworkIDtoDockerNetworkID[flannelNetworkID]
	return dockerNetworkID, exists
}

func (d *data) Init() error {
	err := d.syncNetworks()
	if err != nil {
		return err
	}

	err = d.initServices()
	if err != nil {
		return err
	}

	err = d.initContainers()
	if err != nil {
		return err
	}

	go d.handleDockerEvents()

	_, _, err = d.etcdClient.Watch(d.networksKey(), true, d.networksChangeHandler)
	if err != nil {
		return errors.WithMessage(err, "failed to watch for network changes")
	}

	return nil
}

func (d *data) handleNetwork(dockerNetworkID string) error {
	network, err := d.dockerClient.NetworkInspect(context.Background(), dockerNetworkID, network.InspectOptions{})
	if err != nil {
		return errors.WithMessagef(err, "Error inspecting docker network %s", dockerNetworkID)
	}

	flannelNetworkID, exists := network.IPAM.Options["flannel-id"]
	if !exists {
		// Ignore, it's not for us, or it's misconfigured
		return nil
	}

	previousFlannelNetworkID, exists := d.dockerNetworkIDtoFlannelNetworkID[dockerNetworkID]
	d.dockerNetworkIDtoFlannelNetworkID[dockerNetworkID] = flannelNetworkID
	d.flannelNetworkIDtoDockerNetworkID[flannelNetworkID] = dockerNetworkID
	fmt.Printf("Network %s has flannel network id: %s\n", dockerNetworkID, flannelNetworkID)

	err = d.storeNetworkInfo(dockerNetworkID, flannelNetworkID)
	if err != nil {
		return errors.WithMessagef(err, "Error storing network info for docker network %s", dockerNetworkID)
	}

	if !exists {
		if d.callbacks.NetworkAdded != nil {
			d.callbacks.NetworkAdded(dockerNetworkID, flannelNetworkID)
		}
	} else if previousFlannelNetworkID != flannelNetworkID {
		if d.callbacks.NetworkChanged != nil {
			d.callbacks.NetworkChanged(dockerNetworkID, flannelNetworkID)
		}
	}

	return nil
}

func (d *data) handleDeletedNetwork(dockerNetworkID string) error {
	flannelNetworkID, exists := d.dockerNetworkIDtoFlannelNetworkID[dockerNetworkID]

	delete(d.dockerNetworkIDtoFlannelNetworkID, dockerNetworkID)
	delete(d.flannelNetworkIDtoDockerNetworkID, flannelNetworkID)

	err := d.deleteNetworkInfo(dockerNetworkID)
	if err != nil {
		return errors.WithMessagef(err, "Error deleting network info for docker network %s", dockerNetworkID)
	}

	if exists {
		if d.callbacks.NetworkRemoved != nil {
			d.callbacks.NetworkRemoved(dockerNetworkID, flannelNetworkID)
		}
	}

	return nil
}

func (d *data) syncNetworks() error {
	d.Lock()
	defer d.Unlock()
	fmt.Println("Syncing networks...")

	loadedNetworks, err := d.loadNetworkInfo()

	if err != nil {
		return errors.WithMessage(err, "Error loading docker network info from etcd")
	}

	oldNetworks := d.dockerNetworkIDtoFlannelNetworkID
	d.dockerNetworkIDtoFlannelNetworkID = loadedNetworks
	for dockerNetworkID, flannelNetworkID := range d.dockerNetworkIDtoFlannelNetworkID {
		d.flannelNetworkIDtoDockerNetworkID[flannelNetworkID] = dockerNetworkID
	}

	d.invokeNetworksCallbacks(oldNetworks, d.dockerNetworkIDtoFlannelNetworkID)

	networks, err := d.dockerClient.NetworkList(context.Background(), network.ListOptions{})
	if err != nil {
		return errors.WithMessage(err, "Error listing docker networks")
	}

	for _, network := range networks {
		err = d.handleNetwork(network.ID)
		if err != nil {
			return errors.WithMessagef(err, "Error handling docker network %s", network.ID)
		}
	}

	return nil
}

func (d *data) networksChangeHandler(watcher clientv3.WatchChan, prefix string) {
	for wresp := range watcher {
		for _, ev := range wresp.Events {
			fmt.Printf("Received network change event from etcd: %+v\n", ev)
			err := d.syncNetworks()
			if err != nil {
				log.Printf("Error syncing networks: %+v\n", err)
			}
		}
	}
}

func (d *data) invokeNetworksCallbacks(previousDockerNetworkIDtoFlannelNetworkID map[string]string, currentDockerNetworkIDtoFlannelNetworkID map[string]string) {
	processedItems := make(map[string]bool)

	for dockerNetworkID, currentFlannelNetworkID := range currentDockerNetworkIDtoFlannelNetworkID {
		previousFlannelNetworkID := previousDockerNetworkIDtoFlannelNetworkID[dockerNetworkID]
		if previousFlannelNetworkID == "" {
			if d.callbacks.NetworkAdded != nil {
				d.callbacks.NetworkAdded(dockerNetworkID, currentFlannelNetworkID)
			}
		} else {
			if d.callbacks.NetworkChanged != nil {
				d.callbacks.NetworkChanged(dockerNetworkID, currentFlannelNetworkID)
			}
		}
		processedItems[dockerNetworkID] = true
	}

	if d.callbacks.NetworkRemoved != nil {
		for dockerNetworkID, previousFlannelNetworkID := range previousDockerNetworkIDtoFlannelNetworkID {
			if !processedItems[dockerNetworkID] {
				d.callbacks.NetworkRemoved(dockerNetworkID, previousFlannelNetworkID)
			}
		}
	}
}
