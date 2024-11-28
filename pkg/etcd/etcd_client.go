package etcd

import (
	"context"
	"fmt"
	clientv3 "go.etcd.io/etcd/client/v3"
	"log"
	"strings"
	"time"
)

type Client interface {
	NewConnection() (*Connection, error)
	GetKey(subKeys ...string) string
	CreateSubClient(subKeys ...string) Client
	GetEndpoints() []string
}

func WithConnection[T any](client Client, fn func(*Connection) (T, error)) (T, error) {
	connection, err := client.NewConnection()
	defer connection.Close()

	if err != nil {
		log.Println("Failed to connect to etcd:", err)
		var zero T
		return zero, err
	}

	return fn(connection)
}

type etcdClient struct {
	dialTimeout time.Duration
	prefix      string
	endpoints   []string
}

type Connection struct {
	Client *clientv3.Client
	Ctx    context.Context
	Cancel context.CancelFunc
}

func (c *Connection) Close() {
	c.Client.Close()
	if c.Cancel != nil {
		c.Cancel()
	}
}

func (c *etcdClient) NewConnection() (*Connection, error) {
	cli, err := clientv3.New(clientv3.Config{
		Endpoints:   c.endpoints,
		DialTimeout: c.dialTimeout,
	})

	if err != nil {
		log.Println("Failed to connect to etcd:", err)
		return &Connection{Client: cli, Ctx: nil, Cancel: nil}, err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)

	return &Connection{Client: cli, Ctx: ctx, Cancel: cancel}, nil
}

// GetKey subKey should not start with a slash
func (c *etcdClient) GetKey(subKeys ...string) string {
	if len(subKeys) == 0 {
		return c.prefix
	}
	return fmt.Sprintf("%s/%s", c.prefix, strings.Join(subKeys, "/"))
}

func (c *etcdClient) GetEndpoints() []string {
	return c.endpoints
}

func (c *etcdClient) CreateSubClient(subKeys ...string) Client {
	return NewEtcdClient(c.endpoints, c.dialTimeout, c.GetKey(subKeys...))
}

func NewEtcdClient(endpoints []string, dialTimeout time.Duration, prefix string) Client {
	return &etcdClient{
		endpoints:   endpoints,
		dialTimeout: dialTimeout,
		prefix:      strings.TrimRight(prefix, "/"),
	}
}
