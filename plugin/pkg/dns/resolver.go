package dns

import (
	"fmt"
	"net"
	"sort"
	"strings"
	"sync"

	"github.com/davecgh/go-spew/spew"
	"github.com/miekg/dns"
	"github.com/samber/lo"
	"github.com/sovarto/FlannelNetworkPlugin/pkg/common"
)

type Resolver interface {
	ResolveName(query string, validNetworkIDs []string) []dns.RR
	ResolveIP(query string, validNetworkIDs []string) []dns.RR
	AddNetwork(network common.NetworkInfo)
	RemoveNetwork(network common.NetworkInfo)
	AddContainer(container common.ContainerInfo)
	UpdateContainer(container common.ContainerInfo)
	RemoveContainer(container common.ContainerInfo)
	AddService(service common.Service)
	RemoveService(service common.Service)
}

type containerDNSNameData struct {
	containerID string
	networkID   string
	ip          net.IP
}

type resolver struct {
	networkNameToID map[string]string
	networkIDToName map[string]string
	containerData   map[string][]containerDNSNameData // dns name -> data
	// We only need the service info here, but store the service instance because it's data will always
	// be up-to-date
	serviceData             map[string]common.Service // service name -> data
	dockerCompatibilityMode bool
	ttl                     uint32
	sync.Mutex
}

// NewResolver
// dockerCompatibilityMode: The resolver of the internal docker DNS doesn't return the IP address
// of all possible matches. In fact, it sometimes returns no IP address for a match.
// Here is how docker DNS works:
// - It iterates through the networks of the requesting container, alphabetically
// - For each network, it gets the matching containers and services. If this results in a non-empty list
// it returns that list and is done.
// - If the requested name is ambiguous, because it contains one or multiple dots, it iterates through
// the variations, starting with the variation where the complete string is considered the name and
// no part is considered the network.
// - And for each variation, it then performs the network iteration explained above and returns the results
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
		containerData:           make(map[string][]containerDNSNameData),
		serviceData:             make(map[string]common.Service),
		ttl:                     600,
	}
}

func (r *resolver) AddService(service common.Service) {
	r.Lock()
	defer r.Unlock()

	serviceInfo := service.GetInfo()
	fmt.Printf("Adding service to resolver %+v\n", serviceInfo)
	r.serviceData[serviceInfo.Name] = service
}

func (r *resolver) RemoveService(service common.Service) {
	r.Lock()
	defer r.Unlock()

	serviceInfo := service.GetInfo()
	fmt.Printf("Removing service from resolver %+v\n", serviceInfo)
	delete(r.serviceData, serviceInfo.Name)
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
	for networkID, dnsNames := range container.DNSNames {
		for _, dnsName := range dnsNames {
			add(r.containerData, dnsName, containerDNSNameData{
				networkID:   networkID,
				ip:          container.IPs[networkID],
				containerID: container.ID,
			})
		}
	}
}

func (r *resolver) RemoveContainer(container common.ContainerInfo) {
	r.Lock()
	defer r.Unlock()

	fmt.Printf("Removing container from resolver %+v\n", container)
	for _, dnsNames := range container.DNSNames {
		for _, dnsName := range dnsNames {
			remove(r.containerData, dnsName, func(item containerDNSNameData) bool {
				return item.containerID == container.ID
			})
		}
	}
}

func (r *resolver) UpdateContainer(container common.ContainerInfo) {
	r.Lock()
	defer r.Unlock()

	fmt.Printf("Updating container in resolver %+v\n", container)

	knownDnsNames := make(map[string]map[string]struct{}) // network ID -> dns names

	for dnsName, data := range r.containerData {
		for _, item := range data {
			if item.containerID == container.ID {
				networkData, exists := knownDnsNames[item.networkID]
				if !exists {
					networkData = make(map[string]struct{})
					knownDnsNames[item.networkID] = networkData
				}
				networkData[dnsName] = struct{}{}
			}
		}
	}

	for networkID, dnsNames := range container.DNSNames {
		for _, dnsName := range dnsNames {
			networkData, exists := knownDnsNames[networkID]
			needsToBeAdded := true
			if exists {
				if _, ok := networkData[dnsName]; ok {
					needsToBeAdded = false
					delete(networkData, dnsName)
				}
			}

			if needsToBeAdded {
				add(r.containerData, dnsName, containerDNSNameData{
					networkID:   networkID,
					ip:          container.IPs[networkID],
					containerID: container.ID,
				})
			}
		}
	}

	for networkID, dnsNames := range knownDnsNames {
		for dnsName, _ := range dnsNames {
			remove(r.containerData, dnsName, func(item containerDNSNameData) bool {
				return item.containerID == container.ID && item.networkID == networkID
			})
		}
	}
}

func (r *resolver) ResolveName(query string, validNetworkIDs []string) []dns.RR {
	r.Lock()
	defer r.Unlock()

	queryParts := strings.Split(strings.TrimSuffix(query, "."), ".") // Remove trailing . in queries
	namePartsCount := len(queryParts)

	sortedNetworkIDs := make([]string, len(validNetworkIDs))
	copy(sortedNetworkIDs, validNetworkIDs)
	sort.Slice(sortedNetworkIDs, func(i, j int) bool {
		return r.networkIDToName[validNetworkIDs[i]] < r.networkIDToName[validNetworkIDs[j]]
	})

	result := []net.IP{}

	for i := 0; i < len(queryParts); i++ {
		requestedName := strings.Join(queryParts[:namePartsCount-i], ".")
		requestedNetworkName := strings.Join(queryParts[namePartsCount-i:], ".")

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
	result = lo.Shuffle(lo.UniqBy(result, func(item net.IP) string {
		return item.String()
	}))
	dnsRecords := lo.Map(result, func(item net.IP, index int) dns.RR {
		return &dns.A{
			Hdr: dns.RR_Header{
				Name:   query,
				Rrtype: dns.TypeA,
				Class:  dns.ClassINET,
				Ttl:    r.ttl,
			},
			A: item,
		}
	})

	fmt.Printf("Received request to resolve %s in networks %v. Resolved to %v\n", query, validNetworkIDs, dnsRecords)
	if len(dnsRecords) == 0 {
		fmt.Printf("Available data:\n%s\n", spew.Sdump(r))
	}

	return dnsRecords
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

	return result
}

func (r *resolver) resolveContainerName(requestedName string, validNetworkID string) []net.IP {
	result := []net.IP{}
	dnsNameData, exists := r.containerData[requestedName]
	if exists {
		for _, data := range dnsNameData {
			if validNetworkID == data.networkID {
				result = append(result, data.ip)
			}
		}
	}

	return result
}

func (r *resolver) resolveServiceName(requestedName string, validNetworkID string) []net.IP {
	result := []net.IP{}

	service, exists := r.serviceData[requestedName]
	if exists {
		serviceInfo := service.GetInfo()
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
