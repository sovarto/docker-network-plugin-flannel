package dns

import (
	"fmt"
	"github.com/miekg/dns"
	"github.com/samber/lo"
	"github.com/sovarto/FlannelNetworkPlugin/pkg/common"
	"net"
	"sort"
	"strings"
	"sync"
)

type Resolver interface {
	ResolveName(query string, validNetworkIDs []string) []dns.RR
	ResolveIP(query string, validNetworkIDs []string) []dns.RR
	AddNetwork(network common.NetworkInfo)
	RemoveNetwork(network common.NetworkInfo)
	AddContainer(container common.ContainerInfo)
	RemoveContainer(container common.ContainerInfo)
	AddService(service common.Service)
	RemoveService(service common.Service)
}

// We only need the service info here, but store the service instance because it's data will always
// be up-to-date
type serviceData struct {
	byName map[string]common.Service
	byID   map[string]common.Service
}

type containerAliasData struct {
	containerID string
	networkID   string
	ip          net.IP
}

type containerData struct {
	byName  map[string][]common.ContainerInfo // container names are only unique on a single node, but not in a cluster
	byAlias map[string][]containerAliasData   // alias aren't unique, not even on a single node
	byID    map[string]common.ContainerInfo
}
type resolver struct {
	networkNameToID         map[string]string
	networkIDToName         map[string]string
	containerData           containerData
	serviceData             serviceData
	dockerCompatibilityMode bool
	ttl                     uint32
	sync.Mutex
}

// NewResolver
// dockerCompatibilityMode: The resolver of the internal docker DNS doesn't return the IP address
// of all possible matches. In fact, it sometimes returns no IP address for a match.
// Here is how docker DNS works: It iterates through the networks of the requesting container, alphabetically
// For each network, it gets the matching containers and services. If this results in a non-empty list
// it returns that list and is done.
// If the requested name is ambiguous, because it contains one or multiple dots, it iterates through
// the variations, starting with the variation where the complete string is considered the name and
// no part is considered the network.
// And for each variation, it then performs the network iteration explained above and returns the results
// as soon as there are some.
// Scenarios:
// In all scenarios:
// - Networks "web" and "internal".
// - The container performing the lookup is in both networks
// - Service with endpoint mode "VIP" and name "my-name", connected to both networks
// - Service with endpoint mode "DNSRR" and name "my-alias", connected to both networks
// Scenario 1
// - Container "my-name", connected to both networks, with an alias "my-alias" on both networks
// Example 1: nslookup my-name -> returns the IP of container "my-name" and the VIP of service "my-name",
// both IPs from the "internal" network. The returned name is "my-name"
// Example 2: nslookup my-name.internal -> same IPs returned, but the name is "my-name.internal"
// Example 3: nslookup my-name.web -> returned name "my-name.web" and the IP and VIP of the web network
// Example 4: nslookup my-alias -> returns the IP of container "my-name" and all container IPs of the
// containers of service "my-alias". All IPs are from the internal network
// Scenario 2
// - Container "my-name", connected to web network only
// Example 5: nslookup my-name -> returns only the VIP of the my-name service from the internal network.
// Scenario 3
// - Container "my-name.internal", connected to web network only
// Example 6: nslookup my-name.internal -> returns only the IP of the my-name.internal container from the web network
// If dockerCompatibilityMode is false, the results will be a bit more deterministic:
// We still go through the variations same as docker, so one level will shadow the other levels
// However, on a given level, we return all IPs of all matches:
// Example 1: We get the IPs and VIPs from both networks
// Example 2: We get the IPs and VIPs from both networks
// Example 3: Same as Docker
// Example 4: We get IPs from both networks
// Example 5: We get the VIP from both networks and the container IP from the web network
// Example 6: Same as Docker
// TODO: Add tests
func NewResolver(dockerCompatibilityMode bool) Resolver {
	return &resolver{
		networkNameToID:         make(map[string]string),
		networkIDToName:         make(map[string]string),
		dockerCompatibilityMode: dockerCompatibilityMode,
		containerData: containerData{
			byAlias: make(map[string][]containerAliasData),
			byID:    make(map[string]common.ContainerInfo),
			byName:  make(map[string][]common.ContainerInfo),
		},
		serviceData: serviceData{
			byName: make(map[string]common.Service),
			byID:   make(map[string]common.Service),
		},
		ttl: 600,
	}
}

