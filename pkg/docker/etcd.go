package docker

import (
	"fmt"
	"github.com/sovarto/FlannelNetworkPlugin/pkg/etcd"
	clientv3 "go.etcd.io/etcd/client/v3"
	"strings"
)

func (d *data) networksKey() string {
	return d.etcdClient.GetKey("networks")
}

func (d *data) networkKey(id string) string {
	return fmt.Sprintf("%s/%s", d.networksKey(), id)
}

func (d *data) storeNetworkInfo(dockerNetworkID, flannelNetworkID string) error {
	_, err := etcd.WithConnection(d.etcdClient, func(connection *etcd.Connection) (struct{}, error) {
		key := d.networkKey(dockerNetworkID)

		_, err := connection.PutIfNewOrChanged(key, flannelNetworkID)
		if err != nil {
			return struct{}{}, err
		}

		return struct{}{}, nil
	})

	return err
}

func (d *data) deleteNetworkInfo(dockerNetworkID string) error {
	return d.deleteKey(d.networkKey(dockerNetworkID), false)
}

func (d *data) loadNetworkInfo() (map[string]string, error) {
	return etcd.WithConnection(d.etcdClient, func(connection *etcd.Connection) (map[string]string, error) {
		key := d.networksKey()
		resp, err := connection.Client.Get(connection.Ctx, key, clientv3.WithPrefix())
		if err != nil {
			return nil, err
		}

		result := map[string]string{}

		for _, kv := range resp.Kvs {
			dockerNetworkID := strings.TrimLeft(strings.TrimPrefix(string(kv.Key), key), "/")
			flannelNetworkID := string(kv.Value)
			result[dockerNetworkID] = flannelNetworkID
		}

		return result, nil
	})
}

func (d *data) deleteKey(key string, withPrefix bool) error {
	_, err := etcd.WithConnection(d.etcdClient, func(connection *etcd.Connection) (struct{}, error) {
		options := []clientv3.OpOption{}
		if withPrefix {
			options = append(options, clientv3.WithPrefix())
		}
		_, err := connection.Client.Delete(connection.Ctx, key, options...)
		if err != nil {
			return struct{}{}, err
		}

		return struct{}{}, nil
	})

	return err
}
