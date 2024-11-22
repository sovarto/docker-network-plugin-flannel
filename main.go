package main

import (
	"fmt"
	"github.com/apparentlymart/go-cidr/cidr"
	"github.com/sovarto/FlannelNetworkPlugin/driver"
	"log"
	"net"
	"os"
	"strconv"
	"strings"
)

func main() {
	fmt.Println("Starting Flannel plugin...")

	fmt.Println("Command-line arguments:")
	for idx, arg := range os.Args {
		fmt.Printf("Arg %d: %s\n", idx, arg)
	}

	fmt.Println("Environment variables:")
	for _, env := range os.Environ() {
		fmt.Println(env)
	}

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

	fmt.Println("Flannel plugin is ready")
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
