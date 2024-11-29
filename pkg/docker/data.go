package docker

import (
	"context"
	"fmt"
	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/client"
	"github.com/pkg/errors"
	"github.com/sovarto/FlannelNetworkPlugin/pkg/common"
	"github.com/sovarto/FlannelNetworkPlugin/pkg/etcd"
	"github.com/tiendc/go-deepcopy"
	clientv3 "go.etcd.io/etcd/client/v3"
	"log"
	"net"
	"os"
	"reflect"
	"strings"
	"sync"
)

type Data interface {
	GetFlannelNetworkID(dockerNetworkID string) (flannelNetworkID string, exists bool)
	GetDockerNetworkID(flannelNetworkID string) (dockerNetworkID string, exists bool)
	Init() error
}

type Callbacks struct {
	ContainerChanged func(previousContainerInfo *common.ContainerInfo, currentContainerInfo common.ContainerInfo)
	ContainerAdded   func(containerInfo common.ContainerInfo)
	ContainerRemoved func(containerInfo common.ContainerInfo)
	ServiceChanged   func(previousServiceInfo *common.ServiceInfo, currentServiceInfo common.ServiceInfo)
	ServiceAdded     func(serviceInfo common.ServiceInfo)
	ServiceRemoved   func(serviceID string)
	NetworkAdded     func(dockerNetworkID string, flannelNetworkID string)
	NetworkChanged   func(dockerNetworkID string, flannelNetworkID string)
	NetworkRemoved   func(dockerNetworkID string, flannelNetworkID string)
}

type data struct {
	dockerClient                      *client.Client
	dockerNetworkIDtoFlannelNetworkID map[string]string
	flannelNetworkIDtoDockerNetworkID map[string]string
	etcdClient                        etcd.Client
	hostname                          string
	containers                        map[string]common.ContainerInfo // containerID -> info
	services                          map[string]common.ServiceInfo   // serviceID -> info
	callbacks                         Callbacks
	isManagerNode                     bool
	sync.Mutex
}

