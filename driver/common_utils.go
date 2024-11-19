// parts from https://github.com/KatharaFramework/NetworkPlugin/blob/main/bridge/go-src/src/common_utils.go

package driver

import (
	"context"
	"crypto/md5"
	"fmt"
	clientv3 "go.etcd.io/etcd/client/v3"
	"strconv"
	"strings"
)

type Config struct {
	Network   string        `json:"Network"`
	SubnetLen int           `json:"SubnetLen"`
	Backend   BackendConfig `json:"Backend"`
}

type BackendConfig struct {
	Type string `json:"Type"`
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

// Function to deallocate a subnet
func deallocateSubnet(ctx context.Context, cli *clientv3.Client, etcdPrefix string, networkID, subnetCIDR string) error {
	subnetKey := fmt.Sprintf("%s/subnets/%s", etcdPrefix, subnetCIDR)
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
