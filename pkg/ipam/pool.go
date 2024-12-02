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
	AllocateIP(preferredIP, mac, allocationType string, random bool) (*net.IP, error)
	ReleaseIP(ip string) error
	ReleaseAllIPs() error
}

type reservation struct {
	ip              net.IP
	reservationType string
	reservedAt      time.Time
	mac             string
}

type etcdPool struct {
	poolID      string
	poolSubnet  net.IPNet
	etcdClient  etcd.Client
	allIPs      []net.IP
	reservedIPs map[string]reservation
	// The time since when this IP is unused. Not stored in etcd, because it is only for short-term
	// prevention of rapid re-assignment of the same IP after it was just released
	unusedIPs map[string]time.Time
	sync.Mutex
}

var (
	ReservationTypeReserved    = "reserved"
	ReservationTypeContainerIP = "container-ip"
	ReservationTypeServiceVIP  = "service-vip"
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

func (p *etcdPool) AllocateIP(reservedIP, mac, allocationType string, random bool) (*net.IP, error) {
	p.Lock()
	defer p.Unlock()

	if reservedIP != "" && ((mac != "" && allocationType == ReservationTypeContainerIP) || allocationType == ReservationTypeServiceVIP) {
		parsedIP := net.ParseIP(reservedIP)
		if parsedIP == nil {
			return nil, fmt.Errorf("reserved IP %s is invalid", reservedIP)
		}

		inSubnet := p.poolSubnet.Contains(parsedIP)
		if inSubnet {
			var result IPLeaseResult
			var err error
			if allocationType == ReservationTypeContainerIP {
				result, err = allocateReservedIPForContainer(p.etcdClient, parsedIP, mac)
				if err != nil {
					return nil, errors.WithMessagef(err, "Error allocating reserved IP %s for container with MAC %s", reservedIP, mac)
				}
			} else {
				result, err = allocateReservedIPForService(p.etcdClient, parsedIP)
				if err != nil {
					return nil, errors.WithMessagef(err, "Error allocating reserved IP %s for service", reservedIP)
				}
			}

			if result.Success {
				_, has := p.reservedIPs[reservedIP]
				if !has {
					fmt.Printf("reserved IP %s wasn't previously reserved. This shouldn't happen.", reservedIP)
					delete(p.unusedIPs, reservedIP)
				}

				p.reservedIPs[reservedIP] = result.Reservation

				return &result.Reservation.ip, nil
			}
		}
	}

	for {
		availableUnusedIPs, err := p.getAvailableUnusedIPs()

		if err != nil {
			return nil, errors.WithMessagef(err, "Error getting available unused IPs for pool %s", p.poolID)
		}

		var ip net.IP
		if random {
			ip = availableUnusedIPs[rand.Intn(len(availableUnusedIPs))]
		} else {
			nextIp, err := getNextAvailableIP(availableUnusedIPs, p.reservedIPs)
			if err != nil {
				return nil, errors.WithMessagef(err, "Error getting next available IP for pool %s", p.poolID)
			}
			ip = nextIp
		}

		result, err := reserveIP(p.etcdClient, ip, allocationType, mac)

		if err != nil {
			return nil, errors.WithMessagef(err, "Error reserving IP for pool %s", p.poolID)
		}

		if result.Success {
			ipStr := result.Reservation.ip.String()
			delete(p.unusedIPs, ipStr)
			p.reservedIPs[ipStr] = result.Reservation
			return &result.Reservation.ip, nil
		}
	}
}

func (p *etcdPool) ReleaseIP(ip string) error {
	p.Lock()
	defer p.Unlock()

	fmt.Printf("Releasing IP %s for pool %s...", ip, p.poolID)

	reservation, has := p.reservedIPs[ip]

	if !has {
		return fmt.Errorf("IP %s is not reserved in pool %s. Can't release it", ip, p.poolID)
	}

	result, err := releaseReservation(p.etcdClient, reservation)

	if err != nil {
		return errors.WithMessagef(err, "error releasing ip %s for pool %s", ip, p.poolID)
	}

	if !result.Success {
		p.reservedIPs[ip] = result.Reservation
		return fmt.Errorf("couldn't release ip %s for pool %s. It has since been reserved like this: Reservation Type: %s; IP: %s; MAC %s, Reserved At: %s. This shouldn't happen.\n", ip, p.poolID, result.Reservation.reservationType, result.Reservation.ip.String(), result.Reservation.mac, result.Reservation.reservedAt.Format(time.RFC3339))
	}

	delete(p.reservedIPs, ip)
	p.unusedIPs[ip] = time.Now()

	return nil
}

func (p *etcdPool) ReleaseAllIPs() error {
	p.Lock()
	defer p.Unlock()

	err := deleteAllReservations(p.etcdClient)
	if err != nil {
		return errors.WithMessagef(err, "Error deleting all reserved IPs for pool %s", p.poolID)
	}

	return p.syncIPs()
}

func (p *etcdPool) syncIPs() error {
	reservedIPs, err := getReservations(p.etcdClient)

	if err != nil {
		return errors.WithMessagef(err, "Error getting reservations for pool %s", p.poolID)
	}

	unusedIPs := make(map[string]time.Time)

	for _, ip := range p.allIPs {
		if _, has := reservedIPs[ip.String()]; !has {
			unusedIPs[ip.String()] = time.Now()
		}
	}

	p.reservedIPs = reservedIPs
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
	prefix := reservedIPsKey(etcdClient)
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
					existingReservation, has := p.reservedIPs[ipStr]
					r, err := readReservation(p.etcdClient, ipStr)

					if err != nil {
						log.Printf("found new reservation for IP '%s' for pool '%s', but retrieving the whole object resulted in an error: %v", ipStr, p.poolID, err)
						continue
					}

					if r == nil {
						log.Printf("found new reservation for IP '%s' for pool '%s', but when retrieving the whole object, it could no longer be found", ipStr, p.poolID)
						continue
					}

					if has && (existingReservation.reservationType != r.reservationType || existingReservation.reservedAt != r.reservedAt || existingReservation.mac != r.mac) {
						fmt.Printf("found change in reservation data of ip %s from %+v to %+v. This shouldn't happen", ipStr, existingReservation, r)
						p.reservedIPs[ipStr] = *r
					} else if !has {
						fmt.Printf("found new reserved IP %s for pool %s. This shouldn't happen", ipStr, p.poolID)
						delete(p.unusedIPs, ipStr)
						p.reservedIPs[ipStr] = *r
					} else {
						// found reservation and in memory reservation have the same reservation type
					}
					p.Unlock()
					break
				case mvccpb.DELETE:
					ip := net.ParseIP(keyParts[0])
					if ip == nil {
						log.Printf("found deleted reservation '%s' for pool '%s', but it can't be parsed as a IP. This looks like a data issue. Ignoring..., err: %v", key, p.poolID, err)
						continue
					}
					ipStr := ip.String()

					p.Lock()
					if _, has := p.reservedIPs[ipStr]; !has {
						// the reservation has already been deleted in our in-memory data
					} else {
						log.Printf("found deleted reservation for IP '%s' in pool '%s'. This shouldn't happen", ipStr, p.poolID)
						delete(p.reservedIPs, ipStr)
						p.unusedIPs[ipStr] = time.Now()
					}
					p.Unlock()
				}
			}
		}
	}()

	return watcher, err
}
