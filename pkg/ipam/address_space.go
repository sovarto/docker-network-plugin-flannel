package ipam

import (
	"fmt"
	"github.com/pkg/errors"
	"github.com/samber/lo"
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
	pools         map[string]net.IPNet
	etcdClient    etcd.Client
	sync.Mutex
}

func NewEtcdBasedAddressSpace(completeSpace []net.IPNet, poolSize int, etcdClient etcd.Client) (AddressSpace, error) {

	allSubnets, err := generateAllSubnets(completeSpace, poolSize)
	if err != nil {
		return nil, errors.Wrap(err, "error generating all subnets")
	}

	pools, err := getUsedSubnets(etcdClient)
	if err != nil {
		return nil, errors.Wrap(err, "error getting used subnets")
	}

	usedSubnets := lo.Values(pools)
	unusedSubnets := make([]net.IPNet, len(allSubnets))
	copy(unusedSubnets, allSubnets)

	for _, usedSubnet := range usedSubnets {
		unusedSubnets = lo.Filter(unusedSubnets, func(item net.IPNet, index int) bool {
			return item.String() != usedSubnet.String()
		})
	}

	fmt.Printf("%d total available subnets, of which %d are already used", len(allSubnets), len(pools))

	space := &etcdAddressSpace{
		completeSpace: completeSpace,
		poolSize:      poolSize,
		allSubnets:    allSubnets,
		unusedSubnets: unusedSubnets,
		pools:         pools,
		etcdClient:    etcdClient,
	}

	_, err = space.watchForSubnetUsageChanges(etcdClient)

	if err != nil {
		return nil, errors.Wrap(err, "error creating etcd watcher to watch for used subnets")
	}

	return space, nil
}

func (as *etcdAddressSpace) GetCompleteAddressSpace() []net.IPNet { return as.completeSpace }
func (as *etcdAddressSpace) GetPoolSize() int                     { return as.poolSize }
func (as *etcdAddressSpace) GetNewOrExistingPool(id string) (*net.IPNet, error) {
	pool, has := as.pools[id]
	if has {
		return &pool, nil
	}
	return as.GetNewPool(id)
}

func (as *etcdAddressSpace) GetPoolById(id string) *net.IPNet {
	pool, has := as.pools[id]
	if has {
		return &pool
	}
	return nil
}

func (as *etcdAddressSpace) GetNewPool(id string) (*net.IPNet, error) {
	as.Lock()
	defer as.Unlock()

	if _, has := as.pools[id]; !has {
		return nil, fmt.Errorf("address pool '%s' already exists", id)
	}
	for {
		if len(as.unusedSubnets) == 0 {
			return nil, fmt.Errorf("unable to get new pool with id '%s': there are no unused subnets left", id)
		}

		subnet := as.unusedSubnets[0]

		result, err := reservePoolSubnet(as.etcdClient, subnet.String(), id)
		if err != nil {
			return nil, errors.Wrapf(err, "error reserving subnet %s for pool %s", subnet.String(), id)
		}

		as.unusedSubnets = as.unusedSubnets[1:]

		if result.Success {
			fmt.Printf("reserved subnet %s for pool %s\n", subnet.String(), id)
			as.pools[id] = subnet
			return &subnet, nil
		}

		as.pools[result.PoolID] = subnet
		fmt.Printf("couldn't reserve subnet %s for pool %s. It has been reserved by pool '%s' in the meantime. Trying next available subnet...\n", subnet.String(), id, result.PoolID)
	}
}

func (as *etcdAddressSpace) ReleasePool(id string) error {
	as.Lock()
	defer as.Unlock()

	subnet, has := as.pools[id]
	if !has {
		return fmt.Errorf("address pool '%s' does not exist", id)
	}

	result, err := releasePoolSubnet(as.etcdClient, subnet.String(), id)

	if err != nil {
		return errors.Wrapf(err, "error releasing subnet %s for pool %s", subnet.String(), id)
	}

	if !result.Success {
		as.pools[result.PoolID] = subnet
		return fmt.Errorf("couldn't release subnet %s for pool %s. It has since been registered for different pool %s. This shouldn't happen.\n", subnet.String(), id, result.PoolID)
	}

	delete(as.pools, id)
	as.unusedSubnets = append(as.unusedSubnets, subnet)

	return nil
}

func (as *etcdAddressSpace) watchForSubnetUsageChanges(etcdClient etcd.Client) (clientv3.WatchChan, error) {
	prefix := subnetsKey(etcdClient)
	watcher, err := etcd.WithConnection(etcdClient, func(conn *etcd.Connection) (clientv3.WatchChan, error) {
		return conn.Client.Watch(conn.Ctx, prefix, clientv3.WithPrefix()), nil
	})

	go func() {
		for wresp := range watcher {
			for _, ev := range wresp.Events {
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
						if existingSubnet, has := as.pools[poolID]; has && existingSubnet.String() != subnet.String() {
							fmt.Printf("found new subnet '%s' for pool '%s'. It was '%s' so far. This shouldn't happen.\n ", subnet.String(), poolID, existingSubnet.String())
						} else if !has {
							as.pools[poolID] = *subnet
							as.unusedSubnets = lo.Filter(as.unusedSubnets, func(item net.IPNet, index int) bool {
								return item.String() != subnet.String()
							})
							fmt.Printf("found new used pool subnet '%s'. There are still %d pool subnets available", subnet.String(), len(as.unusedSubnets))
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
						if _, has := as.pools[poolID]; !has {
							// the subnet has already been deleted in our in-memory data
						} else {
							delete(as.pools, poolID)
							as.unusedSubnets = append(as.unusedSubnets, *subnet)
						}
						as.Unlock()
					}
				}
			}
		}
	}()

	return watcher, err
}
