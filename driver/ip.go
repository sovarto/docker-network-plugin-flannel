package driver

import (
	"net"
)

func ipsInSubnet(subnet *net.IPNet) []net.IP {
	var ips []net.IP
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

func isLastIP(allIPs []net.IP, reserved map[string]struct{}) bool {
	for _, ip := range allIPs {
		ipStr := ip.String()
		if _, r := reserved[ipStr]; !r {
			return false
		}
	}
	return true
}
