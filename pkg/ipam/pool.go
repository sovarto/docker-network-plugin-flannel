package ipam

import (
	"fmt"
	"github.com/pkg/errors"
	"github.com/sovarto/FlannelNetworkPlugin/pkg/etcd"
	"go.etcd.io/etcd/api/v3/mvccpb"
	clientv3 "go.etcd.io/etcd/client/v3"
	"golang.org/x/exp/maps"
	"log"
	"math/rand"
	"net"
	"strings"
	"sync"
	"time"
)

type AddressPool interface {
	GetID() string
	GetPoolSubnet() net.IPNet
	AllocateContainerIP(preferredIP, mac string, random bool) (*net.IP, error)
	AllocateServiceVIP(ipamVIP, serviceID string, random bool) (*net.IP, error)
	ReserveIP(random bool) (*net.IP, error)
	ReleaseIP(ip string) error
	ReleaseAllIPs() error
}

type allocation struct {
	ip             net.IP
	allocationType string
	allocatedAt    time.Time
	dataKey        string
	data           string
}

type etcdPool struct {
	poolID       string
	poolSubnet   net.IPNet
	etcdClient   etcd.Client
	allIPs       []net.IP
	allocatedIPs map[string]allocation
	// The time since when this IP is unused. Not stored in etcd, because it is only for short-term
	// prevention of rapid re-assignment of the same IP after it was just released
	unusedIPs map[string]time.Time
	sync.Mutex
}

var (
	AllocationTypeReserved    = "reserved"
	AllocationTypeContainerIP = "container-ip"
	AllocationTypeServiceVIP  = "service-vip"
)

func NewEtcdBasedAddressPool(poolID string, poolSubnet net.IPNet, etcdClient etcd.Client) (AddressPool, error) {

	allIPs := ipsInSubnet(poolSubnet, true)

	pool := &etcdPool{
		poolID:     poolID,
		poolSubnet: poolSubnet,
		etcdClient: etcdClient,
		allIPs:     allIPs,
	}

	err := pool.syncIPs()
	if err != nil {
		return nil, err
	}

	_, err = pool.watchForIPUsageChanges(etcdClient)

	return pool, nil
}

func (p *etcdPool) GetID() string            { return p.poolID }
func (p *etcdPool) GetPoolSubnet() net.IPNet { return p.poolSubnet }

func (p *etcdPool) ReserveIP(random bool) (*net.IP, error) {
	p.Lock()
	defer p.Unlock()

	return p.allocateFreeIP(random, func(ip net.IP) (IPAllocationResult, error) {
		return reserveIP(p.etcdClient, ip)
	})
}

func (p *etcdPool) AllocateContainerIP(preferredIP, mac string, random bool) (*net.IP, error) {
	p.Lock()
	defer p.Unlock()

	parsedIP := net.ParseIP(preferredIP)
	if parsedIP == nil {
		return nil, fmt.Errorf("preferred IP %s is invalid", preferredIP)
	}

	inSubnet := p.poolSubnet.Contains(parsedIP)
	if inSubnet {
		var result IPAllocationResult
		var err error

		result, err = allocateIPForContainer(p.etcdClient, parsedIP, mac)
		if err != nil {
			return nil, errors.WithMessagef(err, "Error allocating reserved IP %s for container with MAC %s", preferredIP, mac)
		}

		if result.Success {
			_, has := p.allocatedIPs[preferredIP]
			if !has {
				fmt.Printf("reserved IP %s wasn't previously reserved. This shouldn't happen.", preferredIP)
				delete(p.unusedIPs, preferredIP)
			}

			p.allocatedIPs[preferredIP] = result.Allocation

			return &result.Allocation.ip, nil
		}
	}

	return p.allocateFreeIP(random, func(ip net.IP) (IPAllocationResult, error) {
		return allocateIPForContainer(p.etcdClient, ip, mac)
	})
}

