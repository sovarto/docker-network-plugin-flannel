package docker

import (
	"context"
	"fmt"
	"github.com/davecgh/go-spew/spew"
	"github.com/docker/docker/client"
	"github.com/pkg/errors"
	"github.com/samber/lo"
	"github.com/sovarto/FlannelNetworkPlugin/pkg/common"
	"github.com/sovarto/FlannelNetworkPlugin/pkg/etcd"
	"os"
	"sync"
	"time"
)

type Data interface {
	Init() error
	GetContainers() etcd.ShardedDistributedStore[ContainerInfo]
	GetServices() etcd.Store[ServiceInfo]
	GetNetworks() etcd.Store[common.NetworkInfo]
}

type data struct {
	dockerClient      *client.Client
	etcdClient        etcd.Client
	hostname          string
	containers        etcd.ShardedDistributedStore[ContainerInfo]
	services          etcd.Store[ServiceInfo]
	networks          etcd.Store[common.NetworkInfo]
	isManagerNode     bool
	containerHandlers etcd.ShardItemsHandlers[ContainerInfo]
	serviceHandlers   etcd.ItemsHandlers[ServiceInfo]
	networkHandlers   etcd.ItemsHandlers[common.NetworkInfo]
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

	fmt.Println("Before creating docker client")
	dockerClient, err := client.NewClientWithOpts(
		client.WithHost("unix:///var/run/docker.sock"),
		client.WithAPIVersionNegotiation(),
		client.WithTimeout(time.Second*5),
	)

	if err != nil {
		return nil, errors.WithMessage(err, "error creating docker client")
	}
	fmt.Println("Before getting docker info")
	info, err := dockerClient.Info(context.Background())
	fmt.Printf("Docker info: %s\n", spew.Sdump(info))
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
		etcdClient:        etcdClient,
		dockerClient:      dockerClient,
		containers:        containers,
		services:          services,
		networks:          networks,
		hostname:          hostname,
		isManagerNode:     isManagerNode,
		containerHandlers: containerHandlers,
		serviceHandlers:   serviceHandlers,
		networkHandlers:   networkHandlers,
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

	d.networkHandlers.OnAdded(
		lo.MapToSlice(d.networks.GetAll(), func(key string, value common.NetworkInfo) etcd.Item[common.NetworkInfo] {
			return etcd.Item[common.NetworkInfo]{
				ID:    key,
				Value: value,
			}
		}))

	d.serviceHandlers.OnAdded(
		lo.MapToSlice(d.services.GetAll(), func(key string, value ServiceInfo) etcd.Item[ServiceInfo] {
			return etcd.Item[ServiceInfo]{
				ID:    key,
				Value: value,
			}
		}))

	d.containerHandlers.OnAdded(lo.Flatten(
		lo.MapToSlice(d.containers.GetAll(), func(shardKey string, shardValues map[string]ContainerInfo) []etcd.ShardItem[ContainerInfo] {
			return lo.MapToSlice(shardValues, func(key string, value ContainerInfo) etcd.ShardItem[ContainerInfo] {
				return etcd.ShardItem[ContainerInfo]{
					ShardKey: shardKey,
					ID:       key,
					Value:    value,
				}
			})
		})))

	go d.handleDockerEvents()

	return nil
}
