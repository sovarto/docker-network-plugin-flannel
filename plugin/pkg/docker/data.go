package docker

import (
	"context"
	"github.com/docker/docker/client"
	"github.com/pkg/errors"
	"github.com/sovarto/FlannelNetworkPlugin/pkg/common"
	"github.com/sovarto/FlannelNetworkPlugin/pkg/etcd"
	"os"
	"sync"
)

type Data interface {
	Init() error
	GetContainers() etcd.ShardedDistributedStore[ContainerInfo]
	GetServices() etcd.Store[ServiceInfo]
	GetNetworks() etcd.Store[common.NetworkInfo]
}

type data struct {
	dockerClient  *client.Client
	etcdClient    etcd.Client
	hostname      string
	containers    etcd.ShardedDistributedStore[ContainerInfo]
	services      etcd.Store[ServiceInfo]
	networks      etcd.Store[common.NetworkInfo]
	isManagerNode bool
	sync.Mutex
}

func NewData(etcdClient etcd.Client,
	containerHandlers etcd.ShardItemsHandlers[ContainerInfo],
	serviceHandlers etcd.ItemsHandlers[ServiceInfo],
	networkHandlers etcd.ItemsHandlers[common.NetworkInfo]) (Data, error) {
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
	var services etcd.Store[ServiceInfo]
	var networks etcd.Store[common.NetworkInfo]
	servicesEtcdClient := etcdClient.CreateSubClient("services")
	networksEtcdClient := etcdClient.CreateSubClient("networks")
	if isManagerNode {
		services = etcd.NewWriteOnlyStore(servicesEtcdClient, serviceHandlers)
		networks = etcd.NewWriteOnlyStore(networksEtcdClient, networkHandlers)
	} else {
		services = etcd.NewReadOnlyStore(servicesEtcdClient, serviceHandlers)
		networks = etcd.NewReadOnlyStore(networksEtcdClient, networkHandlers)
	}

	result := &data{
		etcdClient:    etcdClient,
		dockerClient:  dockerClient,
		containers:    containers,
		services:      services,
		networks:      networks,
		hostname:      hostname,
		isManagerNode: isManagerNode,
	}

	return result, nil
}

func (d *data) GetContainers() etcd.ShardedDistributedStore[ContainerInfo] { return d.containers }
func (d *data) GetServices() etcd.Store[ServiceInfo]                       { return d.services }
func (d *data) GetNetworks() etcd.Store[common.NetworkInfo]                { return d.networks }

func (d *data) Init() error {
	err := d.initNetworks()
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

	return nil
}
