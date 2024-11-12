// parts from https://github.com/KatharaFramework/NetworkPlugin/blob/main/bridge/go-src/src/common_utils.go

package main

import (
	"context"
	"crypto/md5"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/apparentlymart/go-cidr/cidr"
	clientv3 "go.etcd.io/etcd/client/v3"
	"log"
	"net"
	"os"
	"os/exec"
	"strconv"
	"strings"
)

var (
	XTABLES_LOCK_PATH = "/run/xtables.lock"
	IPTABLES_PATH     = "/sbin/iptables"
	IP6TABLES_PATH    = "/sbin/ip6tables"
	NFT_SUFFIX        = "-nft"
	LEGACY_SUFFIX     = "-legacy"
)

type Config struct {
	Network   string        `json:"Network"`
	SubnetLen int           `json:"SubnetLen"`
	Backend   BackendConfig `json:"Backend"`
}

type BackendConfig struct {
	Type string `json:"Type"`
}

func detectIpTables() error {
	useNft := false

	stat, err := os.Stat(XTABLES_LOCK_PATH)
	if err != nil {
		if os.IsNotExist(err) {
			useNft = true
		} else {
			return err
		}
	}

	if stat.IsDir() {
		useNft = true
	}

	ipTablesVersion := ""
	ip6TablesVersion := ""
	if useNft {
		ipTablesVersion = IPTABLES_PATH + NFT_SUFFIX
		ip6TablesVersion = IP6TABLES_PATH + NFT_SUFFIX
	} else {
		ipTablesVersion = IPTABLES_PATH + LEGACY_SUFFIX
		ip6TablesVersion = IP6TABLES_PATH + LEGACY_SUFFIX
	}

	_, err = exec.Command("update-alternatives", "--set", "iptables", ipTablesVersion).CombinedOutput()
	if err != nil {
		return err
	}
	_, err = exec.Command("update-alternatives", "--set", "ip6tables", ip6TablesVersion).CombinedOutput()
	return err
}

func generateMacAddressFromID(macAddressID string) string {
	// Generate an hash from the previous string and truncate it to 6 bytes (48 bits = MAC Length)
	hasher := md5.New()
	hasher.Write([]byte(macAddressID))
	macAddressBytes := hasher.Sum(nil)[:6]

	// Convert the byte array into an hex encoded string separated by `:`
	// This will be the MAC Address of the interface
	macAddressString := []string{}

	for _, element := range macAddressBytes {
		macAddressString = append(macAddressString, fmt.Sprintf("%02x", element))
	}

	// Steps to obtain a locally administered unicast MAC
	// See http://www.noah.org/wiki/MAC_address
	firstByteInt, _ := strconv.ParseInt(macAddressString[0], 16, 32)
	macAddressString[0] = fmt.Sprintf("%02x", (firstByteInt|0x02)&0xfe)

	return strings.Join(macAddressString, ":")
}

func generateAllSubnets(availableSubnets []string, networkSubnetSize int) ([]string, error) {
	var allSubnets []string
	for _, asubnet := range availableSubnets {
		_, ipnet, err := net.ParseCIDR(asubnet)
		if err != nil {
			return nil, err
		}
		ones, _ := ipnet.Mask.Size()
		numSubnets := 1 << uint(networkSubnetSize-ones)
		for i := 0; i < numSubnets; i++ {
			subnet, err := cidr.Subnet(ipnet, networkSubnetSize-ones, i)
			if err != nil {
				return nil, err
			}
			allSubnets = append(allSubnets, subnet.String())
		}
	}
	return allSubnets, nil
}

