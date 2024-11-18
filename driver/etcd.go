package driver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	clientv3 "go.etcd.io/etcd/client/v3"
	"log"
	"net"
	"strings"
	"time"
)

type EtcdClient struct {
	dialTimeout           time.Duration
	prefix                string
	availableSubnets      []string
	defaultHostSubnetSize int
	endpoints             []string
}

type etcdConnection struct {
	client *clientv3.Client
	ctx    context.Context
	cancel context.CancelFunc
}

func (c *etcdConnection) Close() {
	c.client.Close()
	if c.cancel != nil {
		c.cancel()
	}
}

func subnetToKey(subnet string) string {
	return strings.ReplaceAll(subnet, "/", "-")
}

func (e *EtcdClient) networksKey() string {
	return fmt.Sprintf("%s/networks", e.prefix)
}

func (e *EtcdClient) networkKey(subnet string) string {
	return fmt.Sprintf("%s/%s", e.networksKey(), subnetToKey(subnet))
}

func (e *EtcdClient) networkHostSubnetKey(config *FlannelConfig) string {
	return fmt.Sprintf("%s/host-subnets/%s", e.networkKey(config.Network), subnetToKey(config.Subnet))
}

func (e *EtcdClient) reservedIpKey(config *FlannelConfig, ip string) string {
	return fmt.Sprintf("%s/reserved-ips/%s", e.networkHostSubnetKey(config), ip)
}

func (e *EtcdClient) flannelConfigKey(flannelNetworkId string) string {
	return fmt.Sprintf("%s/%s/config", e.prefix, flannelNetworkId)
}

func newEtcdConnection(endpoints []string, dialTimeout time.Duration) (*etcdConnection, error) {
	cli, err := clientv3.New(clientv3.Config{
		Endpoints:   endpoints,
		DialTimeout: dialTimeout,
	})

	if err != nil {
		log.Println("Failed to connect to etcd:", err)
		return &etcdConnection{client: cli, ctx: nil, cancel: nil}, err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)

	return &etcdConnection{client: cli, ctx: ctx, cancel: cancel}, nil
}

func NewEtcdClient(endpoints []string, dialTimeout time.Duration, prefix string, availableSubnets []string, defaultHostSubnetSize int) *EtcdClient {
	return &EtcdClient{
		endpoints:             endpoints,
		dialTimeout:           dialTimeout,
		prefix:                prefix,
		availableSubnets:      availableSubnets,
		defaultHostSubnetSize: defaultHostSubnetSize,
	}
}

func (e *EtcdClient) EnsureFlannelConfig(flannelNetworkId string) (string, error) {
	networkConfigKey := e.flannelConfigKey(flannelNetworkId)

	etcd, err := newEtcdConnection(e.endpoints, e.dialTimeout)
	defer etcd.Close()

	if err != nil {
		log.Println("Failed to connect to etcd:", err)
		return "", err
	}

	subnet, err := e.readExistingNetworkConfig(etcd, networkConfigKey)
	if err == nil {
		return subnet, nil
	}

	for i, subnetCIDR := range e.availableSubnets {
		networkKey := e.networkKey(subnetCIDR)

		configData := Config{
			Network:   subnetCIDR,
			SubnetLen: e.defaultHostSubnetSize,
			Backend: BackendConfig{
				Type: "vxlan",
			},
		}

		// Serialize the configuration to a JSON string
		configBytes, err := json.Marshal(configData)
		if err != nil {
			log.Println("Failed to serialize configuration:", err)
			return "", err
		}

		configString := string(configBytes)

		txn := etcd.client.Txn(etcd.ctx).If(
			clientv3.Compare(clientv3.CreateRevision(networkConfigKey), "=", 0),
			clientv3.Compare(clientv3.CreateRevision(networkKey), "=", 0),
		).Then(
			clientv3.OpPut(networkKey, flannelNetworkId),
			clientv3.OpPut(networkConfigKey, configString),
		)

		resp, err := txn.Commit()
		if err != nil {
			log.Println("Transaction failed:", err)
			return "", err
		}

		if resp.Succeeded {
			log.Printf("Allocated subnet for network %s: %s\n", flannelNetworkId, subnetCIDR)
			if i == len(e.availableSubnets)-1 {
				log.Println("All subnets have been allocated. Cleaning up the ones that have since been released.")
				err = e.cleanupEmptyNetworkKeys(etcd)
				if err != nil {
					return "", err
				}
			}
			return subnetCIDR, nil
		} else {
			log.Println("Config was created by another process.")

			subnet, err := e.readExistingNetworkConfig(etcd, networkConfigKey)
			if err != nil {
				return "", err
			}

			return subnet, nil
		}
	}

	log.Println("No subnets available.")

	return "", errors.New("no subnets available")
}

func (e *EtcdClient) readExistingNetworkConfig(etcd *etcdConnection, networkConfigKey string) (string, error) {
	resp, err := etcd.client.Get(etcd.ctx, networkConfigKey)
	if err != nil {
		log.Printf("Failed to get network config %s:\n%+v", networkConfigKey, err)
		return "", err
	}
	if len(resp.Kvs) > 0 {
		var configData Config
		err := json.Unmarshal(resp.Kvs[0].Value, &configData)
		if err != nil {
			log.Println("Failed to deserialize configuration:", err)
			return "", err
		}

		return configData.Network, nil
	}
	message := fmt.Sprintf("Expected network config '%s' missing", networkConfigKey)
	return "", errors.New(message)
}