func (r *resolver) AddService(service common.Service) {
	r.Lock()
	defer r.Unlock()

	serviceInfo := service.GetInfo()
	fmt.Printf("Adding service to resolver %+v\n", serviceInfo)
	r.serviceData.byName[serviceInfo.Name] = service
	r.serviceData.byID[serviceInfo.ID] = service
}

func (r *resolver) RemoveService(service common.Service) {
	r.Lock()
	defer r.Unlock()

	serviceInfo := service.GetInfo()
	fmt.Printf("Adding service from resolver %+v\n", serviceInfo)
	delete(r.serviceData.byName, serviceInfo.Name)
	delete(r.serviceData.byID, serviceInfo.ID)
}

func (r *resolver) AddNetwork(network common.NetworkInfo) {
	r.Lock()
	defer r.Unlock()

	fmt.Printf("Adding network to resolver %+v\n", network)
	r.networkNameToID[network.Name] = network.DockerID
	r.networkIDToName[network.DockerID] = network.Name
}

func (r *resolver) RemoveNetwork(network common.NetworkInfo) {
	r.Lock()
	defer r.Unlock()

	fmt.Printf("Removing network from resolver %+v\n", network)
	delete(r.networkNameToID, network.DockerID)
	delete(r.networkIDToName, network.Name)
}

func (r *resolver) AddContainer(container common.ContainerInfo) {
	r.Lock()
	defer r.Unlock()

	fmt.Printf("Adding container to resolver %+v\n", container)
	r.containerData.byID[container.ID] = container
	add(r.containerData.byName, container.Name, container)
	for networkID, alias := range container.Aliases {
		add(r.containerData.byAlias, alias, containerAliasData{
			networkID:   networkID,
			ip:          container.IPs[networkID],
			containerID: container.ID,
		})
	}
}

func (r *resolver) RemoveContainer(container common.ContainerInfo) {
	r.Lock()
	defer r.Unlock()

	fmt.Printf("Removing container from resolver %+v\n", container)
	delete(r.containerData.byID, container.ID)
	for _, alias := range container.Aliases {
		remove(r.containerData.byAlias, alias, func(item containerAliasData) bool {
			return item.containerID == container.ID
		})
	}
	remove(r.containerData.byName, container.Name, func(item common.ContainerInfo) bool {
		return item.ID == container.ID
	})
}

func (r *resolver) ResolveName(query string, validNetworkIDs []string) []dns.RR {
	r.Lock()
	defer r.Unlock()

	fmt.Printf("Received request to resolve %s in networks %v\n", query, validNetworkIDs)
	fmt.Printf("service data: %+v\n", r.serviceData)
	fmt.Printf("container data: %+v\n", r.containerData)
	fmt.Printf("network data ID -> name: %+v\n", r.networkIDToName)
	fmt.Printf("network data name -> ID: %+v\n", r.networkNameToID)

	queryParts := strings.Split(strings.TrimSuffix(query, "."), ".") // Remove trailing . in queries
	namePartsCount := len(queryParts)

	sortedNetworkIDs := make([]string, len(validNetworkIDs))
	copy(sortedNetworkIDs, validNetworkIDs)
	sort.Slice(sortedNetworkIDs, func(i, j int) bool {
		return r.networkIDToName[validNetworkIDs[i]] < r.networkIDToName[validNetworkIDs[j]]
	})

	result := []net.IP{}

	for i := 0; i < len(queryParts); i++ {
		requestedName := strings.Join(queryParts[:namePartsCount], ".")
		requestedNetworkName := strings.Join(queryParts[namePartsCount:], ".")

		for _, networkID := range sortedNetworkIDs {
			result = append(result, r.resolveName(requestedName, requestedNetworkName, networkID)...)
			if r.dockerCompatibilityMode && len(result) > 0 {
				break
			}
		}

		if len(result) > 0 {
			break
		}
	}

	return lo.Map(result, func(item net.IP, index int) dns.RR {
		return &dns.A{
			Hdr: dns.RR_Header{
				Name:     query,
				Rrtype:   dns.TypeA,
				Class:    dns.ClassINET,
				Ttl:      r.ttl,
				Rdlength: 0,
			},
			A: item,
		}
	})
}

