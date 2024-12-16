package ipam

import (
	"fmt"
	"github.com/sovarto/FlannelNetworkPlugin/pkg/etcd"
	clientv3 "go.etcd.io/etcd/client/v3"
	"net"
	"strings"
	"time"
)

const (
	dataKeyPartMac       = "mac"
	dataKeyPartServiceID = "service-id"
	allocatedAtKeyPart   = "allocated-at"
)

func allocatedIPsKey(client etcd.Client) string {
	return client.GetKey("allocated-ips")
}

func allocatedIPKey(client etcd.Client, ip string) string {
	return fmt.Sprintf("%s/%s", allocatedIPsKey(client), ip)
}

func macKey(client etcd.Client, ip string) string {
	return fmt.Sprintf("%s/%s", allocatedIPKey(client, ip), dataKeyPartMac)
}

func serviceIDKey(client etcd.Client, ip string) string {
	return fmt.Sprintf("%s/%s", allocatedIPKey(client, ip), dataKeyPartServiceID)
}

func allocatedAtKey(client etcd.Client, ip string) string {
	return fmt.Sprintf("%s/%s", allocatedIPKey(client, ip), allocatedAtKeyPart)
}

func getAllocations(client etcd.Client) (map[string]allocation, error) {
	return getAllocationsByPrefix(client, allocatedIPsKey(client))
}

type IPAllocationResult struct {
	Success    bool
	Allocation allocation
}

func releaseAllocation(client etcd.Client, r allocation) (IPAllocationResult, error) {
	return etcd.WithConnection(client, func(conn *etcd.Connection) (IPAllocationResult, error) {
		ipStr := r.ip.String()
		key := allocatedIPKey(client, ipStr)
		macKey := macKey(client, ipStr)
		serviceIDKey := serviceIDKey(client, ipStr)
		cmps := []clientv3.Cmp{
			clientv3.Compare(clientv3.Value(key), "=", r.allocationType),
		}

		if r.data == "" || r.dataKey == "" {
			cmps = append(cmps,
				clientv3.Compare(clientv3.CreateRevision(macKey), "=", 0),
				clientv3.Compare(clientv3.CreateRevision(serviceIDKey), "=", 0),
			)
		} else {
			cmps = append(cmps, clientv3.Compare(clientv3.Value(r.dataKey), "=", r.data))
		}

		resp, err := conn.Client.Txn(conn.Ctx).
			If(cmps...).
			Then(
				clientv3.OpDelete(key),
				clientv3.OpDelete(allocatedAtKey(client, ipStr)),
				clientv3.OpDelete(macKey),
				clientv3.OpDelete(serviceIDKey),
			).
			Else(
				clientv3.OpGet(key),
				clientv3.OpGet(allocatedAtKey(client, ipStr)),
				clientv3.OpGet(macKey),
				clientv3.OpGet(serviceIDKey),
			).
			Commit()

		if err != nil {
			return IPAllocationResult{Success: false}, err
		}

		if resp.Succeeded {
			return IPAllocationResult{Success: true}, nil
		}

		allocatedAtStr := string(resp.Responses[1].GetResponseRange().Kvs[0].Value)
		allocatedAt, err := time.Parse(time.RFC3339, allocatedAtStr)
		if err != nil {
			fmt.Printf("Couldn't parse allocated at value '%s' for '%s'. Ignoring...\n", allocatedAtStr, key)
		}

		var data string
		var dataKey string
		getMacResp := resp.Responses[2].GetResponseRange().Kvs
		getServiceIDResp := resp.Responses[3].GetResponseRange().Kvs
		if len(getMacResp) > 0 {
			data = string(getMacResp[0].Value)
			dataKey = string(getMacResp[0].Key)
		} else if len(getServiceIDResp) > 0 {
			data = string(getServiceIDResp[0].Value)
			dataKey = string(getServiceIDResp[0].Key)
		}

		return IPAllocationResult{Success: false, Allocation: allocation{
			ip:             r.ip,
			allocationType: string(resp.Responses[0].GetResponseRange().Kvs[0].Value),
			allocatedAt:    allocatedAt,
			dataKey:        dataKey,
			data:           data,
		}}, nil
	})
}

func allocateIPForContainer(client etcd.Client, reservedIP net.IP, mac string) (IPAllocationResult, error) {
	key := allocatedIPKey(client, reservedIP.String())
	macKey := macKey(client, reservedIP.String())

	conditions := [][]clientv3.Cmp{
		// It was already reserved, but without a MAC - usually first pass IPAM, before a container exists
		// This is the default case
		{
			clientv3.Compare(clientv3.Value(key), "=", AllocationTypeReserved),
			clientv3.Compare(clientv3.CreateRevision(macKey), "=", 0),
		},
		// It was never before reserved or has since been freed -> This happens when a service container
		// is started on a different node than the one that was asked for the IPAM IP
		{
			clientv3.Compare(clientv3.CreateRevision(key), "=", 0),
		},
		// It was already reserved for this same MAC -> shouldn't ever happen, reservations are without mac
		{
			clientv3.Compare(clientv3.Value(key), "=", AllocationTypeReserved),
			clientv3.Compare(clientv3.Value(macKey), "=", mac),
		},
		// It was already reserved for this same MAC for a container -> this happens when our plugin is restarted
		{
			clientv3.Compare(clientv3.Value(key), "=", AllocationTypeContainerIP),
			clientv3.Compare(clientv3.Value(macKey), "=", mac),
		},
	}

	return allocateIPByCondition(client, reservedIP, AllocationTypeContainerIP, macKey, mac, conditions)
}

