package ipam

import (
	"fmt"
	"github.com/pkg/errors"
	"github.com/samber/lo"
	"github.com/sovarto/FlannelNetworkPlugin/pkg/common"
	"github.com/sovarto/FlannelNetworkPlugin/pkg/etcd"
	"go.etcd.io/etcd/api/v3/mvccpb"
	clientv3 "go.etcd.io/etcd/client/v3"
	"log"
	"net"
	"strings"
	"sync"
)

type AddressSpace interface {
	GetCompleteAddressSpace() []net.IPNet
	GetPoolSize() int
	GetNewPool(id string) (*net.IPNet, error)
	GetPoolById(id string) *net.IPNet
	GetNewOrExistingPool(id string) (*net.IPNet, error)
	ReleasePool(id string) error
}

type etcdAddressSpace struct {
	completeSpace []net.IPNet
	poolSize      int
	allSubnets    []net.IPNet
	unusedSubnets []net.IPNet
	pools         *common.ConcurrentMap[string, net.IPNet] // poolID (flannel network ID) -> subnet of the pool
	etcdClient    etcd.Client
	sync.Mutex
}

func NewEtcdBasedAddressSpace(completeSpace []net.IPNet, poolSize int, etcdClient etcd.Client) (AddressSpace, error) {

	allSubnets, err := generateAllSubnets(completeSpace, poolSize)
	if err != nil {
		return nil, errors.WithMessage(err, "error generating all subnets")
	}

	pools, err := getUsedSubnets(etcdClient)
	if err != nil {
		return nil, errors.WithMessage(err, "error getting used subnets")
	}

	usedSubnets := pools.Values()
	unusedSubnets := make([]net.IPNet, len(allSubnets))
	copy(unusedSubnets, allSubnets)

	for _, usedSubnet := range usedSubnets {
		unusedSubnets = lo.Filter(unusedSubnets, func(item net.IPNet, index int) bool {
			return item.String() != usedSubnet.String()
		})
	}

	fmt.Printf("%d total available subnets (= docker swarm networks), of which %d are already used\n", len(allSubnets), pools.Count())

	space := &etcdAddressSpace{
		completeSpace: completeSpace,
		poolSize:      poolSize,
		allSubnets:    allSubnets,
		unusedSubnets: unusedSubnets,
		pools:         pools,
		etcdClient:    etcdClient,
	}

	_, _, err = etcdClient.Watch(subnetsKey(etcdClient), true, space.subnetUsageChangeHandler)

	if err != nil {
		return nil, errors.WithMessage(err, "error creating etcd watcher to watch for used subnets")
	}
	return space, nil
}

func (as *etcdAddressSpace) GetCompleteAddressSpace() []net.IPNet { return as.completeSpace }
func (as *etcdAddressSpace) GetPoolSize() int                     { return as.poolSize }
func (as *etcdAddressSpace) GetNewOrExistingPool(id string) (*net.IPNet, error) {
	as.Lock()
	defer as.Unlock()

	pool, _, err := as.getOrAddPool(id)

	return pool, err
}

func (as *etcdAddressSpace) GetPoolById(id string) *net.IPNet {
	pool, has := as.pools.Get(id)
	if has {
		return &pool
	}
	return nil
}

func (as *etcdAddressSpace) GetNewPool(id string) (*net.IPNet, error) {
	as.Lock()
	defer as.Unlock()

	pool, wasAdded, err := as.getOrAddPool(id)
	if !wasAdded {
		return nil, fmt.Errorf("address pool '%s' already exists", id)
	}

	return pool, err
}