func (e *EtcdClient) cleanupEmptyNetworkKeys(etcd *etcdConnection) error {
	resp, err := etcd.client.Get(etcd.ctx, e.networksKey(), clientv3.WithPrefix())
	if err != nil {
		return err
	}

	for _, kv := range resp.Kvs {
		subnetKey := string(kv.Key)
		value := string(kv.Value)
		if value == "" {
			txn := etcd.client.Txn(etcd.ctx).If(
				clientv3.Compare(clientv3.Value(subnetKey), "=", ""),
			).Then(
				clientv3.OpDelete(subnetKey),
			)
			txnResp, err := txn.Commit()
			if err != nil {
				return err
			}
			if !txnResp.Succeeded {
				// The key was modified by another process; skip deletion
				continue
			}
		}
	}
	return nil
}

func (e *EtcdClient) cleanupFreedIPs(etcd *etcdConnection, network *FlannelNetwork) error {
	resp, err := etcd.client.Get(etcd.ctx, e.networkHostSubnetKey(&network.config), clientv3.WithPrefix())
	if err != nil {
		return err
	}

	for _, kv := range resp.Kvs {
		ipKey := string(kv.Key)
		value := string(kv.Value)
		if value == "freed" {
			txn := etcd.client.Txn(etcd.ctx).If(
				clientv3.Compare(clientv3.Value(ipKey), "=", "freed"),
			).Then(
				clientv3.OpDelete(ipKey),
			)
			txnResp, err := txn.Commit()
			if err != nil {
				return err
			}
			if !txnResp.Succeeded {
				continue
			}
			ipKeyParts := strings.Split(ipKey, "/")
			delete(network.reservedAddresses, ipKeyParts[len(ipKeyParts)-1])
		}
	}
	return nil
}

func (e *EtcdClient) ReserveAddress(network *FlannelNetwork) (string, error) {
	_, subnet, err := net.ParseCIDR(network.config.Subnet)
	if err != nil {
		return "", fmt.Errorf("invalid subnet: %v", err)
	}

	etcd, err := newEtcdConnection(e.endpoints, e.dialTimeout)
	defer etcd.Close()

	if err != nil {
		log.Println("Failed to connect to etcd:", err)
		return "", err
	}

	allIPs := ipsInSubnet(subnet)

	for _, ip := range allIPs {
		ipStr := ip.String()

		network.Mutex.Lock()

		if _, reserved := network.reservedAddresses[ipStr]; !reserved {
			reserved, err := e.tryReserveIP(etcd, e.reservedIpKey(&network.config, ipStr))
			if err != nil {
				network.Mutex.Unlock()
				return "", err
			}
			if reserved {
				network.reservedAddresses[ipStr] = struct{}{}

				if isLastIP(allIPs, network.reservedAddresses) {
					if err := e.cleanupFreedIPs(etcd, network); err != nil {
						network.Mutex.Unlock()
						return "", fmt.Errorf("failed to cleanup freed IPs: %v", err)
					}
				}
				network.Mutex.Unlock()
				return ipStr, nil
			}
			// If reservation failed, another thread might have reserved it. Continue to next IP.
		}

		network.Mutex.Unlock()
	}

	return "", errors.New("no available IP addresses to reserve")
}

func (e *EtcdClient) tryReserveIP(etcd *etcdConnection, key string) (bool, error) {
	txn := etcd.client.Txn(etcd.ctx).
		If(clientv3.Compare(clientv3.CreateRevision(key), "=", 0)).
		Then(clientv3.OpPut(key, "reserved")).
		Else()

	txnResp, err := txn.Commit()
	if err != nil {
		return false, fmt.Errorf("etcd transaction failed: %v", err)
	}

	return txnResp.Succeeded, nil
}

func (e *EtcdClient) EnsureGatewayIsMarkedAsReserved(config *FlannelConfig) error {
	etcd, err := newEtcdConnection(e.endpoints, e.dialTimeout)
	defer etcd.Close()
	if err != nil {
		log.Println("Failed to connect to etcd:", err)
		return err
	}

	key := e.reservedIpKey(config, config.Gateway)

	txn := etcd.client.Txn(etcd.ctx).
		If(clientv3.Compare(clientv3.CreateRevision(key), "=", 0)).
		Then(clientv3.OpPut(key, "gateway")).
		Else()

	txnResp, err := txn.Commit()
	if err != nil {
		return fmt.Errorf("etcd transaction failed: %v", err)
	}

	if txnResp.Succeeded {
		return nil
	}

	txn = etcd.client.Txn(etcd.ctx).
		If(clientv3.Compare(clientv3.Value(key), "!=", "reserved")).
		Then(clientv3.OpPut(key, "gateway")).
		Else()
	txnResp, err = txn.Commit()

	if err != nil {
		return fmt.Errorf("etcd transaction failed: %v", err)
	}

	if txnResp.Succeeded {
		// The key was either "gateway" or "freed" and has been set to "gateway".
		// This is considered a success.
		return nil
	} else {
		// The key was "reserved", so the operation failed.
		return fmt.Errorf("gateway IP is already reserved as a normal endpoint IP")
	}
}
