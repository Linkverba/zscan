package stage

import (
	"encoding/json"
	"fmt"
	"io/ioutil"
	"log"
	"net"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"gopkg.in/yaml.v3"
)

type Config struct {
	TCPPorts []int `yaml:"tcp_ports"`
	UDPPorts []int `yaml:"udp_ports"`
}

type Scanner struct {
	config       Config
	detector     *ServiceDetector
	ipInfo       *IPInfo
	censysClient *CensysClient
	enableGeo    bool
	enableCensys bool
	semaphore    chan struct{}
}

var (
	globalScanner *Scanner
	scannerOnce   sync.Once
	scannerMux    sync.RWMutex
)

func InitGlobalScanner(configPath, pluginsDir string, enableGeo, enableCensys bool, censysAPIKey, censysSecret string) error {
	var initErr error
	scannerOnce.Do(func() {
		var scanner *Scanner
		scanner, initErr = NewScanner(configPath, pluginsDir, enableGeo, enableCensys, censysAPIKey, censysSecret)
		if initErr == nil {
			globalScanner = scanner
		}
	})
	return initErr
}

func GetGlobalScanner() *Scanner {
	scannerMux.RLock()
	defer scannerMux.RUnlock()
	return globalScanner
}

func NewScanner(
	configPath string,
	templatesDir string,
	enableGeo bool,
	enableCensys bool,
	censysAPIKey string,
	censysSecret string,
) (*Scanner, error) {
	config := loadConfig(configPath)

	detector := NewServiceDetector(templatesDir)

	var ipInfo *IPInfo
	if enableGeo {
		var err error
		ipInfo, err = NewIPInfo("data")
		if err != nil {
			log.Printf("Warning: IP information lookup disabled: %v", err)
		}
	}

	var censysClient *CensysClient
	if enableCensys && censysAPIKey != "" && censysSecret != "" {
		censysClient = NewCensysClient(censysAPIKey, censysSecret)
	}

	return &Scanner{
		config:       config,
		detector:     detector,
		ipInfo:       ipInfo,
		censysClient: censysClient,
		enableGeo:    enableGeo,
		enableCensys: enableCensys,
		semaphore:    make(chan struct{}, 10),
	}, nil
}

func (s *Scanner) Close() {
	if s.ipInfo != nil {
		s.ipInfo.Close()
	}
	if s.detector != nil {
		s.detector.Close()
	}
}

func (s *Scanner) Scan(target string) ([]Node, error) {
	targetIP, err := s.parseTarget(target)
	if err != nil {
		return nil, err
	}

	ips := expandCIDR(targetIP)

	var wg sync.WaitGroup
	var zscanResult []Node
	var censysResult []Node
	var censysErr error

	wg.Add(2)

	go func() {
		defer wg.Done()
		zscanResult = s.scanParallel(ips)
	}()

	go func() {
		defer wg.Done()
		if s.enableCensys && s.censysClient != nil {
			censysResult, censysErr = s.censysSearch(ips)
		}
	}()

	wg.Wait()

	if censysErr != nil {
		log.Printf("Warning: Censys search failed: %v", censysErr)
	}

	return s.mergeResults(zscanResult, censysResult), nil
}

func (s *Scanner) censysSearch(ips []string) ([]Node, error) {
	var results []Node
	for _, ip := range ips {
		censysData, err := s.censysClient.GetHostInfo(ip)
		if err != nil {
			log.Printf("Warning: Failed to get Censys data for %s: %v", ip, err)
			break
		}

		node := Node{
			IP:    ip,
			Tags:  []string{},
			Ports: []*ServiceInfo{},
		}

		MergeCensysData(&node, censysData)
		results = append(results, node)

		// 避免触发API限制
		time.Sleep(200 * time.Millisecond)
	}
	return results, nil
}

func (s *Scanner) mergeResults(zscanResults, censysResults []Node) []Node {
	nodeMap := make(map[string]*Node)

	// Add zscan results to map
	for i := range zscanResults {
		node := zscanResults[i]
		nodeMap[node.IP] = &node
	}

	// Merge censys results
	for _, censysNode := range censysResults {
		if existingNode, exists := nodeMap[censysNode.IP]; exists {
			// Merge ports
			for _, port := range censysNode.Ports {
				found := false
				for _, existingPort := range existingNode.Ports {
					if existingPort.Port == port.Port {
						found = true
						break
					}
				}
				if !found {
					existingNode.Ports = append(existingNode.Ports, port)
				}
			}

			// Merge tags
			for _, tag := range censysNode.Tags {
				found := false
				for _, existingTag := range existingNode.Tags {
					if existingTag == tag {
						found = true
						break
					}
				}
				if !found {
					existingNode.Tags = append(existingNode.Tags, tag)
				}
			}
		} else {
			nodeMap[censysNode.IP] = &censysNode
		}
	}

	var finalResults []Node
	for _, node := range nodeMap {
		finalResults = append(finalResults, *node)
	}

	return finalResults
}

