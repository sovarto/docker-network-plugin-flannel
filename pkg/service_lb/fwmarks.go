package service_lb

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"github.com/pkg/errors"
	"github.com/sovarto/FlannelNetworkPlugin/pkg/etcd"
	clientv3 "go.etcd.io/etcd/client/v3"
	"hash/crc32"
	"log"
	"strconv"
	"sync"
)

type FwmarksManagement interface {
	Get(serviceID, networkID string) (uint32, error)
	Release(serviceID, networkID string, fwmark uint32) error
}

type fwmarks struct {
	etcdClient etcd.Client
	sync.Mutex
}

func fwmarksListKey(client etcd.Client, networkID string) string {
	return client.GetKey(networkID, "list")
}

func fwmarkKey(client etcd.Client, networkID, fwmark string) string {
	return fmt.Sprintf("%s/%s", fwmarksListKey(client, networkID), fwmark)
}

func fwmarkServicesKey(client etcd.Client, networkID string) string {
	return client.GetKey(networkID, "by-service")
}

func fwmarkServiceKey(client etcd.Client, networkID, serviceID string) string {
	return fmt.Sprintf("%s/%s/%s", fwmarkServicesKey(client, networkID), serviceID)
}

func NewFwmarksManagement(etcdClient etcd.Client) FwmarksManagement {
	return &fwmarks{
		etcdClient: etcdClient,
	}
}

func (f *fwmarks) Get(serviceID, networkID string) (uint32, error) {
	f.Lock()
	defer f.Unlock()

	return etcd.WithConnection(f.etcdClient, func(connection *etcd.Connection) (uint32, error) {
		serviceKey := fwmarkServiceKey(f.etcdClient, networkID, serviceID)
		resp, err := connection.Client.Get(connection.Ctx, serviceKey)
		if err != nil {
			return 0, err
		}

		if len(resp.Kvs) > 0 {
			existingFwmark := string(resp.Kvs[0].Value)
			parsedFwmark, err := strconv.ParseUint(existingFwmark, 10, 32)
			if err != nil {
				log.Printf("Failed to parse existing fwmark %s, discarding: %v", existingFwmark, err)
			} else {
				return uint32(parsedFwmark), nil
			}
		}

		listKey := fwmarksListKey(f.etcdClient, networkID)

		for {
			resp, err = connection.Client.Get(connection.Ctx, listKey, clientv3.WithPrefix())
			if err != nil {
				return 0, err
			}

			existingFwmarks := []uint32{}

			for _, kv := range resp.Kvs {
				existingFwmark := string(kv.Value)
				parsedFwmark, err := strconv.ParseUint(existingFwmark, 10, 32)
				if err != nil {
					log.Printf("Failed to parse existing fwmark %s, skipping: %v", existingFwmark, err)
				} else {
					existingFwmarks = append(existingFwmarks, uint32(parsedFwmark))
				}
			}

			// TODO: move everything into namespace so that only our fwmarks exist
			fwmark, err := GenerateFWMARK(serviceID, existingFwmarks)
			if err != nil {
				return 0, err
			}

			fwmarkStr := strconv.FormatUint(uint64(fwmark), 10)
			fwmarkKey := fwmarkKey(f.etcdClient, networkID, fwmarkStr)

			txn := connection.Client.Txn(connection.Ctx).
				If(clientv3.Compare(clientv3.CreateRevision(fwmarkKey), "=", 0)).
				Then(
					clientv3.OpPut(serviceKey, fwmarkStr),
					clientv3.OpPut(fwmarkKey, serviceID),
				)

			txnResp, err := txn.Commit()
			if err != nil {
				return 0, fmt.Errorf("etcd transaction failed: %v", err)
			}

			if !txnResp.Succeeded {
				// this fwmark has since been registered. This shouldn't happen, but let's just try the next
				continue
			}

			fmt.Printf("Created new fwmark %d for service %s\n", fwmark, serviceID)
			return fwmark, nil
		}

	})
}

func (f *fwmarks) Release(serviceID, networkID string, fwmark uint32) error {
	f.Lock()
	defer f.Unlock()
	_, err := etcd.WithConnection(f.etcdClient, func(connection *etcd.Connection) (struct{}, error) {
		fwmarkStr := strconv.FormatUint(uint64(fwmark), 10)
		serviceKey := fwmarkServiceKey(f.etcdClient, networkID, serviceID)
		fwmarkKey := fwmarkKey(f.etcdClient, networkID, fwmarkStr)
		txn := connection.Client.Txn(connection.Ctx).
			If(
				clientv3.Compare(clientv3.Value(serviceKey), "=", fwmarkStr),
				clientv3.Compare(clientv3.Value(fwmarkKey), "=", serviceID),
			).
			Then(
				clientv3.OpDelete(serviceKey),
				clientv3.OpDelete(fwmarkKey),
			)

		resp, err := txn.Commit()
		if err != nil {
			return struct{}{}, fmt.Errorf("etcd transaction failed: %v", err)
		}

		if !resp.Succeeded {
			return struct{}{}, errors.Wrapf(err, "failed to release fwmark %d for service %s", fwmark, serviceID)
		}

		return struct{}{}, nil
	})

	return err
}

// GenerateFWMARK generates a unique FWMARK based on the serviceID.
// It checks against existingFWMARKs and appends a random suffix to the serviceID
// if a collision is detected. It returns the unique FWMARK, the possibly modified
// serviceID, and an error if a unique FWMARK cannot be found within the maximum attempts.
func GenerateFWMARK(serviceID string, existingFWMARKs []uint32) (uint32, error) {
	const maxAttempts = 1000
	const suffixLength = 4 // Number of random bytes to append

	// Convert existingFWMARKs slice to a map for efficient lookup
	fwmarkMap := make(map[uint32]struct{}, len(existingFWMARKs))
	for _, mark := range existingFWMARKs {
		fwmarkMap[mark] = struct{}{}
	}

	currentServiceID := serviceID

	for attempt := 0; attempt < maxAttempts; attempt++ {
		// Generate FWMARK using CRC32 checksum
		fwmark := crc32.ChecksumIEEE([]byte(currentServiceID))

		// Check for collision
		if _, exists := fwmarkMap[fwmark]; !exists {
			// Unique FWMARK found
			return fwmark, nil
		}

		// Collision detected, prepare to modify the serviceID with a random suffix
		suffix, err := generateRandomSuffix(suffixLength)
		if err != nil {
			return 0, fmt.Errorf("failed to generate random suffix: %v", err)
		}

		// Append the random suffix to the original serviceID
		currentServiceID = fmt.Sprintf("%s_%s", serviceID, suffix)
	}

	return 0, errors.New("unable to generate a unique FWMARK after maximum attempts")
}

// generateRandomSuffix creates a random hexadecimal string of length `length` bytes.
func generateRandomSuffix(length int) (string, error) {
	bytes := make([]byte, length)
	_, err := rand.Read(bytes)
	if err != nil {
		return "", err
	}
	return hex.EncodeToString(bytes), nil
}