func NewData(etcdClient etcd.Client, callbacks Callbacks) (Data, error) {
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

	result := &data{
		etcdClient:                        etcdClient,
		dockerClient:                      dockerClient,
		dockerNetworkIDtoFlannelNetworkID: make(map[string]string),
		flannelNetworkIDtoDockerNetworkID: make(map[string]string),
		containers:                        make(map[string]common.ContainerInfo),
		services:                          make(map[string]common.ServiceInfo),
		callbacks:                         callbacks,
		hostname:                          hostname,
		isManagerNode:                     info.Swarm.ControlAvailable,
	}

	return result, nil
}

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

	err = d.syncContainersAndServices()
	if err != nil {
		return err
	}

	go d.handleEvents()

	_, err = d.watchForNetworkChanges(d.etcdClient)
	if err != nil {
		return errors.WithMessage(err, "failed to watch for network changes")
	}
	_, err = d.watchForContainerChanges(d.etcdClient)
	if err != nil {
		return errors.WithMessage(err, "failed to watch for container changes")
	}
	_, err = d.watchForServiceChanges(d.etcdClient)
	if err != nil {
		return errors.WithMessage(err, "failed to watch for service changes")
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

	err = storeNetworkInfo(d.etcdClient, dockerNetworkID, flannelNetworkID)
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

	err := deleteNetworkInfo(d.etcdClient, dockerNetworkID)
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

func (d *data) handleContainer(containerID string) error {
	container, err := d.dockerClient.ContainerInspect(context.Background(), containerID)
	if err != nil {
		return errors.WithMessagef(err, "Error inspecting docker container %s", containerID)
	}

	serviceID := container.Config.Labels["com.docker.swarm.service.id"]
	serviceName := container.Config.Labels["com.docker.swarm.service.name"]
	containerName := strings.TrimLeft(container.Name, "/")

	ips := make(map[string]net.IP)
	ipamIPs := make(map[string]net.IP)

	containerInfo := common.ContainerInfo{
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

	if len(ips) == 0 {
		return nil
	}

	previousContainerInfo := d.containers[containerID]
	d.containers[containerID] = containerInfo

	serviceInfo, serviceExists := d.services[serviceID]
	var previousServiceInfo *common.ServiceInfo
	if !serviceExists {
		serviceInfo = common.ServiceInfo{
			ID:         serviceID,
			Name:       serviceName,
			Containers: make(map[string]common.ContainerInfo),
			VIPs:       make(map[string]net.IP),
		}

		d.services[serviceID] = serviceInfo
	} else {
		previousServiceInfo = &common.ServiceInfo{}
		err = deepcopy.Copy(previousServiceInfo, serviceInfo)
		if err != nil {
			return errors.WithMessagef(err, "Error deepcopying service info for docker service %s", serviceID)
		}
	}

	serviceInfo.Containers[containerID] = containerInfo

	err = storeContainerAndServiceInfo(d.etcdClient, d.hostname, containerInfo, serviceID, serviceName)
	if err != nil {
		return errors.WithMessagef(err, "Error storing container and service info for container %s", containerID)
	}

	d.invokeContainerCallback(&previousContainerInfo, containerInfo)
	d.invokeServiceCallback(previousServiceInfo, serviceInfo)

	return nil
}

func (d *data) syncNetworks() error {
	d.Lock()
	defer d.Unlock()

	loadedNetworks, err := loadNetworkInfo(d.etcdClient)

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

func (d *data) syncContainersAndServices() error {
	d.Lock()
	defer d.Unlock()

	fmt.Println("syncing containers and services")

	loadedContainers, err := loadContainersInfo(d.etcdClient, d.hostname)
	if err != nil {
		return errors.WithMessage(err, "Error loading docker containers info from etcd")
	}
	oldContainers := d.containers
	d.containers = loadedContainers
	d.invokeContainersCallbacks(oldContainers, loadedContainers)

	loadedServices, err := loadServicesInfo(d.etcdClient)
	if err != nil {
		return errors.WithMessage(err, "Error loading docker services info from etcd")
	}
	oldServices := d.services
	d.services = loadedServices
	d.invokeServicesCallbacks(oldServices, loadedServices)

	containers, err := d.dockerClient.ContainerList(context.Background(), container.ListOptions{})
	if err != nil {
		return errors.WithMessage(err, "Error listing docker containers")
	}

	for _, container := range containers {
		err = d.handleContainer(container.ID)
		if err != nil {
			return errors.WithMessagef(err, "Error handling docker container %s", container.ID)
		}
	}

	if d.isManagerNode {
		services, err := d.dockerClient.ServiceList(context.Background(), types.ServiceListOptions{})
		if err != nil {
			return errors.WithMessage(err, "Error listing docker services")
		}

		for _, service := range services {
			err = d.handleService(service.ID)
			if err != nil {
				return errors.WithMessagef(err, "Error handling docker service %s", service.ID)
			}
		}
	}

	return nil
}

func (d *data) handleService(serviceID string) error {
	service, _, err := d.dockerClient.ServiceInspectWithRaw(context.Background(), serviceID, types.ServiceInspectOptions{})
	if err != nil {
		return errors.WithMessagef(err, "Error inspecting docker service %s", serviceID)
	}

	serviceName := service.Spec.Name
	serviceInfo, serviceExists := d.services[serviceID]
	var previousServiceInfo *common.ServiceInfo
	if !serviceExists {
		serviceInfo = common.ServiceInfo{
			ID:         serviceID,
			Name:       serviceName,
			Containers: make(map[string]common.ContainerInfo),
			VIPs:       make(map[string]net.IP),
		}

		d.services[serviceID] = serviceInfo
	} else {
		previousServiceInfo = &common.ServiceInfo{}
		err = deepcopy.Copy(previousServiceInfo, serviceInfo)
		if err != nil {
			return errors.WithMessagef(err, "Error deepcopying service info for docker service %s", serviceID)
		}
	}

	for _, endpoint := range service.Endpoint.VirtualIPs {
		ip, _, err := net.ParseCIDR(endpoint.Addr)
		if err != nil {
			return errors.WithMessagef(err, "error parsing IP address %s for service %s", endpoint.Addr, serviceID)
		}
		serviceInfo.VIPs[endpoint.NetworkID] = ip
	}

	err = storeServiceVIPs(d.etcdClient, serviceID, serviceInfo.VIPs)
	if err != nil {
		return errors.WithMessagef(err, "Error storing service VIPs for service %s", serviceID)
	}

	d.invokeServiceCallback(previousServiceInfo, serviceInfo)

	return nil
}

func (d *data) handleDeletedService(serviceID string) error {
	_, exists := d.services[serviceID]

	delete(d.services, serviceID)

	err := deleteServiceInfo(d.etcdClient, serviceID)
	if err != nil {
		return errors.WithMessagef(err, "Error deleting service info for docker service %s", serviceID)
	}

	if exists {
		if d.callbacks.ServiceRemoved != nil {
			d.callbacks.ServiceRemoved(serviceID)
		}
	}

	return nil
}

func (d *data) handleDeletedContainer(containerID string) error {
	containerInfo, exists := d.containers[containerID]

	delete(d.containers, containerID)

	err := deleteContainerInfo(d.etcdClient, d.hostname, containerID)
	if err != nil {
		return errors.WithMessagef(err, "Error deleting container info for docker container %s", containerID)
	}

	if exists {
		if d.callbacks.ContainerRemoved != nil {
			d.callbacks.ContainerRemoved(containerInfo)
		}
	}

	for serviceID, serviceInfo := range d.services {
		_, exists := serviceInfo.Containers[containerID]
		if exists {
			previousServiceInfo := &common.ServiceInfo{}
			err = deepcopy.Copy(previousServiceInfo, serviceInfo)
			if err != nil {
				return errors.WithMessagef(err, "Error deepcopying service info for docker service %s", serviceID)
			}

			delete(serviceInfo.Containers, containerID)
			err = deleteContainerFromServiceInfo(d.etcdClient, serviceID, containerID)
			if err != nil {
				return errors.WithMessagef(err, "Error deleting container %s from service %s", containerID, serviceID)
			}
			if d.callbacks.ServiceAdded != nil {
				d.callbacks.ServiceChanged(previousServiceInfo, serviceInfo)
			}
			break
		}
	}

	return nil
}

func (d *data) watchForNetworkChanges(etcdClient etcd.Client) (clientv3.WatchChan, error) {
	prefix := networksKey(etcdClient)
	watcher, err := etcd.WithConnection(etcdClient, func(conn *etcd.Connection) (clientv3.WatchChan, error) {
		return conn.Client.Watch(conn.Ctx, prefix, clientv3.WithPrefix()), nil
	})

	go func() {
		for wresp := range watcher {
			for range wresp.Events {
				err := d.syncNetworks()
				if err != nil {
					log.Printf("Error syncing networks: %+v\n", err)
				}
			}
		}
	}()

	return watcher, err
}

func (d *data) watchForContainerChanges(etcdClient etcd.Client) (clientv3.WatchChan, error) {
	prefix := containersKey(etcdClient, d.hostname)
	watcher, err := etcd.WithConnection(etcdClient, func(conn *etcd.Connection) (clientv3.WatchChan, error) {
		return conn.Client.Watch(conn.Ctx, prefix, clientv3.WithPrefix()), nil
	})

	go func() {
		for wresp := range watcher {
			for range wresp.Events {
				err := d.syncContainersAndServices()
				if err != nil {
					log.Printf("Error syncing containers and services: %+v\n", err)
				}
			}
		}
	}()

	return watcher, err
}

func (d *data) watchForServiceChanges(etcdClient etcd.Client) (clientv3.WatchChan, error) {
	prefix := containersKey(etcdClient, d.hostname)
	watcher, err := etcd.WithConnection(etcdClient, func(conn *etcd.Connection) (clientv3.WatchChan, error) {
		return conn.Client.Watch(conn.Ctx, prefix, clientv3.WithPrefix()), nil
	})

	go func() {
		for wresp := range watcher {
			for range wresp.Events {
				err := d.syncContainersAndServices()
				if err != nil {
					log.Printf("Error syncing containers and services: %+v\n", err)
				}
			}
		}
	}()

	return watcher, err
}

func (d *data) invokeServiceCallback(previous *common.ServiceInfo, current common.ServiceInfo) {
	invokeItemCallback(previous, current, d.callbacks.ServiceAdded, d.callbacks.ServiceChanged)
}

func (d *data) invokeContainerCallback(previous *common.ContainerInfo, current common.ContainerInfo) {
	invokeItemCallback(previous, current, d.callbacks.ContainerAdded, d.callbacks.ContainerChanged)
}

func (d *data) invokeServicesCallbacks(previousServiceInfos map[string]common.ServiceInfo, currentServiceInfos map[string]common.ServiceInfo) {
	invokeItemsCallbacks(
		previousServiceInfos,
		currentServiceInfos,
		func(s common.ServiceInfo) string { return s.ID },
		d.invokeServiceCallback,
		func(s common.ServiceInfo) {
			if d.callbacks.ServiceRemoved != nil {
				d.callbacks.ServiceRemoved(s.ID)
			}
		},
	)
}

func (d *data) invokeContainersCallbacks(previousContainerInfos map[string]common.ContainerInfo, currentContainerInfos map[string]common.ContainerInfo) {
	invokeItemsCallbacks(
		previousContainerInfos,
		currentContainerInfos,
		func(c common.ContainerInfo) string { return c.ID },
		d.invokeContainerCallback,
		d.callbacks.ContainerRemoved,
	)
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

func invokeItemCallback[T any](
	previousItem *T,
	currentItem T,
	addedCallback func(T),
	changedCallback func(*T, T),
) {
	if previousItem != nil {
		if changedCallback != nil {
			if !reflect.DeepEqual(*previousItem, currentItem) {
				changedCallback(previousItem, currentItem)
			}
		}
	} else {
		if addedCallback != nil {
			addedCallback(currentItem)
		}
	}
}

func invokeItemsCallbacks[T any](
	previousItems map[string]T,
	currentItems map[string]T,
	getID func(T) string,
	invokeItemCallback func(previous *T, current T),
	removedCallback func(T),
) {
	processedItems := make(map[string]bool)

	for _, currentItem := range currentItems {
		id := getID(currentItem)
		previousItem := previousItems[id]
		invokeItemCallback(&previousItem, currentItem)
		processedItems[id] = true
	}

	if removedCallback != nil {
		for _, previousItem := range previousItems {
			id := getID(previousItem)
			if !processedItems[id] {
				removedCallback(previousItem)
			}
		}
	}
}
