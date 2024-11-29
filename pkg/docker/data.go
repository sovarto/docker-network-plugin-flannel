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
	GetFlannelNetworkID(dockerNetworkID string) (string, error)
}

type Callbacks struct {
	ContainerChanged func(previousContainerInfo *common.ContainerInfo, currentContainerInfo common.ContainerInfo)
	ContainerAdded   func(containerInfo common.ContainerInfo)
	ContainerRemoved func(containerInfo common.ContainerInfo)
	ServiceChanged   func(previousServiceInfo *common.ServiceInfo, currentServiceInfo common.ServiceInfo)
	ServiceAdded     func(serviceInfo common.ServiceInfo)
	ServiceRemoved   func(serviceID string)
	NetworkAdded     func(networkID string)
	NetworkRemoved   func(networkID string)
}

type data struct {
	dockerClient                      *client.Client
	dockerNetworkIDtoFlannelNetworkID map[string]string
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
		return nil, errors.Wrap(err, "error getting hostname")
	}

	dockerClient, err := client.NewClientWithOpts(
		client.WithHost("unix:///var/run/docker.sock"),
		client.WithAPIVersionNegotiation(),
	)

	if err != nil {
		return nil, errors.Wrap(err, "error creating docker client")
	}

	info, err := dockerClient.Info(context.Background())
	if err != nil {
		return nil, errors.Wrap(err, "Error getting docker info")
	}

	result := &data{
		etcdClient:                        etcdClient,
		dockerClient:                      dockerClient,
		dockerNetworkIDtoFlannelNetworkID: make(map[string]string),
		containers:                        make(map[string]common.ContainerInfo),
		services:                          make(map[string]common.ServiceInfo),
		callbacks:                         callbacks,
		hostname:                          hostname,
		isManagerNode:                     info.Swarm.ControlAvailable,
	}

	err = result.syncNetworks()
	if err != nil {
		return nil, err
	}

	err = result.syncContainersAndServices()
	if err != nil {
		return nil, err
	}

	go result.handleEvents()

	_, err = result.watchForNetworkChanges(etcdClient)
	if err != nil {
		return nil, errors.Wrap(err, "failed to watch for network changes")
	}
	_, err = result.watchForContainerChanges(etcdClient)
	if err != nil {
		return nil, errors.Wrap(err, "failed to watch for container changes")
	}
	_, err = result.watchForServiceChanges(etcdClient)
	if err != nil {
		return nil, errors.Wrap(err, "failed to watch for service changes")
	}

	return result, nil
}

func (d *data) GetFlannelNetworkID(dockerNetworkID string) (string, error) {
	d.Lock()
	defer d.Unlock()

	flannelNetworkID, exists := d.dockerNetworkIDtoFlannelNetworkID[dockerNetworkID]
	if !exists {
		err := d.syncNetworks()
		if err != nil {
			return "", err
		}
		flannelNetworkID, exists = d.dockerNetworkIDtoFlannelNetworkID[dockerNetworkID]
	}

	if !exists {
		return "", fmt.Errorf("flannel network ID not found for docker network %s", dockerNetworkID)
	}

	return flannelNetworkID, nil
}

func (d *data) handleNetwork(networkID string) error {
	network, err := d.dockerClient.NetworkInspect(context.Background(), networkID, network.InspectOptions{})
	if err != nil {
		return errors.Wrapf(err, "Error inspecting docker network %s", networkID)
	}

	id, exists := network.IPAM.Options["flannel-id"]
	if !exists {
		// Ignore, it's not for us, or it's misconfigured
		return nil
	}

	_, exists = d.dockerNetworkIDtoFlannelNetworkID[id]
	d.dockerNetworkIDtoFlannelNetworkID[network.ID] = id
	fmt.Printf("Network %s has flannel network id: %s\n", network.ID, id)

	err = storeNetworkInfo(d.etcdClient, network.ID, id)
	if err != nil {
		return errors.Wrapf(err, "Error storing network info for docker network %s: %+v\n", network.ID, err)
	}

	if !exists && d.callbacks.NetworkAdded != nil {
		d.callbacks.NetworkAdded(network.ID)
	}

	return nil
}