func (s *Scanner) parseTarget(target string) (string, error) {
	fmt.Printf("\n[*] Processing target: %s\n", target)

	if strings.HasPrefix(target, "http://") || strings.HasPrefix(target, "https://") {
		u, err := url.Parse(target)
		if err != nil {
			return "", fmt.Errorf("invalid URL: %v", err)
		}
		target = u.Host
		fmt.Printf("[+] Extracted host from URL: %s\n", target)

		if strings.Contains(target, ":") {
			target = strings.Split(target, ":")[0]
			fmt.Printf("[+] Removed port, using: %s\n", target)
		}
	}

	if strings.Contains(target, "/") {
		fmt.Printf("[+] Detected CIDR format\n")
		return target, nil
	}

	if ip := net.ParseIP(target); ip != nil {
		fmt.Printf("[+] Detected IP address format\n")
		return target, nil
	}

	return target, nil
}

func (s *Scanner) scanParallel(ips []string) []Node {
	resultsChan := make(chan *Node, len(ips))
	var wg sync.WaitGroup
	semaphore := make(chan struct{}, 20)

	for _, ip := range ips {
		wg.Add(1)
		go func(target string) {
			defer wg.Done()
			semaphore <- struct{}{}
			defer func() { <-semaphore }()

			if node := s.scanHost(target); node != nil {
				resultsChan <- node
			}
		}(ip)
	}

	go func() {
		wg.Wait()
		close(resultsChan)
	}()

	var results []Node
	for node := range resultsChan {
		results = append(results, *node)
	}

	return results
}

func (s *Scanner) scanHost(target string) *Node {
	resultsChan := make(chan ServiceInfo, len(s.config.TCPPorts)+len(s.config.UDPPorts))
	var wg sync.WaitGroup
	semaphore := make(chan struct{}, 100)

	node := &Node{
		IP:    target,
		Tags:  []string{},
		Ports: []*ServiceInfo{},
	}

	// Handle IP info if enabled
	if s.ipInfo != nil {
		if ipDetails, err := s.ipInfo.GetIPInfo(target); err == nil {
			s.updateNodeWithIPDetails(node, ipDetails)
		}
	}

	// Scan TCP ports
	for _, port := range s.config.TCPPorts {
		wg.Add(1)
		go s.scanTCPPort(target, port, &wg, semaphore, resultsChan)
	}

	// Scan UDP ports
	for _, port := range s.config.UDPPorts {
		wg.Add(1)
		go s.scanUDPPort(target, port, &wg, semaphore, resultsChan)
	}

	go func() {
		wg.Wait()
		close(resultsChan)
	}()

	// Process results
	s.processResults(node, resultsChan)

	if len(node.Ports) > 0 {
		return node
	}
	return nil
}

// Helper functions moved from main.go
func loadConfig(configPath string) Config {
	data, err := ioutil.ReadFile(configPath)
	if err != nil {
		log.Fatalf("Error reading config file: %v", err)
	}

	var config Config
	err = yaml.Unmarshal(data, &config)
	if err != nil {
		log.Fatalf("Error parsing config file: %v", err)
	}

	return config
}

func loadFingerprints(path string) map[string]Fingerprint {
	data, err := os.ReadFile(path)
	if err != nil {
		log.Fatalf("Error reading fingerprints file: %v", err)
	}

	var fingerprints map[string]Fingerprint
	if err := json.Unmarshal(data, &fingerprints); err != nil {
		log.Fatalf("Error parsing fingerprints: %v", err)
	}

	return fingerprints
}

// Additional helper methods for Scanner
func (s *Scanner) scanTCPPort(target string, port int, wg *sync.WaitGroup, semaphore chan struct{}, resultsChan chan ServiceInfo) {
	defer wg.Done()
	semaphore <- struct{}{}
	defer func() { <-semaphore }()

	if ScanTCPPort(target, port) {
		services := s.detector.DetectService(target, port, "tcp")
		for _, service := range services {
			resultsChan <- service
		}
	}
}

