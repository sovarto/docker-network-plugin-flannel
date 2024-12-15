package flannel_network

import (
	"encoding/json"
	"fmt"
	"github.com/pkg/errors"
	"github.com/samber/lo"
	"github.com/sovarto/FlannelNetworkPlugin/pkg/bridge"
	"github.com/sovarto/FlannelNetworkPlugin/pkg/etcd"
	clientv3 "go.etcd.io/etcd/client/v3"
	"golang.org/x/exp/maps"
	"log"
	"net"
	"strings"
)

type Endpoint interface {
	Join(sandboxKey string) error
	Leave() error
	Delete() error
	GetInfo() endpointInfo
}

type endpointInfo struct {
	ID          string
	IpAddress   net.IP
	MacAddress  string
	VethInside  string
	VethOutside string
}

type endpoint struct {
	id         string
	ipAddress  net.IP
	macAddress string
	vethPair   bridge.VethPair
	sandboxKey string
	etcdClient etcd.Client
	bridge     bridge.BridgeInterface
}

type endpointJson struct {
	IPAddress   string `json:"IPAddress"`
	MacAddress  string `json:"MacAddress"`
	VethInside  string `json:"VethInside"`
	VethOutside string `json:"VethOutside"`
	SandboxKey  string `json:"SandboxKey"`
}

func (e *endpoint) MarshalJSON() ([]byte, error) {
	dataStruct := &endpointJson{
		IPAddress:  e.ipAddress.String(),
		MacAddress: e.macAddress,
		SandboxKey: e.sandboxKey,
	}

	if e.vethPair != nil {
		dataStruct.VethInside = e.vethPair.GetInside()
		dataStruct.VethOutside = e.vethPair.GetOutside()
	}
	return json.Marshal(dataStruct)
}

func endpointKey(client etcd.Client, id string) string {
	return client.GetKey(id)
}

func NewEndpoint(etcdClient etcd.Client, id string, ipAddress net.IP, macAddress string, bridge bridge.BridgeInterface) (Endpoint, error) {
	e := &endpoint{
		id:         id,
		etcdClient: etcdClient,
		ipAddress:  ipAddress,
		macAddress: macAddress,
		bridge:     bridge,
	}

	err := writeToEtcd(e, true)

	if err != nil {
		return nil, errors.WithMessagef(err, "error storing endpoint info for endpoint %s / %s / %s", id, ipAddress, macAddress)
	}

	return e, nil
}

func writeToEtcd(endpoint *endpoint, create bool) error {
	_, err := etcd.WithConnection(endpoint.etcdClient, func(connection *etcd.Connection) (struct{}, error) {

		endpointData, err := json.Marshal(endpoint)
		if err != nil {
			return struct{}{}, errors.WithMessagef(err, "error saving endpoint %s data as JSON", endpoint.id)
		}
		endpointKey := endpointKey(endpoint.etcdClient, endpoint.id)

		if create {
			resp, err := connection.Client.Txn(connection.Ctx).
				If(clientv3.Compare(clientv3.CreateRevision(endpointKey), "=", 0)).
				Then(clientv3.OpPut(endpointKey, string(endpointData))).
				Commit()

			if err != nil {
				return struct{}{}, err
			}

			if !resp.Succeeded {
				return struct{}{}, errors.Errorf("endpoint key already exists: %s", endpointKey)
			}

		} else {
			_, err := connection.Client.Put(connection.Ctx, endpointKey, string(endpointData))
			if err != nil {
				return struct{}{}, err
			}
		}

		return struct{}{}, nil
	})

	return err
}

func (e *endpoint) GetInfo() endpointInfo {
	result := endpointInfo{
		ID:         e.id,
		IpAddress:  e.ipAddress,
		MacAddress: e.macAddress,
	}

	if e.vethPair != nil {
		result.VethInside = e.vethPair.GetInside()
		result.VethOutside = e.vethPair.GetOutside()
	}

	return result
}