func (d *data) handleDeletedNetwork(networkID string) error {
	_, exists := d.dockerNetworkIDtoFlannelNetworkID[networkID]

	delete(d.dockerNetworkIDtoFlannelNetworkID, networkID)

	err := deleteNetworkInfo(d.etcdClient, networkID)
	if err != nil {
		return errors.Wrapf(err, "Error deleting network info for docker network %s: %+v\n", networkID, err)
	}

	if exists {
		if d.callbacks.NetworkRemoved != nil {
			d.callbacks.NetworkRemoved(networkID)
		}
	}

	return nil
}

func (d *data) handleContainer(containerID string) error {
	container, err := d.dockerClient.ContainerInspect(context.Background(), containerID)
	if err != nil {
		return errors.Wrapf(err, "Error inspecting docker container %s: %+v\n", containerID, err)
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
			return errors.Wrapf(err, "Error deepcopying service info for docker service %s", serviceID)
		}
	}

	serviceInfo.Containers[containerID] = containerInfo

	for _, networkData := range container.NetworkSettings.Networks {
		networkID := networkData.NetworkID
		ip := net.ParseIP(networkData.IPAddress)
		ips[networkID] = ip
		if networkData.IPAMConfig != nil && networkData.IPAMConfig.IPv4Address != "" {
			ipamIP := net.ParseIP(networkData.IPAMConfig.IPv4Address)
			ipamIPs[networkID] = ipamIP
		}
	}

	containerInfo, containerExists := d.containers[containerID]
	var previousContainerInfo *common.ContainerInfo
	if containerExists {
		previousContainerInfo = &common.ContainerInfo{}
		err = deepcopy.Copy(previousContainerInfo, containerInfo)
		if err != nil {
			return errors.Wrapf(err, "Error deepcopying container info for docker container %s", containerID)
		}
	}
	d.containers[containerID] = containerInfo

	err = storeContainerAndServiceInfo(d.etcdClient, d.hostname, containerInfo, serviceID, serviceName)
	if err != nil {
		return errors.Wrapf(err, "Error storing container and service info for container %s: %+v\n", containerID, err)
	}

	d.invokeContainerCallback(previousContainerInfo, containerInfo)
	d.invokeServiceCallback(previousServiceInfo, serviceInfo)

	return nil
}

func (d *data) syncNetworks() error {
	d.Lock()
	defer d.Unlock()

	loadedNetworks, err := loadNetworkInfo(d.etcdClient)

	if err != nil {
		return errors.Wrapf(err, "Error loading docker network info from etcd: %+v\n", err)
	}

	d.dockerNetworkIDtoFlannelNetworkID = loadedNetworks

	networks, err := d.dockerClient.NetworkList(context.Background(), network.ListOptions{})
	if err != nil {
		return errors.Wrapf(err, "Error listing docker networks: %+v\n", err)
	}

	for _, network := range networks {
		err = d.handleNetwork(network.ID)
		if err != nil {
			return errors.Wrapf(err, "Error handling docker network %s: %+v\n", network.ID, err)
		}
	}

	return nil
}

func (d *data) syncContainersAndServices() error {
	d.Lock()
	defer d.Unlock()

	loadedContainers, err := loadContainersInfo(d.etcdClient, d.hostname)
	if err != nil {
		return errors.Wrapf(err, "Error loading docker containers info from etcd: %+v\n", err)
	}
	d.containers = loadedContainers

	loadedServices, err := loadServicesInfo(d.etcdClient)
	if err != nil {
		return errors.Wrapf(err, "Error loading docker services info from etcd: %+v\n", err)
	}
	oldServices := d.services
	d.services = loadedServices
	d.invokeServicesCallbacks(oldServices, loadedServices)

	containers, err := d.dockerClient.ContainerList(context.Background(), container.ListOptions{})
	if err != nil {
		return errors.Wrapf(err, "Error listing docker containers: %+v\n", err)
	}

	for _, container := range containers {
		err = d.handleContainer(container.ID)
		if err != nil {
			return errors.Wrapf(err, "Error handling docker container %s: %+v\n", container.ID, err)
		}
	}

	if d.isManagerNode {
		services, err := d.dockerClient.ServiceList(context.Background(), types.ServiceListOptions{})
		if err != nil {
			return errors.Wrapf(err, "Error listing docker services: %+v\n", err)
		}

		for _, service := range services {
			err = d.handleService(service.ID)
			if err != nil {
				return errors.Wrapf(err, "Error handling docker service %s: %+v\n", service.ID, err)
			}
		}
	}

	return nil
}

