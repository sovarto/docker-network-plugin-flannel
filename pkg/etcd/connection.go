package etcd

import (
	"context"
	"github.com/pkg/errors"
	clientv3 "go.etcd.io/etcd/client/v3"
	"log"
)

type Connection struct {
	Client *clientv3.Client
	Ctx    context.Context
	Cancel context.CancelFunc
}

func (c *Connection) PutIfNewOrChanged(key string, value string) (wasWritten bool, err error) {
	resp, err := c.Client.Txn(c.Ctx).
		If(clientv3.Compare(clientv3.CreateRevision(key), "=", 0)).
		Then(clientv3.OpPut(key, value)).
		Commit()

	if err != nil {
		return false, errors.WithMessagef(err, "etcd put operation failed for new key %s", key)
	}

	if !resp.Succeeded {
		resp, err = c.Client.Txn(c.Ctx).
			If(clientv3.Compare(clientv3.Value(key), "!=", value)).
			Then(clientv3.OpPut(key, value)).
			Commit()

		if err != nil {
			return false, errors.WithMessagef(err, "etcd put operation failed for new key %s", key)
		}
	}

	return resp.Succeeded, nil
}

func (c *Connection) Close() {
	c.Client.Close()
	if c.Cancel != nil {
		c.Cancel()
	}
}

func WithConnection[T any](client Client, fn func(*Connection) (T, error)) (T, error) {
	connection, err := client.NewConnection(true)
	defer connection.Close()

	if err != nil {
		log.Println("Failed to connect to etcd:", err)
		var zero T
		return zero, err
	}

	return fn(connection)
}
