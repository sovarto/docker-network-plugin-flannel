package common

import (
	"net"
	"strings"
)

type NetworkInfo struct {
	FlannelID    string
	MTU          int
	Network      *net.IPNet
	HostSubnet   *net.IPNet
	LocalGateway net.IP
}

func SubnetToKey(subnet string) string {
	return strings.ReplaceAll(subnet, "/", "-")
}
func GetPtrFromMap[K comparable, V any](m map[K]V, key K) *V {
	if val, ok := m[key]; ok {
		return &val
	}
	return nil
}

type Equaler interface {
	Equals(other Equaler) bool
}

func CompareIPMaps(a, b map[string]net.IP) bool {
	if len(a) != len(b) {
		return false
	}

	for key, valA := range a {
		valB, exists := b[key]
		if !exists {
			return false
		}
		if !valA.Equal(valB) {
			return false
		}
	}

	return true
}

type Ordered interface {
	~int | ~int8 | ~int16 | ~int32 | ~int64 |
		~uint | ~uint8 | ~uint16 | ~uint32 | ~uint64 |
		~float32 | ~float64 | ~string
}

// Generic Max function
func Max[T Ordered](a, b T) T {
	if a > b {
		return a
	}
	return b
}