func (d *data) handleService(serviceID string) error {
	service, _, err := d.dockerClient.ServiceInspectWithRaw(context.Background(), serviceID, types.ServiceInspectOptions{})
	if err != nil {
		return errors.Wrapf(err, "Error inspecting docker service %s: %+v\n", serviceID, err)
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
			return errors.Wrapf(err, "Error deepcopying service info for docker service %s", serviceID)
		}
	}

	for _, endpoint := range service.Endpoint.VirtualIPs {
		ip := net.ParseIP(endpoint.Addr)
		if ip == nil {
			return fmt.Errorf("error parsing IP address %s for service %s", endpoint.Addr, serviceID)
		}
		serviceInfo.VIPs[endpoint.NetworkID] = ip
	}

	err = storeServiceVIPs(d.etcdClient, serviceID, serviceInfo.VIPs)
	if err != nil {
		return errors.Wrapf(err, "Error storing service VIPs for service %s: %+v\n", serviceID, err)
	}

	d.invokeServiceCallback(previousServiceInfo, serviceInfo)

	return nil
}

func (d *data) handleDeletedService(serviceID string) error {
	_, exists := d.services[serviceID]

	delete(d.services, serviceID)

	err := deleteServiceInfo(d.etcdClient, serviceID)
	if err != nil {
		return errors.Wrapf(err, "Error deleting service info for docker service %s: %+v\n", serviceID, err)
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
		return errors.Wrapf(err, "Error deleting container info for docker container %s: %+v\n", containerID, err)
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
				return errors.Wrapf(err, "Error deepcopying service info for docker service %s", serviceID)
			}

			delete(serviceInfo.Containers, containerID)
			err = deleteContainerFromServiceInfo(d.etcdClient, serviceID, containerID)
			if err != nil {
				return errors.Wrapf(err, "Error deleting container %s from service %s", containerID, serviceID)
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

func (d *data) invokeServiceCallback(previousServiceInfo *common.ServiceInfo, currentServiceInfo common.ServiceInfo) {
	if previousServiceInfo != nil {
		if d.callbacks.ServiceChanged != nil {
			if !reflect.DeepEqual(*previousServiceInfo, currentServiceInfo) {
				d.callbacks.ServiceChanged(previousServiceInfo, currentServiceInfo)
			}
		}
	} else {
		if d.callbacks.ServiceAdded != nil {
			d.callbacks.ServiceAdded(currentServiceInfo)
		}
	}
}

func (d *data) invokeServicesCallbacks(previousServiceInfos map[string]common.ServiceInfo, currentServiceInfos map[string]common.ServiceInfo) {

	processedServices := make(map[string]bool)

	for _, currentService := range currentServiceInfos {
		if previousService, exists := previousServiceInfos[currentService.ID]; exists {
			d.invokeServiceCallback(&previousService, currentService)
		} else {
			d.invokeServiceCallback(nil, currentService)
		}
		processedServices[currentService.ID] = true
	}

	if d.callbacks.ServiceRemoved != nil {
		for _, previousService := range previousServiceInfos {
			if !processedServices[previousService.ID] {
				d.callbacks.ServiceRemoved(previousService.ID)
			}
		}
	}
}

func (d *data) invokeContainerCallback(previousContainerInfo *common.ContainerInfo, currentContainerInfo common.ContainerInfo) {
	if previousContainerInfo != nil {
		if d.callbacks.ContainerChanged != nil {
			if !reflect.DeepEqual(*previousContainerInfo, currentContainerInfo) {
				d.callbacks.ContainerChanged(previousContainerInfo, currentContainerInfo)
			}
		}
	} else {
		if d.callbacks.ContainerAdded != nil {
			d.callbacks.ContainerAdded(currentContainerInfo)
		}
	}
}

func (d *data) invokeContainersCallbacks(previousContainerInfos map[string]common.ContainerInfo, currentContainerInfos map[string]common.ContainerInfo) error {

	processedContainers := make(map[string]bool)

	for _, currentContainer := range currentContainerInfos {
		if previousContainer, exists := previousContainerInfos[currentContainer.ID]; exists {
			d.invokeContainerCallback(&previousContainer, currentContainer)
		} else {
			d.invokeContainerCallback(nil, currentContainer)
		}
		processedContainers[currentContainer.ID] = true
	}

	if d.callbacks.ContainerRemoved != nil {
		for _, previousContainer := range previousContainerInfos {
			if !processedContainers[previousContainer.ID] {
				d.callbacks.ContainerRemoved(previousContainer)
			}
		}
	}

	return nil
}
