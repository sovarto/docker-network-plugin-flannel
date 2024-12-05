package ipam

import (
	"fmt"
	"github.com/sovarto/FlannelNetworkPlugin/pkg/etcd"
	clientv3 "go.etcd.io/etcd/client/v3"
	"net"
	"strings"
	"time"
)

var (
	reservationsKeyName = "reservations"
	reservedAtKeyName   = "reserved-at"
	macKeyName          = "mac"
)

func reservedIPsKey(client etcd.Client) string {
	return client.GetKey(reservationsKeyName)
}

func reservedIPKey(client etcd.Client, ip string) string {
	return fmt.Sprintf("%s/%s", reservedIPsKey(client), ip)
}

func macKey(client etcd.Client, ip string) string {
	return fmt.Sprintf("%s/%s", reservedIPKey(client, ip), macKeyName)
}

func reservedAtKey(client etcd.Client, ip string) string {
	return fmt.Sprintf("%s/%s", reservedIPKey(client, ip), reservedAtKeyName)
}

func getReservations(client etcd.Client) (map[string]reservation, error) {
	return getReservationsByPrefix(client, reservedIPsKey(client))
}

type IPLeaseResult struct {
	Success     bool
	Reservation reservation
}

func releaseReservation(client etcd.Client, r reservation) (IPLeaseResult, error) {
	return etcd.WithConnection(client, func(conn *etcd.Connection) (IPLeaseResult, error) {
		ipStr := r.ip.String()
		key := reservedIPKey(client, ipStr)
		macKey := macKey(client, ipStr)
		cmps := []clientv3.Cmp{
			clientv3.Compare(clientv3.Value(key), "=", r.reservationType),
		}

		if r.mac == "" {
			cmps = append(cmps, clientv3.Compare(clientv3.CreateRevision(macKey), "=", 0))
		} else {
			cmps = append(cmps, clientv3.Compare(clientv3.Value(macKey), "=", r.mac))
		}

		resp, err := conn.Client.Txn(conn.Ctx).
			If(cmps...).
			Then(
				clientv3.OpDelete(key),
				clientv3.OpDelete(reservedAtKey(client, ipStr)),
				clientv3.OpDelete(macKey)).
			Else(
				clientv3.OpGet(key),
				clientv3.OpGet(reservedAtKey(client, ipStr)),
				clientv3.OpGet(macKey),
			).
			Commit()

		if err != nil {
			return IPLeaseResult{Success: false}, err
		}

		if resp.Succeeded {
			return IPLeaseResult{Success: true}, nil
		}

		reservedAtStr := string(resp.Responses[1].GetResponseRange().Kvs[0].Value)
		reservedAt, err := time.Parse(time.RFC3339, reservedAtStr)
		if err != nil {
			fmt.Printf("Couldn't parse reserved at value '%s' for '%s'. Ignoring...\n", reservedAtStr, key)
		}

		var mac string
		if len(resp.Responses[2].GetResponseRange().Kvs) > 0 {
			mac = string(resp.Responses[2].GetResponseRange().Kvs[0].Value)
		}

		return IPLeaseResult{Success: false, Reservation: reservation{
			ip:              r.ip,
			reservationType: string(resp.Responses[0].GetResponseRange().Kvs[0].Value),
			reservedAt:      reservedAt,
			mac:             mac,
		}}, nil
	})
}

func allocateReservedIPForContainer(client etcd.Client, reservedIP net.IP, mac string) (IPLeaseResult, error) {
	key := reservedIPKey(client, reservedIP.String())
	macKey := macKey(client, reservedIP.String())

	conditions := [][]clientv3.Cmp{
		// It was already reserved, but without a MAC - usually first pass IPAM, before a container exists
		// This is the default case
		{
			clientv3.Compare(clientv3.Value(key), "=", ReservationTypeReserved),
			clientv3.Compare(clientv3.CreateRevision(macKey), "=", 0),
		},
		// It was never before reserved or has since been freed -> shouldn't ever happen
		{
			clientv3.Compare(clientv3.CreateRevision(key), "=", 0),
		},
	}

	if mac != "" {
		conditions = append(conditions, [][]clientv3.Cmp{
			// It was already reserved for this same MAC -> shouldn't ever happen, because we
			// don't get a MAC during first pass IPAM
			{
				clientv3.Compare(clientv3.Value(key), "=", ReservationTypeReserved),
				clientv3.Compare(clientv3.Value(macKey), "=", mac),
			},
			// It was already reserved for this same MAC for a container -> shouldn't ever happen
			{
				clientv3.Compare(clientv3.Value(key), "=", ReservationTypeContainerIP),
				clientv3.Compare(clientv3.Value(macKey), "=", mac),
			},
		}...)
	}

	return reserveIPByCondition(client, reservedIP, ReservationTypeContainerIP, mac, conditions)
}

