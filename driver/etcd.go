package driver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	clientv3 "go.etcd.io/etcd/client/v3"
	"log"
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

func (e *EtcdClient) containersKey(networkSubnet string) string {
	return fmt.Sprintf("%s/containers", e.networkKey(networkSubnet))
}

func (e *EtcdClient) containersByIdKey(networkSubnet string) string {
	return fmt.Sprintf("%s/containers/by-id", e.containersKey(networkSubnet))
}

func (e *EtcdClient) containersByNameKey(networkSubnet string) string {
	return fmt.Sprintf("%s/containers/by-name", e.containersKey(networkSubnet))
}

func (e *EtcdClient) containerByNameKey(networkSubnet string, containerName string) string {
	return fmt.Sprintf("%s/%s", e.containersByNameKey(networkSubnet), containerName)
}

func (e *EtcdClient) containerByIdKey(networkSubnet string, containerId string) string {
	return fmt.Sprintf("%s/%s", e.containersByIdKey(networkSubnet), containerId)
}

func (e *EtcdClient) containerIpByNameKey(networkSubnet string, containerName string) string {
	return fmt.Sprintf("%s/ip", e.containerByNameKey(networkSubnet, containerName))
}

func (e *EtcdClient) containerIpByIdKey(networkSubnet string, containerId string) string {
	return fmt.Sprintf("%s/ip", e.containerByIdKey(networkSubnet, containerId))
}

func (e *EtcdClient) servicesKey(networkSubnet string) string {
	return fmt.Sprintf("%s/services", e.networkKey(networkSubnet))
}

func (e *EtcdClient) serviceKey(networkSubnet string, serviceId string) string {
	return fmt.Sprintf("%s/%s", e.servicesKey(networkSubnet), serviceId)
}

func (e *EtcdClient) serviceInstancesKey(networkSubnet string, serviceId string) string {
	return fmt.Sprintf("%s/instances", e.serviceKey(networkSubnet, serviceId))
}

func (e *EtcdClient) serviceInstanceKey(networkSubnet string, serviceId string, ip string) string {
	return fmt.Sprintf("%s/%s", e.serviceInstancesKey(networkSubnet, serviceId), ip)
}

func (e *EtcdClient) serviceFwmarksKey(networkSubnet string, serviceId string) string {
	return fmt.Sprintf("%s/fwmarks", e.serviceKey(networkSubnet, serviceId))
}

func (e *EtcdClient) serviceFwmarkKey(config *FlannelConfig, serviceId string) string {
	return fmt.Sprintf("%s/%s", e.serviceFwmarksKey(config.Network.String(), serviceId), subnetToKey(config.Subnet.String()))
}

func (e *EtcdClient) serviceVipsKey(networkSubnet string, serviceId string) string {
	return fmt.Sprintf("%s/vips", e.serviceKey(networkSubnet, serviceId))
}

func (e *EtcdClient) serviceVipKey(config *FlannelConfig, serviceId string) string {
	return fmt.Sprintf("%s/%s", e.serviceVipsKey(config.Network.String(), serviceId), subnetToKey(config.Subnet.String()))
}

func (e *EtcdClient) networkHostSubnetKey(config *FlannelConfig) string {
	return fmt.Sprintf("%s/host-subnets/%s", e.networkKey(config.Network.String()), subnetToKey(config.Subnet.String()))
}

func (e *EtcdClient) reservedIpsKey(config *FlannelConfig) string {
	return fmt.Sprintf("%s/reserved-ips", e.networkHostSubnetKey(config))
}

func (e *EtcdClient) reservedIpKey(config *FlannelConfig, ip string) string {
	return fmt.Sprintf("%s/%s", e.reservedIpsKey(config), ip)
}

func (e *EtcdClient) fwmarksKey(config *FlannelConfig) string {
	return fmt.Sprintf("%s/fwmarks", e.networkHostSubnetKey(config))
}

func (e *EtcdClient) fwmarkKey(config *FlannelConfig, fwmark string) string {
	return fmt.Sprintf("%s/%s", e.fwmarksKey(config), fwmark)
}

func (e *EtcdClient) flannelConfigKey(flannelNetworkId string) string {
	return fmt.Sprintf("%s/%s/config", e.prefix, flannelNetworkId)
}

func (e *EtcdClient) macKey(reservedIpKey string) string {
	return fmt.Sprintf("%s/mac", reservedIpKey)
}

func (e *EtcdClient) EnsureFlannelConfig(flannelNetworkId string) (string, error) {
	networkConfigKey := e.flannelConfigKey(flannelNetworkId)

	etcd, err := newEtcdConnection(e.endpoints, e.dialTimeout)
	defer etcd.Close()

	if err != nil {
		log.Println("Failed to connect to etcd:", err)
		return "", err
	}

	subnet, found, err := e.readExistingNetworkConfig(etcd, networkConfigKey)
	if found && err == nil {
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
			fmt.Printf("Allocated subnet for network %s: %s\n", flannelNetworkId, subnetCIDR)
			if i == len(e.availableSubnets)-1 {
				log.Println("All subnets have been allocated. Cleaning up the ones that have since been released.")
				err = e.cleanupEmptyNetworkKeys(etcd)
				if err != nil {
					return "", err
				}
			}
			return subnetCIDR, nil
		} else {
			subnet, found, err := e.readExistingNetworkConfig(etcd, networkConfigKey)
			if found {
				fmt.Println("Config was created by another process. Reusing it.")
				if err != nil {
					return "", err
				}

				return subnet, nil
			} else {
				// Not found means: network config didn't exist, but network key did
				// -> The network/subnet is already registered for a different docker network
			}
		}
	}

	log.Println("No subnets available.")

	return "", errors.New("no subnets available")
}