func (s *Scanner) scanUDPPort(target string, port int, wg *sync.WaitGroup, semaphore chan struct{}, resultsChan chan ServiceInfo) {
	defer wg.Done()
	semaphore <- struct{}{}
	defer func() { <-semaphore }()

	if ScanUDPPort(target, port) {
		services := s.detector.DetectService(target, port, "udp")
		for _, service := range services {
			resultsChan <- service
		}
	}
}

func (s *Scanner) updateNodeWithIPDetails(node *Node, details *IPDetails) {
	if details == nil {
		return
	}

	node.Continent = details.Continent
	node.ContinentCode = details.ContinentCode
	node.Country = details.Country
	node.CountryCode = details.CountryCode
	node.Region = details.Region
	node.RegionCode = details.RegionCode
	node.City = details.City
	node.PostalCode = details.PostalCode
	node.Latitude = details.Latitude
	node.Longitude = details.Longitude
	node.TimeZone = details.TimeZone
	node.ASN = details.ASN
	node.ASNOrg = details.ASNOrg
	node.ISP = details.ISP
	node.Domain = details.Domain
	node.NetworkType = details.NetworkType
	node.AccuracyRadius = details.AccuracyRadius

	if details.IsAnonymous {
		node.Tags = append(node.Tags, "anonymous")
	}
	if details.IsAnonymousVPN {
		node.Tags = append(node.Tags, "vpn")
	}
	if details.IsHosting {
		node.Tags = append(node.Tags, "hosting")
	}
	if details.IsProxy {
		node.Tags = append(node.Tags, "proxy")
	}
	if details.IsTorExitNode {
		node.Tags = append(node.Tags, "tor_exit")
	}
}

func (s *Scanner) processResults(node *Node, resultsChan chan ServiceInfo) {
	osSet := make(map[string]struct{})
	manufacturerSet := make(map[string]struct{})
	devicetypeSet := make(map[string]struct{})
	sensitiveInfoSet := make(map[string]struct{})

	for result := range resultsChan {
		if len(result.Types) > 0 {
			for _, serviceType := range result.Types {
				if !contains(node.Tags, serviceType) {
					node.Tags = append(node.Tags, serviceType)
				}
			}
		}
		if result.OS != "" {
			osSet[result.OS] = struct{}{}
			node.OS = result.OS
		}
		if result.Manufacturer != "" {
			manufacturerSet[result.Manufacturer] = struct{}{}
			node.Manufacturer = result.Manufacturer
		}
		if result.Devicetype != "" {
			devicetypeSet[result.Devicetype] = struct{}{}
			node.Devicetype = result.Devicetype
		}
		if len(result.SensitiveInfo) > 0 {
			for _, info := range result.SensitiveInfo {
				sensitiveInfoSet[info] = struct{}{}
			}
		}

		node.Ports = append(node.Ports, &result)
	}

	var osList []string
	for os := range osSet {
		osList = append(osList, os)
	}
	node.OS = strings.Join(osList, "/")

	for info := range sensitiveInfoSet {
		node.SensitiveInfo = append(node.SensitiveInfo, info)
	}
}

func expandCIDR(cidr string) []string {
	if !strings.Contains(cidr, "/") {
		return []string{cidr}
	}

	ip, ipnet, err := net.ParseCIDR(cidr)
	if err != nil {
		return []string{cidr}
	}

	var ips []string
	for ip := ip.Mask(ipnet.Mask); ipnet.Contains(ip); inc(ip) {
		ips = append(ips, ip.String())
	}

	// Remove network and broadcast addresses if the network is larger than /31
	if len(ips) > 2 {
		ips = ips[1 : len(ips)-1]
	}

	return ips
}

func inc(ip net.IP) {
	for j := len(ip) - 1; j >= 0; j-- {
		ip[j]++
		if ip[j] > 0 {
			break
		}
	}
}

func findRealIP(domain string) ([]string, error) {
	var ips []string

	// Try to resolve the domain using different record types
	records := []string{"A", "AAAA"}
	for _, recordType := range records {
		switch recordType {
		case "A":
			addrs, err := net.LookupIP(domain)
			if err != nil {
				continue
			}
			for _, addr := range addrs {
				if ipv4 := addr.To4(); ipv4 != nil {
					ips = append(ips, ipv4.String())
				}
			}
		case "AAAA":
			addrs, err := net.LookupIP(domain)
			if err != nil {
				continue
			}
			for _, addr := range addrs {
				if ipv4 := addr.To4(); ipv4 == nil {
					ips = append(ips, addr.String())
				}
			}
		}
	}

	if len(ips) == 0 {
		return nil, fmt.Errorf("no IP addresses found for domain: %s", domain)
	}

	return ips, nil
}