func allocateIPForService(client etcd.Client, reservedIP net.IP, serviceID string) (IPAllocationResult, error) {
	key := allocatedIPKey(client, reservedIP.String())
	serviceIDKey := serviceIDKey(client, reservedIP.String())

	conditions := [][]clientv3.Cmp{
		// It was already reserved, but without a MAC
		// This is the default case
		{
			clientv3.Compare(clientv3.Value(key), "=", AllocationTypeReserved),
			clientv3.Compare(clientv3.CreateRevision(serviceIDKey), "=", 0),
		},
		// It has already been registered as service VIP
		// This happens when our plugin is being restarted
		{
			clientv3.Compare(clientv3.Value(key), "=", AllocationTypeServiceVIP),
			clientv3.Compare(clientv3.Value(serviceIDKey), "=", serviceID),
		},
		// It was never before reserved (this shouldn't happen)
		{
			clientv3.Compare(clientv3.CreateRevision(key), "=", 0),
		},
	}

	return allocateIPByCondition(client, reservedIP, AllocationTypeServiceVIP, serviceIDKey, serviceID, conditions)
}

func reserveIP(client etcd.Client, ip net.IP) (IPAllocationResult, error) {
	key := allocatedIPKey(client, ip.String())

	conditions := [][]clientv3.Cmp{
		// It was never before reserved or has since been freed -> default case
		{
			clientv3.Compare(clientv3.CreateRevision(key), "=", 0),
		},
	}

	return allocateIPByCondition(client, ip, AllocationTypeReserved, "", "", conditions)
}

func allocateIPByCondition(client etcd.Client, ip net.IP, allocationType, dataKey, data string, conditions [][]clientv3.Cmp) (IPAllocationResult, error) {
	key := allocatedIPKey(client, ip.String())

	return etcd.WithConnection(client, func(conn *etcd.Connection) (IPAllocationResult, error) {

		now := time.Now()

		ops := []clientv3.Op{
			clientv3.OpPut(key, allocationType),
			clientv3.OpPut(allocatedAtKey(client, ip.String()), now.Format(time.RFC3339)),
		}

		if dataKey != "" && data != "" {
			ops = append(ops, clientv3.OpPut(dataKey, data))
		}

		for _, condition := range conditions {
			txn := conn.Client.Txn(conn.Ctx).
				If(condition...).
				Then(ops...).
				Else()

			txnResp, err := txn.Commit()
			if err != nil {
				return IPAllocationResult{Success: false}, fmt.Errorf("etcd transaction failed: %v", err)
			}

			if txnResp.Succeeded {
				return IPAllocationResult{Success: true, Allocation: allocation{
					ip:             ip,
					allocationType: allocationType,
					allocatedAt:    now,
					dataKey:        dataKey,
					data:           data,
				}}, nil
			}
		}

		return IPAllocationResult{Success: false}, nil
	})
}

func readAllocation(client etcd.Client, ip string) (*allocation, error) {
	tmp, err := getAllocationsByPrefix(client, allocatedIPKey(client, ip))

	if err != nil {
		return nil, err
	}

	r, has := tmp[ip]
	if !has {
		return nil, nil
	}

	return &r, nil
}

func getAllocationsByPrefix(client etcd.Client, prefix string) (map[string]allocation, error) {
	return etcd.WithConnection(client, func(connection *etcd.Connection) (map[string]allocation, error) {
		resp, err := connection.Client.Get(connection.Ctx, prefix, clientv3.WithPrefix(), clientv3.WithSort(clientv3.SortByKey, clientv3.SortAscend))
		if err != nil {
			return nil, err
		}

		result := make(map[string]allocation)
		for _, kv := range resp.Kvs {
			key := strings.TrimLeft(strings.TrimPrefix(string(kv.Key), prefix), "/")
			value := string(kv.Value)

			if !strings.Contains(key, "/") {
				ip := net.ParseIP(key)

				if ip == nil {
					fmt.Printf("couldn't parse %s as IP. Skipping...\n", key)
					continue
				}

				addOrUpdate(result, key, allocation{
					ip:             ip,
					allocationType: value,
				}, func(existing allocation) {
					existing.ip = ip
					existing.allocationType = value
				})
			} else {
				parts := strings.Split(key, "/")
				if len(parts) == 2 {
					if parts[1] == dataKeyPartMac {
						addOrUpdate(result, key,
							allocation{dataKey: dataKeyPartMac, data: value},
							func(existing allocation) {
								existing.dataKey = dataKeyPartMac
								existing.data = value
							})
					} else if parts[1] == dataKeyPartServiceID {
						addOrUpdate(result, key,
							allocation{dataKey: dataKeyPartServiceID, data: value},
							func(existing allocation) {
								existing.dataKey = dataKeyPartServiceID
								existing.data = value
							})
					} else if parts[1] == allocatedAtKeyPart {
						allocatedAt, err := time.Parse(time.RFC3339, value)
						if err != nil {
							fmt.Printf("Couldn't parse allocated at value '%s' for '%s'. Skipping...\n", value, key)
							continue
						}
						addOrUpdate(result, key, allocation{allocatedAt: allocatedAt},
							func(existing allocation) { existing.allocatedAt = allocatedAt })
					}
				}
			}
		}

		return result, nil
	})
}

func deleteAllAllocations(client etcd.Client) error {
	_, err := etcd.WithConnection(client, func(connection *etcd.Connection) (struct{}, error) {
		_, err := connection.Client.Delete(connection.Ctx, allocatedIPsKey(client), clientv3.WithPrefix())
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