func (r *resolver) ResolveIP(query string, validNetworkIDs []string) []dns.RR {
	r.Lock()
	defer r.Unlock()

	// TODO: Implement
	return []dns.RR{}
}

// resolveName returns the IP of the first valid network alphabetically speaking for a matching container
// and the VIP of the first valid network for a matching service with endpoint mode "vip"
// If the endpoint mode is "dnsrr" it returns the IP of the first valid network for each container
// of the matching service
func (r *resolver) resolveName(requestedName string, requestedNetworkName string, validNetworkID string) []net.IP {
	result := []net.IP{}
	if requestedNetworkName != "" {
		networkID, exists := r.networkNameToID[requestedNetworkName]
		if !exists {
			fmt.Printf("Network Name %s does not exist\n", requestedNetworkName)
			// No network with this name exists, so there can't be a match
			return result
		}
		if validNetworkID != networkID {
			fmt.Printf("Network ID %s is not valid. Expected: %s\n", networkID, validNetworkID)
			// While the network exists, it's not the valid network, so there can't be a match
			return result
		}
	}

	result = append(result, r.resolveServiceName(requestedName, validNetworkID)...)
	result = append(result, r.resolveContainerName(requestedName, validNetworkID)...)

	return lo.Shuffle(lo.UniqBy(result, func(item net.IP) string {
		return item.String()
	}))
}

func (r *resolver) resolveContainerName(requestedName string, validNetworkID string) []net.IP {
	fmt.Sprintf("resolving container name %s in network %s\n", requestedName, validNetworkID)
	result := []net.IP{}
	aliasData, exists := r.containerData.byAlias[requestedName]
	if exists {
		for _, data := range aliasData {
			if validNetworkID == data.networkID {
				result = append(result, data.ip)
			}
		}
	}

	containers, exists := r.containerData.byName[requestedName]
	if exists {
		for _, container := range containers {
			result = append(result, filterIPsByNetwork(container.IPs, validNetworkID)...)
		}
	}

	return result
}

func (r *resolver) resolveServiceName(requestedName string, validNetworkID string) []net.IP {
	fmt.Printf("resolving service name %s in network %s\n", requestedName, validNetworkID)
	result := []net.IP{}

	service, exists := r.serviceData.byName[requestedName]
	if exists {
		serviceInfo := service.GetInfo()
		fmt.Printf("service exists: %+v\n", serviceInfo)
		if serviceInfo.EndpointMode == common.ServiceEndpointModeVip {
			result = append(result, filterIPsByNetwork(serviceInfo.VIPs, validNetworkID)...)
		} else if serviceInfo.EndpointMode == common.ServiceEndpointModeDnsrr {
			for _, container := range serviceInfo.Containers {
				result = append(result, filterIPsByNetwork(container.IPs, validNetworkID)...)
			}
		}
	}
	return result
}

func filterIPsByNetwork(ips map[string]net.IP, validNetworkID string) []net.IP {
	fmt.Printf("filtering IPs %v by network %s\n", ips, validNetworkID)
	result := []net.IP{}
	for networkID, ip := range ips {
		if validNetworkID == networkID {
			result = append(result, ip)
		}
	}

	return result
}

func add[T any](itemsMap map[string][]T, key string, item T) {
	items, exists := itemsMap[key]
	if !exists {
		items = make([]T, 0)
	}
	items = append(items, item)
	itemsMap[key] = items
}

func remove[T any](itemsMap map[string][]T, key string, predicate func(item T) bool) {
	items, exists := itemsMap[key]
	if exists {
		items = lo.Filter(items, func(item T, index int) bool {
			return !predicate(item)
		})
	}
	if len(items) == 0 {
		delete(itemsMap, key)
	} else {
		itemsMap[key] = items
	}
}
