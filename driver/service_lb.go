package driver

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"github.com/moby/ipvs"
	"golang.org/x/sys/unix"
	"hash/crc32"
	"log"
	"net"
)

func getLoadBalancerInterfaceName(flannelNetworkId string) string {
	return getInterfaceName("lb", "_", flannelNetworkId)
}

func ensureLoadBalancerInterface(flannelNetworkId string, network *FlannelNetwork) error {
	interfaceName := getLoadBalancerInterfaceName(flannelNetworkId)

	_, err := ensureInterface(interfaceName, "dummy", network.config.MTU, true)

	if err != nil {
		log.Printf("Error ensuring load balancer interface %s for flannel network ID %s exists", interfaceName, flannelNetworkId)
		return err
	}

	return nil
}

func ensureLoadBalancerConfigurationForService(flannelNetworkId string, network *FlannelNetwork, serviceId string) error {
	err := ensureLoadBalancerInterface(flannelNetworkId, network)
	if err != nil {
		return err
	}

	//configureIPVS(fwmark)
	return nil
}

func configureIPVS(fwmark int, vip string, backendIPs []string) error {
	handle, err := ipvs.New("")
	if err != nil {
		return fmt.Errorf("failed to initialize IPVS handle: %v", err)
	}
	defer handle.Close()

	vipIP := net.ParseIP(vip)
	if vipIP == nil {
		return fmt.Errorf("invalid VIP IP address: %s", vip)
	}

	svc := &ipvs.Service{
		FWMark:    uint32(fwmark),
		SchedName: "rr",
	}

	exists := handle.IsServicePresent(svc)
	if !exists {
		err = handle.NewService(svc)
		if err != nil {
			return fmt.Errorf("failed to create IPVS service: %v", err)
		}
	} else {
		existingSvc, err := handle.GetService(svc)
		if err != nil {
			return fmt.Errorf("failed to get existing IPVS service: %v", err)
		}

		if existingSvc.SchedName != svc.SchedName || existingSvc.Flags != svc.Flags || existingSvc.Timeout != svc.Timeout || existingSvc.PEName != svc.PEName {
			err = handle.UpdateService(svc)
			return fmt.Errorf("failed to update existing IPVS service: %v", err)
		}
	}

	existingDestinations, err := handle.GetDestinations(svc)
	if err != nil {
		return fmt.Errorf("failed to get destinations for IPVS service: %v", err)
	}

	desiredBackends := make(map[string]struct{})
	for _, ipStr := range backendIPs {
		desiredBackends[ipStr] = struct{}{}
	}

	existingBackends := make(map[string]*ipvs.Destination)
	for _, dest := range existingDestinations {
		destIP := dest.Address.String()
		existingBackends[destIP] = dest
	}

	for _, ipStr := range backendIPs {
		if _, found := existingBackends[ipStr]; !found {
			backendIP := net.ParseIP(ipStr)
			if backendIP == nil {
				return fmt.Errorf("invalid backend IP address: %s", ipStr)
			}

			dest := &ipvs.Destination{
				AddressFamily:   unix.AF_INET,
				Address:         backendIP,
				Port:            0, // Port (0 means all ports)
				Weight:          1, // Weight for load balancing
				ConnectionFlags: 0,
			}

			err = handle.NewDestination(svc, dest)
			if err != nil {
				return fmt.Errorf("failed to add destination %s: %v", ipStr, err)
			}
		}
	}

	for ipStr, dest := range existingBackends {
		if _, found := desiredBackends[ipStr]; !found {
			err = handle.DelDestination(svc, dest)
			if err != nil {
				return fmt.Errorf("failed to delete destination %s: %v", ipStr, err)
			}
		}
	}

	return nil
}

// GenerateFWMARK generates a unique FWMARK based on the serviceID.
// It checks against existingFWMARKs and appends a random suffix to the serviceID
// if a collision is detected. It returns the unique FWMARK, the possibly modified
// serviceID, and an error if a unique FWMARK cannot be found within the maximum attempts.
func GenerateFWMARK(serviceID string, existingFWMARKs []uint32) (uint32, string, error) {
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
			return fwmark, currentServiceID, nil
		}

		// Collision detected, prepare to modify the serviceID with a random suffix
		suffix, err := generateRandomSuffix(suffixLength)
		if err != nil {
			return 0, "", fmt.Errorf("failed to generate random suffix: %v", err)
		}

		// Append the random suffix to the original serviceID
		currentServiceID = fmt.Sprintf("%s_%s", serviceID, suffix)
	}

	return 0, "", errors.New("unable to generate a unique FWMARK after maximum attempts")
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
