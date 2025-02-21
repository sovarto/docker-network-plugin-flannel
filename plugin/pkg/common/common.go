package common

import (
	"net"
	"strings"
)

type FlannelNetworkID string
type DockerNetworkID string

type FlannelNetworkInfo struct {
	FlannelID    string
	MTU          int
	Network      *net.IPNet
	HostSubnet   *net.IPNet
	LocalGateway net.IP
}

type NetworkInfo struct {
	DockerID  string `json:"DockerID"`
	FlannelID string `json:"FlannelID"`
	Subnet    string `json:"Subnet"`
	Name      string `json:"Name"`
}

func (n NetworkInfo) IsFlannelNetwork() bool { return n.FlannelID != "" }

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
	if a == nil && b == nil {
		return true
	}
	if a == nil || b == nil {
		return false
	}
	if len(a) != len(b) {
		return false
	}

	for key, valA := range a {
		valB, exists := b[key]
		if !exists || !valA.Equal(valB) {
			return false
		}
	}

	return true
}

func CompareStringMaps(a, b map[string]string) bool {
	if a == nil && b == nil {
		return true
	}
	if a == nil || b == nil {
		return false
	}
	if len(a) != len(b) {
		return false
	}

	for key, valA := range a {
		valB, exists := b[key]
		if !exists || valA != valB {
			return false
		}
	}

	return true
}

func CompareStringArrayMaps(a, b map[string][]string) bool {
	if a == nil && b == nil {
		return true
	}
	if a == nil || b == nil {
		return false
	}
	if len(a) != len(b) {
		return false
	}

	for key, valA := range a {
		valB, exists := b[key]
		if !exists || !compareStringSlices(valA, valB) {
			return false
		}
	}

	return true
}

// Helper function to compare two string slices
func compareStringSlices(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
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

func (n NetworkInfo) Equals(other Equaler) bool {
	o, ok := other.(NetworkInfo)
	if !ok {
		return false
	}
	if n.FlannelID != o.FlannelID || n.Name != o.Name {
		return false
	}

	return true
}

func AddOrUpdate[T any](store map[string]T, id string, valueToAdd T, update func(existing *T)) {
	existing, exists := store[id]
	if exists {
		if update != nil {
			update(&existing)
		}
	} else {
		existing = valueToAdd
	}
	store[id] = existing
}
