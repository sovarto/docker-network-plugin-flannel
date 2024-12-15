package ipam

import (
	"fmt"
	"github.com/apparentlymart/go-cidr/cidr"
	"net"
	"sort"
)

func generateAllSubnets(availableSubnets []net.IPNet, poolSize int) ([]net.IPNet, error) {
	var allSubnets []net.IPNet
	for _, availableSubnet := range availableSubnets {
		ones, _ := availableSubnet.Mask.Size()
		numSubnets := 1 << uint(poolSize-ones)
		for i := 0; i < numSubnets; i++ {
			subnet, err := cidr.Subnet(&availableSubnet, poolSize-ones, i)
			if err != nil {
				return nil, err
			}
			allSubnets = append(allSubnets, *subnet)
		}
	}
	return allSubnets, nil
}

func ipsInSubnet(subnet net.IPNet, excludeGateway bool) []net.IP {
	var ips []net.IP
	gatewayIP := nextIP(subnet.IP)
	ip := subnet.IP.Mask(subnet.Mask)
	for {
		ip = nextIP(ip)
		if !subnet.Contains(ip) {
			break
		}
		// Exclude network and broadcast addresses
		if ip.Equal(subnet.IP) {
			continue
		}
		if excludeGateway && ip.Equal(gatewayIP) {
			continue
		}
		ips = append(ips, append(net.IP(nil), ip...))
	}
	return ips
}

func nextIP(ip net.IP) net.IP {
	ip = append(net.IP(nil), ip...)
	for j := len(ip) - 1; j >= 0; j-- {
		ip[j]++
		if ip[j] != 0 {
			break
		}
	}
	return ip
}

func ipToInt(ip net.IP) (uint32, error) {
	ipv4 := ip.To4()
	if ipv4 == nil {
		return 0, fmt.Errorf("invalid IPv4 address: %s", ip.String())
	}
	return uint32(ipv4[0])<<24 | uint32(ipv4[1])<<16 | uint32(ipv4[2])<<8 | uint32(ipv4[3]), nil
}

func getLastReservedIP(reservations map[string]allocation) (net.IP, error) {
	var lastReserved allocation
	var found bool

	for _, res := range reservations {
		if !found || res.allocatedAt.After(lastReserved.allocatedAt) {
			lastReserved = res
			found = true
		}
	}

	if !found {
		return nil, fmt.Errorf("no reservations found")
	}

	return lastReserved.ip, nil
}

func sortIPs(ips []net.IP) ([]net.IP, error) {
	sorted := make([]net.IP, len(ips))
	copy(sorted, ips)

	sort.Slice(sorted, func(i, j int) bool {
		ip1 := sorted[i].To4()
		ip2 := sorted[j].To4()

		if ip1 == nil && ip2 == nil {
			return false
		}
		if ip1 == nil {
			return false
		}
		if ip2 == nil {
			return true
		}

		int1, err1 := ipToInt(ip1)
		int2, err2 := ipToInt(ip2)

		if err1 != nil || err2 != nil {
			// If conversion fails, maintain original order
			return false
		}

		return int1 < int2
	})

	return sorted, nil
}

func getNextAvailableIP(availableUnusedIPs []net.IP, reservedIPs map[string]allocation) (net.IP, error) {
	if len(availableUnusedIPs) == 0 {
		return nil, fmt.Errorf("no available unused IPs")
	}

	// Step 1: Find the last reserved IP
	lastReservedIP, err := getLastReservedIP(reservedIPs)
	if err != nil {
		sortedIPs, sortErr := sortIPs(availableUnusedIPs)
		if sortErr != nil {
			return nil, sortErr
		}
		return sortedIPs[0], nil
	}

	lastReservedInt, err := ipToInt(lastReservedIP)
	if err != nil {
		return nil, err
	}

	// Step 2: Sort the available IPs
	sortedAvailableIPs, err := sortIPs(availableUnusedIPs)
	if err != nil {
		return nil, err
	}

	// Step 3: Iterate through the sorted available IPs to find the first IP > lastReservedIP
	for _, ip := range sortedAvailableIPs {
		if ip == nil {
			continue // Skip invalid IPs
		}
		currentIPInt, err := ipToInt(ip)
		if err != nil {
			continue // Skip non-IPv4 or invalid IPs
		}

		if currentIPInt > lastReservedInt {
			return ip, nil
		}
	}

	// Step 4: If no IP is found after the last reserved IP, wrap around and return the first available IP
	fmt.Println("No available IP found after the last reserved IP. Wrapping around.")
	return sortedAvailableIPs[0], nil
}