func (as *etcdAddressSpace) getOrAddPool(id string) (subnet *net.IPNet, isNewPool bool, err error) {
	foundPoolReservations := make(map[string]*net.IPNet)

	value, wasAdded, err := as.pools.GetOrAdd(id, func() (net.IPNet, error) {
		for {
			if len(as.unusedSubnets) == 0 {
				return net.IPNet{}, fmt.Errorf("unable to get new pool with id '%s': there are no unused subnets left", id)
			}

			subnet := as.unusedSubnets[0]

			result, err := reservePoolSubnet(as.etcdClient, subnet.String(), id)
			if err != nil {
				return net.IPNet{}, errors.WithMessagef(err, "error reserving subnet %s for pool %s", subnet.String(), id)
			}

			as.unusedSubnets = as.unusedSubnets[1:]

			if result.Success {
				fmt.Printf("reserved subnet %s for pool %s\n", subnet.String(), id)
				return subnet, nil
			}

			foundPoolReservations[result.PoolID] = &subnet
			fmt.Printf("couldn't reserve subnet %s for pool %s. It has been reserved by pool '%s' in the meantime. Trying next available subnet...\n", subnet.String(), id, result.PoolID)
		}
	})

	for id, subnet := range foundPoolReservations {
		as.pools.Set(id, *subnet)
	}

	if err != nil {
		return nil, false, err
	}

	return &value, wasAdded, nil
}

func (as *etcdAddressSpace) ReleasePool(id string) error {
	as.Lock()
	defer as.Unlock()

	subnet, wasRemoved := as.pools.TryRemove(id)
	if !wasRemoved {
		return fmt.Errorf("address pool '%s' does not exist", id)
	}

	result, err := releasePoolSubnet(as.etcdClient, subnet.String(), id)

	if err != nil {
		return errors.WithMessagef(err, "error releasing subnet %s for pool %s", subnet.String(), id)
	}

	if !result.Success {
		as.pools.Set(result.PoolID, subnet)
		return fmt.Errorf("couldn't release subnet %s for pool %s. It has since been registered for different pool %s. This shouldn't happen.\n", subnet.String(), id, result.PoolID)
	}

	as.unusedSubnets = append(as.unusedSubnets, subnet)

	return nil
}

func (as *etcdAddressSpace) subnetUsageChangeHandler(watcher clientv3.WatchChan, prefix string) {
	fmt.Println("Starting subnetUsageChangeHandler...")
	for wresp := range watcher {
		for _, ev := range wresp.Events {
			fmt.Printf("watchForSubnetUsageChanges received event %+v\n", ev)
			key := strings.TrimLeft(strings.TrimPrefix(string(ev.Kv.Key), prefix), "/")
			if !strings.Contains(key, "/") {
				switch ev.Type {
				case mvccpb.PUT:
					poolID := string(ev.Kv.Value)
					_, subnet, err := net.ParseCIDR(strings.ReplaceAll(key, "-", "/"))
					if err != nil {
						log.Printf("found new used pool subnet '%s' for pool '%s', but it can't be parsed as a CIDR. This looks like a data issue. Ignoring..., err: %v", key, poolID, err)
						continue
					}
					as.Lock()
					_, wasUpdated := as.pools.AddOrUpdate(poolID, *subnet, func(existing net.IPNet) net.IPNet {
						if existing.String() != subnet.String() {
							fmt.Printf("found new subnet '%s' for pool '%s'. It was '%s' so far. This shouldn't happen.\n ", subnet.String(), poolID, existing.String())
						} else {
							// found subnet and in memory subnet are the same, so nothing to do
						}

						return existing
					})

					if !wasUpdated {
						as.unusedSubnets = lo.Filter(as.unusedSubnets, func(item net.IPNet, index int) bool {
							return item.String() != subnet.String()
						})
						fmt.Printf("found new used pool subnet '%s' for pool %s. There are still %d pool subnets available\n", subnet.String(), poolID, len(as.unusedSubnets))
					} else {
						// found subnet and in memory subnet are the same, so nothing to do
					}
					as.Unlock()
					break
				case mvccpb.DELETE:
					poolID := string(ev.Kv.Value)
					_, subnet, err := net.ParseCIDR(strings.ReplaceAll(key, "-", "/"))
					if err != nil {
						log.Printf("found deleted used pool subnet '%s' for pool '%s', but it can't be parsed as a CIDR. This looks like a data issue. Ignoring..., err: %v", key, poolID, err)
						continue
					}

					as.Lock()
					_, wasRemoved := as.pools.TryRemove(poolID)
					if wasRemoved {
						as.unusedSubnets = append(as.unusedSubnets, *subnet)
					}
					as.Unlock()
				}
			}
		}
	}

	fmt.Println("subnetUsageChangeHandler finished")
}