func (p *etcdPool) AllocateServiceVIP(ipamVIP, serviceID string, random bool) (*net.IP, error) {
	p.Lock()
	defer p.Unlock()

	parsedIP := net.ParseIP(ipamVIP)
	if parsedIP == nil {
		return nil, fmt.Errorf("ipam VIP %s is invalid", ipamVIP)
	}

	inSubnet := p.poolSubnet.Contains(parsedIP)
	if inSubnet {
		var result IPAllocationResult
		var err error
		result, err = allocateIPForService(p.etcdClient, parsedIP, serviceID)
		if err != nil {
			return nil, errors.WithMessagef(err, "Error allocating IPAM VIP %s for service %s", ipamVIP, serviceID)
		}

		if result.Success {
			_, has := p.allocatedIPs[ipamVIP]
			if !has {
				fmt.Printf("IPAM VIP %s wasn't previously reserved. This shouldn't happen.", ipamVIP)
				delete(p.unusedIPs, ipamVIP)
			}

			p.allocatedIPs[ipamVIP] = result.Allocation

			return &result.Allocation.ip, nil
		}
	}

	return p.allocateFreeIP(random, func(ip net.IP) (IPAllocationResult, error) {
		return allocateIPForService(p.etcdClient, ip, serviceID)
	})
}

func (p *etcdPool) allocateFreeIP(random bool, allocator func(ip net.IP) (IPAllocationResult, error)) (*net.IP, error) {
	for {
		availableUnusedIPs, err := p.getAvailableUnusedIPs()

		if err != nil {
			return nil, errors.WithMessagef(err, "Error getting available unused IPs for pool %s", p.poolID)
		}

		var ip net.IP
		if random {
			ip = availableUnusedIPs[rand.Intn(len(availableUnusedIPs))]
		} else {
			nextIp, err := getNextAvailableIP(availableUnusedIPs, p.allocatedIPs)
			if err != nil {
				return nil, errors.WithMessagef(err, "Error getting next available IP for pool %s", p.poolID)
			}
			ip = nextIp
		}

		result, err := allocator(ip)

		if err != nil {
			return nil, errors.WithMessagef(err, "Error reserving IP for pool %s", p.poolID)
		}

		if result.Success {
			ipStr := result.Allocation.ip.String()
			delete(p.unusedIPs, ipStr)
			p.allocatedIPs[ipStr] = result.Allocation
			return &result.Allocation.ip, nil
		}
	}
}

func (p *etcdPool) ReleaseIP(ip string) error {
	p.Lock()
	defer p.Unlock()

	fmt.Printf("Releasing IP %s for pool %s...\n", ip, p.poolID)

	allocation, has := p.allocatedIPs[ip]

	if !has {
		return fmt.Errorf("IP %s is not reserved in pool %s. Can't release it", ip, p.poolID)
	} else {
		fmt.Printf("Releasing allocation %+v for pool %s...\n", allocation, p.poolID)
	}

	result, err := releaseAllocation(p.etcdClient, allocation)

	if err != nil {
		return errors.WithMessagef(err, "error releasing ip %s for pool %s", ip, p.poolID)
	}

	if !result.Success {
		p.allocatedIPs[ip] = result.Allocation
		return fmt.Errorf("couldn't release ip %s for pool %s. It has since been allocated like this: Allocation Type: %s; IP: %s; MAC %s, Reserved At: %s. This shouldn't happen.\n", ip, p.poolID, result.Allocation.allocationType, result.Allocation.ip.String(), result.Allocation.data, result.Allocation.allocatedAt.Format(time.RFC3339))
	}

	delete(p.allocatedIPs, ip)
	p.unusedIPs[ip] = time.Now()

	return nil
}

func (p *etcdPool) ReleaseAllIPs() error {
	p.Lock()
	defer p.Unlock()

	err := deleteAllAllocations(p.etcdClient)
	if err != nil {
		return errors.WithMessagef(err, "Error releasing all IPs for pool %s", p.poolID)
	}

	return p.syncIPs()
}

func (p *etcdPool) syncIPs() error {
	reservedIPs, err := getAllocations(p.etcdClient)

	if err != nil {
		return errors.WithMessagef(err, "Error getting allocations for pool %s", p.poolID)
	}

	unusedIPs := make(map[string]time.Time)

	for _, ip := range p.allIPs {
		if _, has := reservedIPs[ip.String()]; !has {
			unusedIPs[ip.String()] = time.Now()
		}
	}

	p.allocatedIPs = reservedIPs
	p.unusedIPs = unusedIPs

	return nil
}