func (e *EtcdClient) readExistingNetworkConfig(etcd *etcdConnection, networkConfigKey string) (string, bool, error) {
	resp, err := etcd.client.Get(etcd.ctx, networkConfigKey)
	if err != nil {
		log.Printf("Failed to get network config %s:\n%+v", networkConfigKey, err)
		return "", false, err
	}
	if len(resp.Kvs) > 0 {
		var configData Config
		err := json.Unmarshal(resp.Kvs[0].Value, &configData)
		if err != nil {
			log.Println("Failed to deserialize configuration:", err)
			return "", true, err
		}

		return configData.Network, true, nil
	}
	message := fmt.Sprintf("Expected network config '%s' missing", networkConfigKey)
	return "", false, errors.New(message)
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

func (e *EtcdClient) EnsureGatewayIsMarkedAsReserved(config *FlannelConfig) error {
	etcd, err := newEtcdConnection(e.endpoints, e.dialTimeout)
	defer etcd.Close()
	if err != nil {
		log.Println("Failed to connect to etcd:", err)
		return err
	}

	key := e.reservedIpKey(config, config.Gateway.String())

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

//func (e *EtcdClient) LoadNetworks() (map[string]*FlannelNetwork, error) {
//	etcd, err := newEtcdConnection(e.endpoints, e.dialTimeout)
//	defer etcd.Close()
//	if err != nil {
//		log.Println("Failed to connect to etcd:", err)
//		return nil, err
//	}
//
//	prefix := e.networksKey()
//	resp, err := etcd.client.Get(etcd.ctx, prefix, clientv3.WithPrefix())
//	if err != nil {
//		return nil, err
//	}
//
//	resultByNetwork := make(map[string]*FlannelNetwork)
//	result := make(map[string]*FlannelNetwork)
//
//	for _, kv := range resp.Kvs {
//		key := strings.TrimLeft(strings.TrimPrefix(string(kv.Key), prefix), "/")
//		value := string(kv.Value)
//
//		keyParts := strings.Split(key, "/")
//
//		networkSubnet := strings.ReplaceAll(keyParts[0], "-", "/")
//		network, exists := resultByNetwork[networkSubnet]
//		if !exists {
//			network = &FlannelNetwork{
//				reservedAddresses: make(map[string]struct{}),
//			}
//			resultByNetwork[networkSubnet] = network
//		}
//		if len(keyParts) == 1 {
//			result[value] = network
//		}
//	}
//
//	return result, nil
//}

func (e *EtcdClient) RegisterContainer(network *FlannelNetwork, serviceId, serviceName, containerId, containerName, ip string) (bool, error) {
	etcd, err := newEtcdConnection(e.endpoints, e.dialTimeout)
	defer etcd.Close()
	if err != nil {
		log.Println("Failed to connect to etcd:", err)
		return false, err
	}

	ops := []clientv3.Op{
		clientv3.OpPut(e.containerByIdKey(network.config.Network.String(), containerId), containerName),
		clientv3.OpPut(e.containerByNameKey(network.config.Network.String(), containerName), containerId),
		clientv3.OpPut(e.containerIpByIdKey(network.config.Network.String(), containerId), ip),
		clientv3.OpPut(e.containerIpByNameKey(network.config.Network.String(), containerName), ip),
	}

	if serviceName != "" && serviceId != "" {
		ops = append(ops,
			clientv3.OpPut(e.serviceKey(network.config.Network.String(), serviceId), serviceName),
			clientv3.OpPut(e.serviceInstanceKey(network.config.Network.String(), serviceId, ip), serviceName),
		)
	}

	txn := etcd.client.Txn(etcd.ctx).Then(ops...)

	txnResp, err := txn.Commit()
	if err != nil {
		return false, fmt.Errorf("etcd transaction failed: %v", err)
	}

	return txnResp.Succeeded, nil
}

func (e *EtcdClient) EnsureServiceVip(network *FlannelNetwork, serviceID string) (string, error) {
	etcd, err := newEtcdConnection(e.endpoints, e.dialTimeout)
	defer etcd.Close()
	if err != nil {
		log.Println("Failed to connect to etcd:", err)
		return "", err
	}

	key := e.serviceVipKey(&network.config, serviceID)
	resp, err := etcd.client.Get(etcd.ctx, key)
	if err != nil {
		return "", err
	}
	if len(resp.Kvs) > 0 {
		existingVip := string(resp.Kvs[0].Value)
		return existingVip, nil
	}

	vip, err := e.reserveAnyIP(network, etcd, "", true)
	if err != nil {
		log.Printf("Failed to reserve VIP for service %s: %+v\n", serviceID, err)
		return "", err
	}

	// TODO: Switch to network.ID once it exists
	fmt.Printf("Reserved new VIP %s for service %s in network %s\n", vip, serviceID, network.bridgeName)

	return vip, nil
}
