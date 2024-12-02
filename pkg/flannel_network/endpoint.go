package flannel_network

import (
	"github.com/pkg/errors"
	"github.com/sovarto/FlannelNetworkPlugin/pkg/bridge"
	"github.com/sovarto/FlannelNetworkPlugin/pkg/etcd"
	clientv3 "go.etcd.io/etcd/client/v3"
	"net"
)

type Endpoint interface {
	Join(sandboxKey string) error
	Leave() error
	Delete() error
	GetInfo() endpointInfo
}

type endpointInfo struct {
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

func endpointKey(client etcd.Client, id string) string {
	return client.GetKey(id)
}

func macKey(client etcd.Client, id string) string {
	return client.GetKey(id, "mac")
}

func NewEndpoint(etcdClient etcd.Client, id string, ipAddress net.IP, macAddress string, bridge bridge.BridgeInterface) (Endpoint, error) {
	_, err := etcd.WithConnection(etcdClient, func(connection *etcd.Connection) (struct{}, error) {
		endpointKey := endpointKey(etcdClient, id)
		macKey := macKey(etcdClient, id)
		resp, err := connection.Client.Txn(connection.Ctx).
			If(clientv3.Compare(clientv3.CreateRevision(endpointKey), "=", 0)).
			Then(
				clientv3.OpPut(endpointKey, ipAddress.String()),
				clientv3.OpPut(macKey, macAddress),
			).
			Commit()

		if err != nil {
			return struct{}{}, err
		}

		if !resp.Succeeded {
			return struct{}{}, errors.Errorf("endpoint key already exists: %s", endpointKey)
		}

		return struct{}{}, nil
	})

	if err != nil {
		return nil, errors.WithMessagef(err, "error storing endpoint info for endpoint %s / %s / %s", id, ipAddress, macAddress)
	}

	return &endpoint{
		id:         id,
		etcdClient: etcdClient,
		ipAddress:  ipAddress,
		macAddress: macAddress,
		bridge:     bridge,
	}, nil
}

func (e *endpoint) GetInfo() endpointInfo {
	return endpointInfo{
		IpAddress:   e.ipAddress,
		MacAddress:  e.macAddress,
		VethInside:  e.vethPair.GetInside(),
		VethOutside: e.vethPair.GetOutside(),
	}
}

func (e *endpoint) Join(sandboxKey string) error {
	vethPair, err := e.bridge.CreateAttachedVethPair(e.macAddress)

	if err != nil {
		return err
	}

	e.vethPair = vethPair

	return nil
}

func (e *endpoint) Leave() error {
	if err := e.vethPair.Delete(); err != nil {
		return err
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
		endpointKey := endpointKey(e.etcdClient, e.id)
		macKey := macKey(e.etcdClient, e.id)
		resp, err := connection.Client.Txn(connection.Ctx).
			If(clientv3.Compare(clientv3.Value(endpointKey), "=", e.ipAddress.String())).
			Then(
				clientv3.OpDelete(endpointKey),
				clientv3.OpDelete(macKey),
			).
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