func (p *etcdPool) getAvailableUnusedIPs() ([]net.IP, error) {
	availableUnusedIPs := []string{}
	for key, value := range p.unusedIPs {
		if value.Add(5 * time.Minute).Before(time.Now()) {
			availableUnusedIPs = append(availableUnusedIPs, key)
		}
	}
	if len(availableUnusedIPs) == 0 {
		availableUnusedIPs = maps.Keys(p.unusedIPs)
	}
	if len(availableUnusedIPs) == 0 {
		err := p.syncIPs()
		if err != nil {
			return nil, errors.WithMessagef(err, "Error syncing reserved IPs for pool %s when allocating a new IP and no more unused IPs were available", p.poolID)
		}
		availableUnusedIPs = maps.Keys(p.unusedIPs)
	}
	if len(availableUnusedIPs) == 0 {
		return nil, fmt.Errorf("no more IPs available for pool %s", p.poolID)
	}

	result := []net.IP{}

	for _, ip := range availableUnusedIPs {
		parsedIP := net.ParseIP(ip)
		if parsedIP == nil {
			return nil, fmt.Errorf("invalid IP: %s", ip)
		}
		result = append(result, parsedIP)
	}

	return result, nil
}

func (p *etcdPool) watchForIPUsageChanges(etcdClient etcd.Client) (clientv3.WatchChan, error) {
	prefix := allocatedIPsKey(etcdClient)
	watcher, err := etcd.WithConnection(etcdClient, func(conn *etcd.Connection) (clientv3.WatchChan, error) {
		return conn.Client.Watch(conn.Ctx, prefix, clientv3.WithPrefix()), nil
	})

	go func() {
		for wresp := range watcher {
			for _, ev := range wresp.Events {
				key := strings.TrimLeft(strings.TrimPrefix(string(ev.Kv.Key), prefix), "/")
				keyParts := strings.Split(key, "/")
				switch ev.Type {
				case mvccpb.PUT:
					ip := net.ParseIP(keyParts[0])
					if ip == nil {
						log.Printf("found new reserved IP '%s' for pool '%s', but it can't be parsed as an IP. This looks like a data issue. Ignoring..., err: %v", key, p.poolID, err)
						continue
					}
					ipStr := ip.String()
					p.Lock()
					existingReservation, has := p.allocatedIPs[ipStr]
					r, err := readAllocation(p.etcdClient, ipStr)

					if err != nil {
						log.Printf("found new allocation for IP '%s' for pool '%s', but retrieving the whole object resulted in an error: %v", ipStr, p.poolID, err)
						continue
					}

					if r == nil {
						log.Printf("found new allocation for IP '%s' for pool '%s', but when retrieving the whole object, it could no longer be found", ipStr, p.poolID)
						continue
					}

					if has && (existingReservation.allocationType != r.allocationType || existingReservation.allocatedAt != r.allocatedAt || existingReservation.data != r.data) {
						fmt.Printf("found change in allocation data of ip %s from %+v to %+v. This shouldn't happen", ipStr, existingReservation, r)
						p.allocatedIPs[ipStr] = *r
					} else if !has {
						fmt.Printf("found new reserved IP %s for pool %s. This shouldn't happen", ipStr, p.poolID)
						delete(p.unusedIPs, ipStr)
						p.allocatedIPs[ipStr] = *r
					} else {
						// found allocation and in memory allocation have the same allocation type
					}
					p.Unlock()
					break
				case mvccpb.DELETE:
					ip := net.ParseIP(keyParts[0])
					if ip == nil {
						log.Printf("found deleted allocation '%s' for pool '%s', but it can't be parsed as a IP. This looks like a data issue. Ignoring..., err: %v", key, p.poolID, err)
						continue
					}
					ipStr := ip.String()

					p.Lock()
					if _, has := p.allocatedIPs[ipStr]; !has {
						// the allocation has already been deleted in our in-memory data
					} else {
						log.Printf("found deleted allocation for IP '%s' in pool '%s'. This shouldn't happen", ipStr, p.poolID)
						delete(p.allocatedIPs, ipStr)
						p.unusedIPs[ipStr] = time.Now()
					}
					p.Unlock()
				}
			}
		}
	}()

	return watcher, err
}