func allocateReservedIPForService(client etcd.Client, reservedIP net.IP) (IPLeaseResult, error) {
	key := reservedIPKey(client, reservedIP.String())
	macKey := macKey(client, reservedIP.String())

	conditions := [][]clientv3.Cmp{
		// It was already reserved, but without a MAC
		// This is the default case
		{
			clientv3.Compare(clientv3.Value(key), "=", ReservationTypeReserved),
			clientv3.Compare(clientv3.CreateRevision(macKey), "=", 0),
		},
		// It was never before reserved or has since been freed -> shouldn't ever happen
		{
			clientv3.Compare(clientv3.CreateRevision(key), "=", 0),
		},
	}

	return reserveIPByCondition(client, reservedIP, ReservationTypeContainerIP, "", conditions)
}

func reserveIP(client etcd.Client, ip net.IP, reservationType, mac string) (IPLeaseResult, error) {
	key := reservedIPKey(client, ip.String())
	macKey := macKey(client, ip.String())

	conditions := [][]clientv3.Cmp{
		// It was never before reserved or has since been freed -> default case
		{
			clientv3.Compare(clientv3.CreateRevision(key), "=", 0),
		},
	}

	if mac != "" {
		conditions = append(conditions, [][]clientv3.Cmp{
			// It was already reserved for this same MAC and reservation type -> shouldn't ever happen
			{
				clientv3.Compare(clientv3.Value(key), "=", reservationType),
				clientv3.Compare(clientv3.Value(macKey), "=", mac),
			},
		}...)
	}
	return reserveIPByCondition(client, ip, reservationType, mac, conditions)
}

func reserveIPByCondition(client etcd.Client, ip net.IP, reservationType, mac string, conditions [][]clientv3.Cmp) (IPLeaseResult, error) {
	key := reservedIPKey(client, ip.String())
	macKey := macKey(client, ip.String())

	return etcd.WithConnection(client, func(conn *etcd.Connection) (IPLeaseResult, error) {

		now := time.Now()

		ops := []clientv3.Op{
			clientv3.OpPut(key, reservationType),
			clientv3.OpPut(reservedAtKey(client, ip.String()), now.Format(time.RFC3339)),
		}

		if mac != "" {
			ops = append(ops, clientv3.OpPut(macKey, mac))
		}

		for _, condition := range conditions {
			txn := conn.Client.Txn(conn.Ctx).
				If(condition...).
				Then(ops...).
				Else()

			txnResp, err := txn.Commit()
			if err != nil {
				return IPLeaseResult{Success: false}, fmt.Errorf("etcd transaction failed: %v", err)
			}

			if txnResp.Succeeded {
				return IPLeaseResult{Success: true, Reservation: reservation{
					ip:              ip,
					reservationType: reservationType,
					reservedAt:      now,
					mac:             mac,
				}}, nil
			}
		}

		return IPLeaseResult{Success: false}, nil
	})
}

func readReservation(client etcd.Client, ip string) (*reservation, error) {
	tmp, err := getReservationsByPrefix(client, reservedIPKey(client, ip))

	if err != nil {
		return nil, err
	}

	r, has := tmp[ip]
	if !has {
		return nil, nil
	}

	return &r, nil
}

func getReservationsByPrefix(client etcd.Client, prefix string) (map[string]reservation, error) {
	return etcd.WithConnection(client, func(connection *etcd.Connection) (map[string]reservation, error) {
		resp, err := connection.Client.Get(connection.Ctx, prefix, clientv3.WithPrefix(), clientv3.WithSort(clientv3.SortByKey, clientv3.SortAscend))
		if err != nil {
			return nil, err
		}

		result := make(map[string]reservation)
		for _, kv := range resp.Kvs {
			key := strings.TrimLeft(strings.TrimPrefix(string(kv.Key), prefix), "/")
			value := string(kv.Value)

			if !strings.Contains(key, "/") {
				ip := net.ParseIP(key)

				if ip == nil {
					fmt.Printf("couldn't parse %s as IP. Skipping...\n", key)
					continue
				}

				addOrUpdate(result, key, reservation{
					ip:              ip,
					reservationType: value,
				}, func(existing reservation) {
					existing.ip = ip
					existing.reservationType = value
				})
			} else {
				parts := strings.Split(key, "/")
				if len(parts) == 2 {
					if parts[1] == macKeyName {
						addOrUpdate(result, key, reservation{mac: value}, func(existing reservation) { existing.mac = value })
					} else if parts[1] == reservedAtKeyName {
						reservedAt, err := time.Parse(time.RFC3339, value)
						if err != nil {
							fmt.Printf("Couldn't parse reserved at value '%s' for '%s'. Skipping...\n", value, key)
							continue
						}
						addOrUpdate(result, key, reservation{reservedAt: reservedAt},
							func(existing reservation) { existing.reservedAt = reservedAt })
					}
				}
			}
		}

		return result, nil
	})
}

func deleteAllReservations(client etcd.Client) error {
	_, err := etcd.WithConnection(client, func(connection *etcd.Connection) (struct{}, error) {
		_, err := connection.Client.Delete(connection.Ctx, reservedIPsKey(client), clientv3.WithPrefix())
		return struct{}{}, err
	})

	return err
}

func addOrUpdate[T any](store map[string]T, id string, valueToAdd T, update func(existing T)) {
	existing, exists := store[id]
	if exists {
		if update != nil {
			update(existing)
		}
	} else {
		existing = valueToAdd
	}
	store[id] = existing
}
