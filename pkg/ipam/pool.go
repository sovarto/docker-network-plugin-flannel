package ipam

import (
	"fmt"
	"github.com/pkg/errors"
	"github.com/sovarto/FlannelNetworkPlugin/pkg/etcd"
	"golang.org/x/exp/maps"
	"math/rand"
	"net"
	"sync"
	"time"
)

type AddressPool interface {
	GetID() string
	GetPoolSubnet() net.IPNet
	AllocateIP(preferredIP, mac, allocationType string, random bool) (*net.IP, error)
	ReleaseIP(ip string) error
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

	// TODO: Watcher

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

	return pool, nil
}

func (p *etcdPool) GetID() string            { return p.poolID }
func (p *etcdPool) GetPoolSubnet() net.IPNet { return p.poolSubnet }

func (p *etcdPool) AllocateIP(reservedIP, mac, allocationType string, random bool) (*net.IP, error) {
	p.Lock()
	defer p.Unlock()

	if reservedIP != "" && mac != "" && allocationType == ReservationTypeContainerIP {
		parsedIP := net.ParseIP(reservedIP)
		if parsedIP == nil {
			return nil, fmt.Errorf("reserved IP %s is invalid", reservedIP)
		}

		inSubnet := p.poolSubnet.Contains(parsedIP)
		if inSubnet {
			result, err := allocateReservedIPForContainer(p.etcdClient, parsedIP, mac)

			if err != nil {
				return nil, errors.Wrapf(err, "Error allocating reserved IP %s for container with MAC %s", reservedIP, mac)
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
			return nil, errors.Wrapf(err, "Error getting available unused IPs for pool %s", p.poolID)
		}

		var ip net.IP
		if random {
			ip = availableUnusedIPs[rand.Intn(len(availableUnusedIPs))]
		} else {
			nextIp, err := getNextAvailableIP(availableUnusedIPs, p.reservedIPs)
			if err != nil {
				return nil, errors.Wrapf(err, "Error getting next available IP for pool %s", p.poolID)
			}
			ip = nextIp
		}

		result, err := reserveIP(p.etcdClient, ip, allocationType, mac)
	}
}

func (p *etcdPool) ReleaseIP(ip string) error {
	p.Lock()
	defer p.Unlock()

	reservation, has := p.reservedIPs[ip]

	if !has {
		return fmt.Errorf("IP %s is not reserved in pool %s. Can't release it", ip, p.poolID)
	}

	result, err := releaseReservation(p.etcdClient, reservation)

	if err != nil {
		return errors.Wrapf(err, "error releasing ip %s for pool %s", ip, p.poolID)
	}

	if !result.Success {
		p.reservedIPs[ip] = result.Reservation
		return fmt.Errorf("couldn't release ip %s for pool %s. It has since been reserved like this: %+v. This shouldn't happen.\n", ip, p.poolID, result.Reservation)
	}

	delete(p.reservedIPs, ip)
	p.unusedIPs[ip] = time.Now()

	return nil
}

func (p *etcdPool) syncIPs() error {
	reservedIPs, err := getReservations(p.etcdClient)

	if err != nil {
		return errors.Wrapf(err, "Error getting reservations for pool %s", p.poolID)
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
			return nil, errors.Wrapf(err, "Error syncing reserved IPs for pool %s when allocating a new IP and no more unused IPs were available", p.poolID)
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
