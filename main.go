package main

import (
	"github.com/apparentlymart/go-cidr/cidr"
	"github.com/sovarto/FlannelNetworkPlugin/driver"
	"log"
	"net"
	"os"
	"strconv"
	"strings"
)

func main() {
	log.Println("Starting Flannel plugin")

	etcdEndPoints := strings.Split(os.Getenv("ETCD_ENDPOINTS"), ",")
	etcdPrefix := strings.TrimRight(os.Getenv("ETCD_PREFIX"), "/")
	defaultFlannelOptions := strings.Split(os.Getenv("DEFAULT_FLANNEL_OPTIONS"), ",")
	availableSubnets := strings.Split(os.Getenv("AVAILABLE_SUBNETS"), ",")
	networkSubnetSize := getEnvAsInt("NETWORK_SUBNET_SIZE", 20)
	defaultHostSubnetSize := getEnvAsInt("DEFAULT_HOST_SUBNET_SIZE", 25)
	allSubnets, err := generateAllSubnets(availableSubnets, networkSubnetSize)

	if err != nil {
		log.Fatalf("ERROR: %s init failed, invalid subnets configuration: %v", "flannel-np", err)
	}

	driver.ServeFlannelDriver(etcdEndPoints, etcdPrefix, defaultFlannelOptions,
		allSubnets,
		defaultHostSubnetSize)
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