func allocateSubnetAndCreateFlannelConfig(ctx context.Context, cli *clientv3.Client, etcdPrefix string, networkID string, allSubnets []string, hostSubnetLength int) (string, error) {
	networkConfigKey := fmt.Sprintf("/%s/%s/config", etcdPrefix, networkID)

	resp, err := cli.Get(ctx, networkConfigKey)
	if err != nil {
		fmt.Println("Failed to get key from etcd:", err)
		return "", err
	}

	if len(resp.Kvs) > 0 {
		return "", nil
	}

	for i, subnetCIDR := range allSubnets {
		subnetKey := fmt.Sprintf("/%s/subnets/%s", etcdPrefix, subnetCIDR)

		configData := Config{
			Network:   subnetCIDR,
			SubnetLen: hostSubnetLength,
			Backend: BackendConfig{
				Type: "vxlan",
			},
		}

		// Serialize the configuration to a JSON string
		configBytes, err := json.Marshal(configData)
		if err != nil {
			fmt.Println("Failed to serialize configuration:", err)
			return "", err
		}

		configString := string(configBytes)

		txn := cli.Txn(ctx).If(
			clientv3.Compare(clientv3.CreateRevision(networkConfigKey), "=", 0),
			clientv3.Compare(clientv3.CreateRevision(subnetKey), "=", 0),
		).Then(
			clientv3.OpPut(subnetKey, networkID),
			clientv3.OpPut(networkConfigKey, configString),
		)

		resp, err := txn.Commit()
		if err != nil {
			fmt.Println("Transaction failed:", err)
			return "", err
		}

		if resp.Succeeded {
			fmt.Printf("Allocated subnet for network %s: %s\n", networkID, subnetCIDR)
			if i == len(allSubnets)-1 {
				fmt.Println("All subnets have been allocated. Cleaning up the ones that have since been released.")
				err = cleanupEmptySubnetKeys(ctx, cli, etcdPrefix)
				if err != nil {
					return "", err
				}
			}
			return subnetCIDR, nil
		} else {
			resp, err := cli.Get(ctx, networkConfigKey)
			if err != nil {
				fmt.Println("Failed to get network config:", err)
				return "", err
			}
			if len(resp.Kvs) > 0 {
				fmt.Println("Config was created by another process.")

				var configData Config
				err := json.Unmarshal(resp.Kvs[0].Value, &configData)
				if err != nil {
					fmt.Println("Failed to deserialize configuration:", err)
					return "", err
				}

				return configData.Network, nil
			}
			continue
		}
	}

	fmt.Println("No subnets available.")

	return "", errors.New("no subnets available")
}

func cleanupEmptySubnetKeys(ctx context.Context, cli *clientv3.Client, etcdPrefix string) error {
	prefix := fmt.Sprintf("/%s/subnets/", etcdPrefix)
	resp, err := cli.Get(ctx, prefix, clientv3.WithPrefix())
	if err != nil {
		return err
	}

	for _, kv := range resp.Kvs {
		subnetKey := string(kv.Key)
		value := string(kv.Value)
		if value == "" {
			txn := cli.Txn(ctx).If(
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

// Function to deallocate a subnet
func deallocateSubnet(ctx context.Context, cli *clientv3.Client, etcdPrefix string, networkID, subnetCIDR string) error {
	subnetKey := fmt.Sprintf("/%s/subnets/%s", etcdPrefix, subnetCIDR)
	// Attempt to set the value to empty if it is currently allocated to the network
	txn := cli.Txn(ctx).If(
		clientv3.Compare(clientv3.Value(subnetKey), "=", networkID),
	).Then(
		clientv3.OpPut(subnetKey, ""),
	)
	txnResp, err := txn.Commit()
	if err != nil {
		return err
	}
	if !txnResp.Succeeded {
		return fmt.Errorf("failed to deallocate subnet; it may have been modified")
	}
	return nil
}

func getEnvAsInt(name string, defaultVal int) int {
	if valueStr := os.Getenv(name); valueStr != "" {
		if value, err := strconv.Atoi(valueStr); err == nil {
			return value
		} else {
			log.Printf("Invalid %s, using default value %d: %v", name, defaultVal, err)
		}
	}
	return defaultVal
}