func (e *endpoint) Join(sandboxKey string) error {
	vethPair, err := e.bridge.CreateAttachedVethPair(e.macAddress)

	if err != nil {
		return err
	}

	e.vethPair = vethPair

	if err := writeToEtcd(e, false); err != nil {
		return errors.WithMessagef(err, "error storing veth pair for endpoint %s / %s", e.id, e.ipAddress)
	}

	return nil
}

func (e *endpoint) Leave() error {
	if err := e.vethPair.Delete(); err != nil {
		return err
	}

	e.vethPair = nil

	if err := writeToEtcd(e, false); err != nil {
		return errors.WithMessagef(err, "error storing removed veth pair for endpoint %s / %s", e.id, e.ipAddress)
	}

	return nil
}

func (e *endpoint) Delete() error {
	if e.sandboxKey != "" {
		err := e.Leave()
		if err != nil {
			return err
		}
	}
	_, err := etcd.WithConnection(e.etcdClient, func(connection *etcd.Connection) (struct{}, error) {
		endpointData, err := json.Marshal(e)
		if err != nil {
			return struct{}{}, errors.WithMessagef(err, "error saving endpoint %s data as JSON", e.id)
		}

		endpointKey := endpointKey(e.etcdClient, e.id)
		resp, err := connection.Client.Txn(connection.Ctx).
			If(clientv3.Compare(clientv3.Value(endpointKey), "=", string(endpointData))).
			Then(clientv3.OpDelete(endpointKey)).
			Commit()

		if err != nil {
			return struct{}{}, err
		}

		if !resp.Succeeded {
			return struct{}{}, errors.Errorf("endpoint key %s doesn't exist with IP %s", endpointKey, e.ipAddress)
		}

		return struct{}{}, nil
	})

	if err != nil {
		return errors.WithMessagef(err, "error deleting endpoint info for endpoint %s / %s / %s", e.id, e.ipAddress, e.macAddress)
	}

	return nil
}

func loadEndpointsFromEtcd(etcdClient etcd.Client, bridgeInterface bridge.BridgeInterface) ([]Endpoint, error) {
	endpoints, err := etcd.WithConnection(etcdClient, func(connection *etcd.Connection) ([]Endpoint, error) {
		prefix := etcdClient.GetKey()
		resp, err := connection.Client.Get(connection.Ctx, prefix, clientv3.WithPrefix())
		if err != nil {
			return nil, err
		}

		endpoints := make(map[string]*endpoint)

		for _, kv := range resp.Kvs {
			key := strings.TrimLeft(strings.TrimPrefix(string(kv.Key), prefix), "/")
			keyParts := strings.Split(key, "/")
			e, exists := endpoints[keyParts[0]]
			if !exists {
				e = &endpoint{}
				endpoints[keyParts[0]] = e
			}
			if len(keyParts) == 1 {
				jsonData := &endpointJson{}
				if err := json.Unmarshal(kv.Value, jsonData); err != nil {
					log.Printf("error unmarshalling endpoint info for key %s. Ignorin... err: %s", key, err)
					continue
				}
				e.id = keyParts[0]
				e.ipAddress = net.ParseIP(jsonData.IPAddress)
				e.macAddress = jsonData.MacAddress
				e.sandboxKey = jsonData.SandboxKey
				e.vethPair = bridge.HydrateVethPair(jsonData.VethInside, jsonData.VethOutside)
				e.etcdClient = etcdClient
				e.bridge = bridgeInterface
			} else {
				fmt.Printf("Ignoring unknown key %s\n", string(kv.Key))
			}
		}

		return lo.Map(maps.Values(endpoints), func(value *endpoint, index int) Endpoint {
			return value
		}), nil
	})

	if err != nil {
		return nil, errors.WithMessage(err, "error getting endpoints from etcd")
	}

	return endpoints, nil
}
