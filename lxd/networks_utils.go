package main

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"io/ioutil"
	"math"
	"math/big"
	"math/rand"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/gopacket"
	"github.com/google/gopacket/layers"
	"golang.org/x/net/context"
	"golang.org/x/sys/unix"

	"github.com/lxc/lxd/lxd/db"
	"github.com/lxc/lxd/lxd/state"
	"github.com/lxc/lxd/lxd/task"
	"github.com/lxc/lxd/shared"
	"github.com/lxc/lxd/shared/api"
	"github.com/lxc/lxd/shared/logger"
	"github.com/lxc/lxd/shared/version"
)

var networkStaticLock sync.Mutex
var forkdnsServersLock sync.Mutex

func networkAutoAttach(cluster *db.Cluster, devName string) error {
	_, dbInfo, err := cluster.NetworkGetInterface(devName)
	if err != nil {
		// No match found, move on
		return nil
	}

	return networkAttachInterface(dbInfo.Name, devName)
}

func networkAttachInterface(netName string, devName string) error {
	if shared.PathExists(fmt.Sprintf("/sys/class/net/%s/bridge", netName)) {
		_, err := shared.RunCommand("ip", "link", "set", "dev", devName, "master", netName)
		if err != nil {
			return err
		}
	} else {
		_, err := shared.RunCommand("ovs-vsctl", "port-to-br", devName)
		if err != nil {
			_, err := shared.RunCommand("ovs-vsctl", "add-port", netName, devName)
			if err != nil {
				return err
			}
		}
	}

	return nil
}

func networkDetachInterface(netName string, devName string) error {
	if shared.PathExists(fmt.Sprintf("/sys/class/net/%s/bridge", netName)) {
		_, err := shared.RunCommand("ip", "link", "set", "dev", devName, "nomaster")
		if err != nil {
			return err
		}
	} else {
		_, err := shared.RunCommand("ovs-vsctl", "port-to-br", devName)
		if err == nil {
			_, err := shared.RunCommand("ovs-vsctl", "del-port", netName, devName)
			if err != nil {
				return err
			}
		}
	}

	return nil
}

func networkGetInterfaces(cluster *db.Cluster) ([]string, error) {
	networks, err := cluster.Networks()
	if err != nil {
		return nil, err
	}

	ifaces, err := net.Interfaces()
	if err != nil {
		return nil, err
	}

	for _, iface := range ifaces {
		// Ignore veth pairs (for performance reasons)
		if strings.HasPrefix(iface.Name, "veth") {
			continue
		}

		// Append to the list
		if !shared.StringInSlice(iface.Name, networks) {
			networks = append(networks, iface.Name)
		}
	}

	return networks, nil
}

func networkIsInUse(c container, name string) bool {
	for _, d := range c.ExpandedDevices() {
		if d["type"] != "nic" {
			continue
		}

		if !shared.StringInSlice(d["nictype"], []string{"bridged", "macvlan", "ipvlan", "physical", "sriov"}) {
			continue
		}

		if d["parent"] == "" {
			continue
		}

		if networkGetHostDevice(d["parent"], d["vlan"]) == name {
			return true
		}
	}

	return false
}

func networkGetHostDevice(parent string, vlan string) string {
	// If no VLAN, just use the raw device
	if vlan == "" {
		return parent
	}

	// If no VLANs are configured, use the default pattern
	defaultVlan := fmt.Sprintf("%s.%s", parent, vlan)
	if !shared.PathExists("/proc/net/vlan/config") {
		return defaultVlan
	}

	// Look for an existing VLAN
	f, err := os.Open("/proc/net/vlan/config")
	if err != nil {
		return defaultVlan
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		// Only grab the lines we're interested in
		s := strings.Split(scanner.Text(), "|")
		if len(s) != 3 {
			continue
		}

		vlanIface := strings.TrimSpace(s[0])
		vlanId := strings.TrimSpace(s[1])
		vlanParent := strings.TrimSpace(s[2])

		if vlanParent == parent && vlanId == vlan {
			return vlanIface
		}
	}

	// Return the default pattern
	return defaultVlan
}

func networkGetIP(subnet *net.IPNet, host int64) net.IP {
	// Convert IP to a big int
	bigIP := big.NewInt(0)
	bigIP.SetBytes(subnet.IP.To16())

	// Deal with negative offsets
	bigHost := big.NewInt(host)
	bigCount := big.NewInt(host)
	if host < 0 {
		mask, size := subnet.Mask.Size()

		bigHosts := big.NewFloat(0)
		bigHosts.SetFloat64((math.Pow(2, float64(size-mask))))
		bigHostsInt, _ := bigHosts.Int(nil)

		bigCount.Set(bigHostsInt)
		bigCount.Add(bigCount, bigHost)
	}

	// Get the new IP int
	bigIP.Add(bigIP, bigCount)

	// Generate an IPv6
	if subnet.IP.To4() == nil {
		newIp := bigIP.Bytes()
		return newIp
	}

	// Generate an IPv4
	newIp := make(net.IP, 4)
	binary.BigEndian.PutUint32(newIp, uint32(bigIP.Int64()))
	return newIp
}

func networkGetTunnels(config map[string]string) []string {
	tunnels := []string{}

	for k := range config {
		if !strings.HasPrefix(k, "tunnel.") {
			continue
		}

		fields := strings.Split(k, ".")
		if !shared.StringInSlice(fields[1], tunnels) {
			tunnels = append(tunnels, fields[1])
		}
	}

	return tunnels
}

func networkPingSubnet(subnet *net.IPNet) bool {
	var fail bool
	var failLock sync.Mutex
	var wgChecks sync.WaitGroup

	ping := func(ip net.IP) {
		defer wgChecks.Done()

		cmd := "ping"
		if ip.To4() == nil {
			cmd = "ping6"
		}

		_, err := shared.RunCommand(cmd, "-n", "-q", ip.String(), "-c", "1", "-W", "1")
		if err != nil {
			// Remote didn't answer
			return
		}

		// Remote answered
		failLock.Lock()
		fail = true
		failLock.Unlock()
	}

	poke := func(ip net.IP) {
		defer wgChecks.Done()

		addr := fmt.Sprintf("%s:22", ip.String())
		if ip.To4() == nil {
			addr = fmt.Sprintf("[%s]:22", ip.String())
		}

		_, err := net.DialTimeout("tcp", addr, time.Second)
		if err == nil {
			// Remote answered
			failLock.Lock()
			fail = true
			failLock.Unlock()
			return
		}
	}

	// Ping first IP
	wgChecks.Add(1)
	go ping(networkGetIP(subnet, 1))

	// Poke port on first IP
	wgChecks.Add(1)
	go poke(networkGetIP(subnet, 1))

	// Ping check
	if subnet.IP.To4() != nil {
		// Ping last IP
		wgChecks.Add(1)
		go ping(networkGetIP(subnet, -2))

		// Poke port on last IP
		wgChecks.Add(1)
		go poke(networkGetIP(subnet, -2))
	}

	wgChecks.Wait()

	return fail
}

func networkInRoutingTable(subnet *net.IPNet) bool {
	filename := "route"
	if subnet.IP.To4() == nil {
		filename = "ipv6_route"
	}

	file, err := os.Open(fmt.Sprintf("/proc/net/%s", filename))
	if err != nil {
		return false
	}
	defer file.Close()

	scanner := bufio.NewReader(file)
	for {
		line, _, err := scanner.ReadLine()
		if err != nil {
			break
		}

		fields := strings.Fields(string(line))

		// Get the IP
		var ip net.IP
		if filename == "ipv6_route" {
			ip, err = hex.DecodeString(fields[0])
			if err != nil {
				continue
			}
		} else {
			bytes, err := hex.DecodeString(fields[1])
			if err != nil {
				continue
			}

			ip = net.IPv4(bytes[3], bytes[2], bytes[1], bytes[0])
		}

		// Get the mask
		var mask net.IPMask
		if filename == "ipv6_route" {
			size, err := strconv.ParseInt(fmt.Sprintf("0x%s", fields[1]), 0, 64)
			if err != nil {
				continue
			}

			mask = net.CIDRMask(int(size), 128)
		} else {
			bytes, err := hex.DecodeString(fields[7])
			if err != nil {
				continue
			}

			mask = net.IPv4Mask(bytes[3], bytes[2], bytes[1], bytes[0])
		}

		// Generate a new network
		lineNet := net.IPNet{IP: ip, Mask: mask}

		// Ignore default gateway
		if lineNet.IP.Equal(net.ParseIP("::")) {
			continue
		}

		if lineNet.IP.Equal(net.ParseIP("0.0.0.0")) {
			continue
		}

		// Check if we have a route to our new subnet
		if lineNet.Contains(subnet.IP) {
			return true
		}
	}

	return false
}

func networkRandomSubnetV4() (string, error) {
	for i := 0; i < 100; i++ {
		cidr := fmt.Sprintf("10.%d.%d.1/24", rand.Intn(255), rand.Intn(255))
		_, subnet, err := net.ParseCIDR(cidr)
		if err != nil {
			continue
		}

		if networkInRoutingTable(subnet) {
			continue
		}

		if networkPingSubnet(subnet) {
			continue
		}

		return cidr, nil
	}

	return "", fmt.Errorf("Failed to automatically find an unused IPv4 subnet, manual configuration required")
}

func networkRandomSubnetV6() (string, error) {
	for i := 0; i < 100; i++ {
		cidr := fmt.Sprintf("fd42:%x:%x:%x::1/64", rand.Intn(65535), rand.Intn(65535), rand.Intn(65535))
		_, subnet, err := net.ParseCIDR(cidr)
		if err != nil {
			continue
		}

		if networkInRoutingTable(subnet) {
			continue
		}

		if networkPingSubnet(subnet) {
			continue
		}

		return cidr, nil
	}

	return "", fmt.Errorf("Failed to automatically find an unused IPv6 subnet, manual configuration required")
}

func networkDefaultGatewaySubnetV4() (*net.IPNet, string, error) {
	file, err := os.Open("/proc/net/route")
	if err != nil {
		return nil, "", err
	}
	defer file.Close()

	ifaceName := ""

	scanner := bufio.NewReader(file)
	for {
		line, _, err := scanner.ReadLine()
		if err != nil {
			break
		}

		fields := strings.Fields(string(line))

		if fields[1] == "00000000" && fields[7] == "00000000" {
			ifaceName = fields[0]
			break
		}
	}

	if ifaceName == "" {
		return nil, "", fmt.Errorf("No default gateway for IPv4")
	}

	iface, err := net.InterfaceByName(ifaceName)
	if err != nil {
		return nil, "", err
	}

	addrs, err := iface.Addrs()
	if err != nil {
		return nil, "", err
	}

	var subnet *net.IPNet

	for _, addr := range addrs {
		addrIP, addrNet, err := net.ParseCIDR(addr.String())
		if err != nil {
			return nil, "", err
		}

		if addrIP.To4() == nil {
			continue
		}

		if subnet != nil {
			return nil, "", fmt.Errorf("More than one IPv4 subnet on default interface")
		}

		subnet = addrNet
	}

	if subnet == nil {
		return nil, "", fmt.Errorf("No IPv4 subnet on default interface")
	}

	return subnet, ifaceName, nil
}

func networkValidName(value string) error {
	// Not a veth-liked name
	if strings.HasPrefix(value, "veth") {
		return fmt.Errorf("Interface name cannot be prefix with veth")
	}

	// Validate the length
	if len(value) < 2 {
		return fmt.Errorf("Interface name is too short (minimum 2 characters)")
	}

	if len(value) > 15 {
		return fmt.Errorf("Interface name is too long (maximum 15 characters)")
	}

	// Validate the character set
	match, _ := regexp.MatchString("^[-_a-zA-Z0-9.]*$", value)
	if !match {
		return fmt.Errorf("Interface name contains invalid characters")
	}

	return nil
}

func networkValidPort(value string) error {
	if value == "" {
		return nil
	}

	valueInt, err := strconv.ParseInt(value, 10, 64)
	if err != nil {
		return fmt.Errorf("Invalid value for an integer: %s", value)
	}

	if valueInt < 1 || valueInt > 65536 {
		return fmt.Errorf("Invalid port number: %s", value)
	}

	return nil
}

func networkValidAddressCIDRV6(value string) error {
	if value == "" {
		return nil
	}

	ip, subnet, err := net.ParseCIDR(value)
	if err != nil {
		return err
	}

	if ip.To4() != nil {
		return fmt.Errorf("Not an IPv6 address: %s", value)
	}

	if ip.String() == subnet.IP.String() {
		return fmt.Errorf("Not a usable IPv6 address: %s", value)
	}

	return nil
}

func networkValidAddressCIDRV4(value string) error {
	if value == "" {
		return nil
	}

	ip, subnet, err := net.ParseCIDR(value)
	if err != nil {
		return err
	}

	if ip.To4() == nil {
		return fmt.Errorf("Not an IPv4 address: %s", value)
	}

	if ip.String() == subnet.IP.String() {
		return fmt.Errorf("Not a usable IPv4 address: %s", value)
	}

	return nil
}

func networkValidAddress(value string) error {
	if value == "" {
		return nil
	}

	ip := net.ParseIP(value)
	if ip == nil {
		return fmt.Errorf("Not an IP address: %s", value)
	}

	return nil
}

func networkValidAddressV4(value string) error {
	if value == "" {
		return nil
	}

	ip := net.ParseIP(value)
	if ip == nil || ip.To4() == nil {
		return fmt.Errorf("Not an IPv4 address: %s", value)
	}

	return nil
}

func networkValidAddressV6(value string) error {
	if value == "" {
		return nil
	}

	ip := net.ParseIP(value)
	if ip == nil || ip.To4() != nil {
		return fmt.Errorf("Not an IPv6 address: %s", value)
	}

	return nil
}

func networkValidAddressV4List(value string) error {
	for _, v := range strings.Split(value, ",") {
		v = strings.TrimSpace(v)
		err := networkValidAddressV4(v)
		if err != nil {
			return err
		}
	}
	return nil
}

func networkValidAddressV6List(value string) error {
	for _, v := range strings.Split(value, ",") {
		v = strings.TrimSpace(v)
		err := networkValidAddressV6(v)
		if err != nil {
			return err
		}
	}
	return nil
}

func networkValidNetworkV4(value string) error {
	if value == "" {
		return nil
	}

	ip, subnet, err := net.ParseCIDR(value)
	if err != nil {
		return err
	}

	if ip.To4() == nil {
		return fmt.Errorf("Not an IPv4 network: %s", value)
	}

	if ip.String() != subnet.IP.String() {
		return fmt.Errorf("Not an IPv4 network address: %s", value)
	}

	return nil
}

func networkValidNetworkV6(value string) error {
	if value == "" {
		return nil
	}

	ip, subnet, err := net.ParseCIDR(value)
	if err != nil {
		return err
	}

	if ip == nil || ip.To4() != nil {
		return fmt.Errorf("Not an IPv6 network: %s", value)
	}

	if ip.String() != subnet.IP.String() {
		return fmt.Errorf("Not an IPv6 network address: %s", value)
	}

	return nil
}

func networkAddressForSubnet(subnet *net.IPNet) (net.IP, string, error) {
	ifaces, err := net.Interfaces()
	if err != nil {
		return net.IP{}, "", err
	}

	for _, iface := range ifaces {
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}

		for _, addr := range addrs {
			ip, _, err := net.ParseCIDR(addr.String())
			if err != nil {
				continue
			}

			if subnet.Contains(ip) {
				return ip, iface.Name, nil
			}
		}
	}

	return net.IP{}, "", fmt.Errorf("No address found in subnet")
}

func networkFanAddress(underlay *net.IPNet, overlay *net.IPNet) (string, string, string, error) {
	// Sanity checks
	underlaySize, _ := underlay.Mask.Size()
	if underlaySize != 16 && underlaySize != 24 {
		return "", "", "", fmt.Errorf("Only /16 or /24 underlays are supported at this time")
	}

	overlaySize, _ := overlay.Mask.Size()
	if overlaySize != 8 && overlaySize != 16 {
		return "", "", "", fmt.Errorf("Only /8 or /16 overlays are supported at this time")
	}

	if overlaySize+(32-underlaySize)+8 > 32 {
		return "", "", "", fmt.Errorf("Underlay or overlay networks too large to accommodate the FAN")
	}

	// Get the IP
	ip, dev, err := networkAddressForSubnet(underlay)
	if err != nil {
		return "", "", "", err
	}
	ipStr := ip.String()

	// Force into IPv4 format
	ipBytes := ip.To4()
	if ipBytes == nil {
		return "", "", "", fmt.Errorf("Invalid IPv4: %s", ip)
	}

	// Compute the IP
	ipBytes[0] = overlay.IP[0]
	if overlaySize == 16 {
		ipBytes[1] = overlay.IP[1]
		ipBytes[2] = ipBytes[3]
	} else if underlaySize == 24 {
		ipBytes[1] = ipBytes[3]
		ipBytes[2] = 0
	} else if underlaySize == 16 {
		ipBytes[1] = ipBytes[2]
		ipBytes[2] = ipBytes[3]
	}

	ipBytes[3] = 1

	return fmt.Sprintf("%s/%d", ipBytes.String(), overlaySize), dev, ipStr, err
}

func networkKillForkDNS(name string) error {
	// Check if we have a running forkdns at all
	pidPath := shared.VarPath("networks", name, "forkdns.pid")
	if !shared.PathExists(pidPath) {
		return nil
	}

	// Grab the PID
	content, err := ioutil.ReadFile(pidPath)
	if err != nil {
		return err
	}
	pid := strings.TrimSpace(string(content))

	// Check for empty string
	if pid == "" {
		os.Remove(pidPath)
		return nil
	}

	// Check if it's forkdns
	cmdArgs, err := ioutil.ReadFile(fmt.Sprintf("/proc/%s/cmdline", pid))
	if err != nil {
		os.Remove(pidPath)
		return nil
	}

	cmdFields := strings.Split(string(bytes.TrimRight(cmdArgs, string("\x00"))), string(byte(0)))
	if len(cmdFields) < 5 || cmdFields[1] != "forkdns" {
		os.Remove(pidPath)
		return nil
	}

	// Parse the pid
	pidInt, err := strconv.Atoi(pid)
	if err != nil {
		return err
	}

	// Actually kill the process
	err = unix.Kill(pidInt, unix.SIGKILL)
	if err != nil {
		return err
	}

	// Cleanup
	os.Remove(pidPath)
	return nil
}

func networkKillDnsmasq(name string, reload bool) error {
	// Check if we have a running dnsmasq at all
	pidPath := shared.VarPath("networks", name, "dnsmasq.pid")
	if !shared.PathExists(pidPath) {
		if reload {
			return fmt.Errorf("dnsmasq isn't running")
		}

		return nil
	}

	// Grab the PID
	content, err := ioutil.ReadFile(pidPath)
	if err != nil {
		return err
	}
	pid := strings.TrimSpace(string(content))

	// Check for empty string
	if pid == "" {
		os.Remove(pidPath)

		if reload {
			return fmt.Errorf("dnsmasq isn't running")
		}

		return nil
	}

	// Check if the process still exists
	if !shared.PathExists(fmt.Sprintf("/proc/%s", pid)) {
		os.Remove(pidPath)

		if reload {
			return fmt.Errorf("dnsmasq isn't running")
		}

		return nil
	}

	// Check if it's dnsmasq
	cmdPath, err := os.Readlink(fmt.Sprintf("/proc/%s/exe", pid))
	if err != nil {
		cmdPath = ""
	}

	// Deal with deleted paths
	cmdName := filepath.Base(strings.Split(cmdPath, " ")[0])
	if cmdName != "dnsmasq" {
		if reload {
			return fmt.Errorf("dnsmasq isn't running")
		}

		os.Remove(pidPath)
		return nil
	}

	// Parse the pid
	pidInt, err := strconv.Atoi(pid)
	if err != nil {
		return err
	}

	// Actually kill the process
	if reload {
		err = unix.Kill(pidInt, unix.SIGHUP)
		if err != nil {
			return err
		}

		return nil
	}

	err = unix.Kill(pidInt, unix.SIGKILL)
	if err != nil {
		return err
	}

	// Cleanup
	os.Remove(pidPath)
	return nil
}

func networkGetDnsmasqVersion() (*version.DottedVersion, error) {
	// Discard stderr on purpose (occasional linker errors)
	output, err := exec.Command("dnsmasq", "--version").Output()
	if err != nil {
		return nil, fmt.Errorf("Failed to check dnsmasq version: %v", err)
	}

	lines := strings.Split(string(output), " ")
	return version.NewDottedVersion(lines[2])
}

func networkUpdateStatic(s *state.State, networkName string) error {
	// We don't want to race with ourselves here
	networkStaticLock.Lock()
	defer networkStaticLock.Unlock()

	// Get all the networks
	var networks []string
	if networkName == "" {
		var err error
		networks, err = s.Cluster.Networks()
		if err != nil {
			return err
		}
	} else {
		networks = []string{networkName}
	}

	// Get all the containers
	containers, err := containerLoadNodeAll(s)
	if err != nil {
		return err
	}

	// Build a list of dhcp host entries
	entries := map[string][][]string{}
	for _, c := range containers {
		// Go through all its devices (including profiles
		for k, d := range c.ExpandedDevices() {
			// Skip uninteresting entries
			if d["type"] != "nic" || d["nictype"] != "bridged" || !shared.StringInSlice(d["parent"], networks) {
				continue
			}

			// Fill in the hwaddr from volatile
			d, err = c.(*containerLXC).fillNetworkDevice(k, d)
			if err != nil {
				continue
			}

			// Add the new host entries
			_, ok := entries[d["parent"]]
			if !ok {
				entries[d["parent"]] = [][]string{}
			}

			entries[d["parent"]] = append(entries[d["parent"]], []string{d["hwaddr"], c.Project(), c.Name(), d["ipv4.address"], d["ipv6.address"]})
		}
	}

	// Update the host files
	for _, network := range networks {
		entries, _ := entries[network]

		// Skip networks we don't manage (or don't have DHCP enabled)
		if !shared.PathExists(shared.VarPath("networks", network, "dnsmasq.pid")) {
			continue
		}

		n, err := networkLoadByName(s, network)
		if err != nil {
			return err
		}
		config := n.Config()

		// Wipe everything clean
		files, err := ioutil.ReadDir(shared.VarPath("networks", network, "dnsmasq.hosts"))
		if err != nil {
			return err
		}

		for _, entry := range files {
			err = os.Remove(shared.VarPath("networks", network, "dnsmasq.hosts", entry.Name()))
			if err != nil {
				return err
			}
		}

		// Apply the changes
		for entryIdx, entry := range entries {
			hwaddr := entry[0]
			project := entry[1]
			cName := entry[2]
			ipv4Address := entry[3]
			ipv6Address := entry[4]
			line := hwaddr

			// Look for duplicates
			duplicate := false
			for iIdx, i := range entries {
				if projectPrefix(entry[1], entry[2]) == projectPrefix(i[1], i[2]) {
					// Skip ourselves
					continue
				}

				if entry[0] == i[0] {
					// Find broken configurations
					logger.Errorf("Duplicate MAC detected: %s and %s", projectPrefix(entry[1], entry[2]), projectPrefix(i[1], i[2]))
				}

				if i[3] == "" && i[4] == "" {
					// Skip unconfigured
					continue
				}

				if entry[3] == i[3] && entry[4] == i[4] {
					// Find identical containers (copies with static configuration)
					if entryIdx > iIdx {
						duplicate = true
					} else {
						line = fmt.Sprintf("%s,%s", line, i[0])
						logger.Debugf("Found containers with duplicate IPv4/IPv6: %s and %s", projectPrefix(entry[1], entry[2]), projectPrefix(i[1], i[2]))
					}
				}
			}

			if duplicate {
				continue
			}

			// Generate the dhcp-host line
			if ipv4Address != "" {
				line += fmt.Sprintf(",%s", ipv4Address)
			}

			if ipv6Address != "" {
				line += fmt.Sprintf(",[%s]", ipv6Address)
			}

			if config["dns.mode"] == "" || config["dns.mode"] == "managed" {
				line += fmt.Sprintf(",%s", cName)
			}

			if line == hwaddr {
				continue
			}

			err := ioutil.WriteFile(shared.VarPath("networks", network, "dnsmasq.hosts", projectPrefix(project, cName)), []byte(line+"\n"), 0644)
			if err != nil {
				return err
			}
		}

		// Signal dnsmasq
		err = networkKillDnsmasq(network, true)
		if err != nil {
			return err
		}
	}

	return nil
}

// networkUpdateForkdnsServersFile takes a list of node addresses and writes them atomically to
// the forkdns.servers file ready for forkdns to notice and re-apply its config.
func networkUpdateForkdnsServersFile(networkName string, addresses []string) error {
	// We don't want to race with ourselves here
	forkdnsServersLock.Lock()
	defer forkdnsServersLock.Unlock()

	permName := shared.VarPath("networks", networkName, forkdnsServersListPath+"/"+forkdnsServersListFile)
	tmpName := permName + ".tmp"

	// Open tmp file and truncate
	tmpFile, err := os.Create(tmpName)
	if err != nil {
		return err
	}
	defer tmpFile.Close()

	for _, address := range addresses {
		_, err := tmpFile.WriteString(address + "\n")
		if err != nil {
			return err
		}
	}

	tmpFile.Close()

	// Atomically rename finished file into permanent location so forkdns can pick it up.
	err = os.Rename(tmpName, permName)
	if err != nil {
		return err
	}

	return nil
}

// networkUpdateForkdnsServersTask runs every 30s and refreshes the forkdns servers list.
func networkUpdateForkdnsServersTask(d *Daemon) (task.Func, task.Schedule) {
	f := func(ctx context.Context) {
		s := d.State()

		// Get a list of managed networks
		networks, err := s.Cluster.NetworksNotPending()
		if err != nil {
			logger.Errorf("Failed to get networks: %v", err)
			return
		}

		for _, name := range networks {
			n, err := networkLoadByName(s, name)
			if err != nil {
				logger.Errorf("Failed to load network: %v", err)
				return
			}

			if n.config["bridge.mode"] == "fan" {
				n.refreshForkdnsServerAddresses()
			}
		}
	}

	first := true
	schedule := func() (time.Duration, error) {
		interval := time.Second * 30

		if first {
			first = false
			return interval, task.ErrSkip
		}

		return interval, nil
	}

	return f, schedule
}

// networksGetForkdnsServersList reads the server list file and returns the list as a slice.
func networksGetForkdnsServersList(networkName string) ([]string, error) {
	servers := []string{}
	file, err := os.Open(shared.VarPath("networks", networkName, forkdnsServersListPath, "/", forkdnsServersListFile))
	if err != nil {
		return servers, err
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) > 0 {
			servers = append(servers, fields[0])
		}
	}
	if err := scanner.Err(); err != nil {
		return servers, err
	}

	return servers, nil
}

func networkSysctlGet(path string) (string, error) {
	// Read the current content
	content, err := ioutil.ReadFile(fmt.Sprintf("/proc/sys/net/%s", path))
	if err != nil {
		return "", err
	}

	return string(content), nil
}

func networkSysctlSet(path string, value string) error {
	// Get current value
	current, err := networkSysctlGet(path)
	if err == nil && current == value {
		// Nothing to update
		return nil
	}

	return ioutil.WriteFile(fmt.Sprintf("/proc/sys/net/%s", path), []byte(value), 0)
}

func networkGetMacSlice(hwaddr string) []string {
	var buf []string

	if !strings.Contains(hwaddr, ":") {
		if s, err := strconv.ParseUint(hwaddr, 10, 64); err == nil {
			hwaddr = fmt.Sprintln(fmt.Sprintf("%x", s))
			var tuple string
			for i, r := range hwaddr {
				tuple = tuple + string(r)
				if i > 0 && (i+1)%2 == 0 {
					buf = append(buf, tuple)
					tuple = ""
				}
			}
		}
	} else {
		buf = strings.Split(strings.ToLower(hwaddr), ":")
	}

	return buf
}

func networkClearLease(s *state.State, name string, network string, hwaddr string) error {
	leaseFile := shared.VarPath("networks", network, "dnsmasq.leases")

	// Check that we are in fact running a dnsmasq for the network
	if !shared.PathExists(leaseFile) {
		return nil
	}

	// Convert MAC string to bytes to avoid any case comparison issues later.
	srcMAC, err := net.ParseMAC(hwaddr)
	if err != nil {
		return err
	}

	iface, err := net.InterfaceByName(network)
	if err != nil {
		return err
	}

	// Get IPv4 and IPv6 address of interface running dnsmasq on host.
	addrs, err := iface.Addrs()
	if err != nil {
		return err
	}

	var dstIPv4, dstIPv6 net.IP
	for _, addr := range addrs {
		ip, _, err := net.ParseCIDR(addr.String())
		if err != nil {
			return err
		}
		if !ip.IsGlobalUnicast() {
			continue
		}
		if ip.To4() == nil {
			dstIPv6 = ip
		} else {
			dstIPv4 = ip
		}
	}

	// Iterate the dnsmasq leases file looking for matching leases for this container to release.
	file, err := os.Open(leaseFile)
	if err != nil {
		return err
	}
	defer file.Close()

	var dstDUID string
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		fieldsLen := len(fields)

		// Handle lease lines
		if fieldsLen == 5 {
			if srcMAC.String() == fields[1] { // Handle IPv4 leases by matching MAC address to lease.
				srcIP := net.ParseIP(fields[2])

				if dstIPv4 == nil {
					logger.Errorf("Failed to release DHCPv4 lease for container \"%s\", IP \"%s\", MAC \"%s\", %v", name, srcIP, srcMAC, "No server address found")
					continue
				}

				err = networkDHCPv4Release(srcMAC, srcIP, dstIPv4)
				if err != nil {
					logger.Errorf("Failed to release DHCPv4 lease for container \"%s\", IP \"%s\", MAC \"%s\", %v", name, srcIP, srcMAC, err)
				}
			} else if name == fields[3] { // Handle IPv6 addresses by matching hostname to lease.
				IAID := fields[1]
				srcIP := net.ParseIP(fields[2])
				DUID := fields[4]

				// Skip IPv4 addresses.
				if srcIP.To4() != nil {
					continue
				}

				if dstIPv6 == nil {
					logger.Errorf("Failed to release DHCPv6 lease for container \"%s\", IP \"%s\", DUID \"%s\", IAID \"%s\": %s", name, srcIP, DUID, IAID, "No server address found")
					continue // Cant send release packet if no dstIP found.
				}

				if dstDUID == "" {
					logger.Errorf("Failed to release DHCPv6 lease for container \"%s\", IP \"%s\", DUID \"%s\", IAID \"%s\": %s", name, srcIP, DUID, IAID, "No server DUID found")
					continue // Cant send release packet if no dstDUID found.
				}

				err = networkDHCPv6Release(DUID, IAID, srcIP, dstIPv6, dstDUID)
				if err != nil {
					logger.Errorf("Failed to release DHCPv6 lease for container \"%s\", IP \"%s\", DUID \"%s\", IAID \"%s\": %v", name, srcIP, DUID, IAID, err)
				}
			}
		} else if fieldsLen == 2 && fields[0] == "duid" {
			// Handle server DUID line needed for releasing IPv6 leases.
			// This should come before the IPv6 leases in the lease file.
			dstDUID = fields[1]
		}
	}
	if err := scanner.Err(); err != nil {
		return err
	}

	return nil
}

func networkGetState(netIf net.Interface) api.NetworkState {
	netState := "down"
	netType := "unknown"

	if netIf.Flags&net.FlagBroadcast > 0 {
		netType = "broadcast"
	}

	if netIf.Flags&net.FlagPointToPoint > 0 {
		netType = "point-to-point"
	}

	if netIf.Flags&net.FlagLoopback > 0 {
		netType = "loopback"
	}

	if netIf.Flags&net.FlagUp > 0 {
		netState = "up"
	}

	network := api.NetworkState{
		Addresses: []api.NetworkStateAddress{},
		Counters:  api.NetworkStateCounters{},
		Hwaddr:    netIf.HardwareAddr.String(),
		Mtu:       netIf.MTU,
		State:     netState,
		Type:      netType,
	}

	// Get address information
	addrs, err := netIf.Addrs()
	if err == nil {
		for _, addr := range addrs {
			fields := strings.SplitN(addr.String(), "/", 2)
			if len(fields) != 2 {
				continue
			}

			family := "inet"
			if strings.Contains(fields[0], ":") {
				family = "inet6"
			}

			scope := "global"
			if strings.HasPrefix(fields[0], "127") {
				scope = "local"
			}

			if fields[0] == "::1" {
				scope = "local"
			}

			if strings.HasPrefix(fields[0], "169.254") {
				scope = "link"
			}

			if strings.HasPrefix(fields[0], "fe80:") {
				scope = "link"
			}

			address := api.NetworkStateAddress{}
			address.Family = family
			address.Address = fields[0]
			address.Netmask = fields[1]
			address.Scope = scope

			network.Addresses = append(network.Addresses, address)
		}
	}

	// Get counters
	network.Counters = shared.NetworkGetCounters(netIf.Name)
	return network
}

// networkGetDevMTU retrieves the current MTU setting for a named network device.
func networkGetDevMTU(devName string) (uint64, error) {
	content, err := ioutil.ReadFile(fmt.Sprintf("/sys/class/net/%s/mtu", devName))
	if err != nil {
		return 0, err
	}

	// Parse value
	mtu, err := strconv.ParseUint(strings.TrimSpace(string(content)), 10, 32)
	if err != nil {
		return 0, err
	}

	return mtu, nil
}

// networkSetDevMTU sets the MTU setting for a named network device if different from current.
func networkSetDevMTU(devName string, mtu uint64) error {
	curMTU, err := networkGetDevMTU(devName)
	if err != nil {
		return err
	}

	// Only try and change the MTU if the requested mac is different to current one.
	if curMTU != mtu {
		_, err := shared.RunCommand("ip", "link", "set", "dev", devName, "mtu", fmt.Sprintf("%d", mtu))
		if err != nil {
			return err
		}
	}

	return nil
}

// networkGetDevMAC retrieves the current MAC setting for a named network device.
func networkGetDevMAC(devName string) (string, error) {
	content, err := ioutil.ReadFile(fmt.Sprintf("/sys/class/net/%s/address", devName))
	if err != nil {
		return "", err
	}

	return strings.TrimSpace(fmt.Sprintf("%s", content)), nil
}

// networkSetDevMAC sets the MAC setting for a named network device if different from current.
func networkSetDevMAC(devName string, mac string) error {
	curMac, err := networkGetDevMAC(devName)
	if err != nil {
		return err
	}

	// Only try and change the MAC if the requested mac is different to current one.
	if curMac != mac {
		_, err := shared.RunCommand("ip", "link", "set", "dev", devName, "address", mac)
		if err != nil {
			return err
		}
	}

	return nil
}

// networkListBootRoutesV4 returns a list of IPv4 boot routes on a named network device.
func networkListBootRoutesV4(devName string) ([]string, error) {
	routes := []string{}
	cmd := exec.Command("ip", "-4", "route", "show", "dev", devName, "proto", "boot")
	ipOut, err := cmd.StdoutPipe()
	if err != nil {
		return routes, err
	}
	cmd.Start()
	scanner := bufio.NewScanner(ipOut)
	for scanner.Scan() {
		route := strings.Replace(scanner.Text(), "linkdown", "", -1)
		routes = append(routes, route)
	}
	cmd.Wait()
	return routes, nil
}

// networkListBootRoutesV6 returns a list of IPv6 boot routes on a named network device.
func networkListBootRoutesV6(devName string) ([]string, error) {
	routes := []string{}
	cmd := exec.Command("ip", "-6", "route", "show", "dev", devName, "proto", "boot")
	ipOut, err := cmd.StdoutPipe()
	if err != nil {
		return routes, err
	}
	cmd.Start()
	scanner := bufio.NewScanner(ipOut)
	for scanner.Scan() {
		route := strings.Replace(scanner.Text(), "linkdown", "", -1)
		routes = append(routes, route)
	}
	cmd.Wait()
	return routes, nil
}

// networkApplyBootRoutesV4 applies a list of IPv4 boot routes to a named network device.
func networkApplyBootRoutesV4(devName string, routes []string) error {
	for _, route := range routes {
		cmd := []string{"-4", "route", "replace", "dev", devName, "proto", "boot"}
		cmd = append(cmd, strings.Fields(route)...)
		_, err := shared.RunCommand("ip", cmd...)
		if err != nil {
			return err
		}
	}

	return nil
}

// networkApplyBootRoutesV6 applies a list of IPv6 boot routes to a named network device.
func networkApplyBootRoutesV6(devName string, routes []string) error {
	for _, route := range routes {
		cmd := []string{"-6", "route", "replace", "dev", devName, "proto", "boot"}
		cmd = append(cmd, strings.Fields(route)...)
		_, err := shared.RunCommand("ip", cmd...)
		if err != nil {
			return err
		}
	}

	return nil
}

// virtFuncInfo holds information about SR-IOV virtual functions.
type virtFuncInfo struct {
	mac        string
	vlan       int
	spoofcheck bool
}

// networkGetVirtFuncInfo returns info about an SR-IOV virtual function from the ip tool.
func networkGetVirtFuncInfo(devName string, vfID int) (vf virtFuncInfo, err error) {
	cmd := exec.Command("ip", "link", "show", devName)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return
	}
	if err = cmd.Start(); err != nil {
		return
	}
	defer stdout.Close()

	// Try and match: "vf 1 MAC 00:00:00:00:00:00, vlan 4095, spoof checking off"
	reVlan := regexp.MustCompile(fmt.Sprintf(`vf %d MAC ((?:[[:xdigit:]]{2}:){5}[[:xdigit:]]{2}).*, vlan (\d+), spoof checking (\w+)`, vfID))

	// IP link command doesn't show the vlan property if its set to 0, so we need to detect that.
	// Try and match: "vf 1 MAC 00:00:00:00:00:00, spoof checking off"
	reNoVlan := regexp.MustCompile(fmt.Sprintf(`vf %d MAC ((?:[[:xdigit:]]{2}:){5}[[:xdigit:]]{2}).*, spoof checking (\w+)`, vfID))
	scanner := bufio.NewScanner(stdout)
	for scanner.Scan() {
		// First try and find VF and reads its properties with VLAN activated.
		res := reVlan.FindStringSubmatch(scanner.Text())
		if len(res) == 4 {
			vlan, err := strconv.Atoi(res[2])
			if err != nil {
				return vf, err
			}

			vf.mac = res[1]
			vf.vlan = vlan
			vf.spoofcheck = shared.IsTrue(res[3])
			return vf, err
		}

		// Next try and find VF and reads its properties with VLAN missing.
		res = reNoVlan.FindStringSubmatch(scanner.Text())
		if len(res) == 3 {
			vf.mac = res[1]
			vf.vlan = 0 // Missing VLAN ID means 0 when resetting later.
			vf.spoofcheck = shared.IsTrue(res[2])
			return vf, err
		}
	}
	if err = scanner.Err(); err != nil {
		return
	}

	return vf, fmt.Errorf("no matching virtual function found")
}

// networkGetVFDevicePCISlot returns the PCI slot name for a network virtual function device.
func networkGetVFDevicePCISlot(parentName string, vfID string) (string, error) {
	file, err := os.Open(fmt.Sprintf("/sys/class/net/%s/device/virtfn%s/uevent", parentName, vfID))
	if err != nil {
		return "", err
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		// Looking for something like this "PCI_SLOT_NAME=0000:05:10.0"
		fields := strings.SplitN(scanner.Text(), "=", 2)
		if len(fields) == 2 && fields[0] == "PCI_SLOT_NAME" {
			return fields[1], nil
		}
	}
	if err := scanner.Err(); err != nil {
		return "", err
	}

	return "", fmt.Errorf("PCI_SLOT_NAME not found")
}

// networkGetVFDeviceDriverPath returns the path to the network virtual function device driver in /sys.
func networkGetVFDeviceDriverPath(parentName string, vfID string) (string, error) {
	return filepath.EvalSymlinks(fmt.Sprintf("/sys/class/net/%s/device/virtfn%s/driver", parentName, vfID))
}

// networkDeviceUnbind unbinds a network device from the OS using its PCI Slot Name and driver path.
func networkDeviceUnbind(pciSlotName string, driverPath string) error {
	return ioutil.WriteFile(fmt.Sprintf("%s/unbind", driverPath), []byte(pciSlotName), 0600)
}

// networkDeviceUnbind binds a network device to the OS using its PCI Slot Name and driver path.
func networkDeviceBind(pciSlotName string, driverPath string) error {
	return ioutil.WriteFile(fmt.Sprintf("%s/bind", driverPath), []byte(pciSlotName), 0600)
}

// networkDeviceBindWait waits for network interface to appear after being binded.
func networkDeviceBindWait(devName string) error {
	for i := 0; i < 10; i++ {
		if shared.PathExists(fmt.Sprintf("/sys/class/net/%s", devName)) {
			return nil
		}

		time.Sleep(50 * time.Millisecond)
	}

	return fmt.Errorf("Bind of interface \"%s\" took too long", devName)
}

// networkDHCPv4Release sends a DHCPv4 release packet to a DHCP server.
func networkDHCPv4Release(srcMAC net.HardwareAddr, srcIP net.IP, dstIP net.IP) error {
	dstAddr, err := net.ResolveUDPAddr("udp", fmt.Sprintf("%s:67", dstIP.String()))
	if err != nil {
		return err
	}
	conn, err := net.DialUDP("udp", nil, dstAddr)
	if err != nil {
		return err
	}
	defer conn.Close()

	//Random DHCP transaction ID
	xid := rand.Uint32()

	// Construct a DHCP packet pretending to be from the source IP and MAC supplied.
	dhcp := layers.DHCPv4{
		Operation:    layers.DHCPOpRequest,
		HardwareType: layers.LinkTypeEthernet,
		ClientHWAddr: srcMAC,
		ClientIP:     srcIP,
		Xid:          xid,
	}

	// Add options to DHCP release packet.
	dhcp.Options = append(dhcp.Options,
		layers.NewDHCPOption(layers.DHCPOptMessageType, []byte{byte(layers.DHCPMsgTypeRelease)}),
		layers.NewDHCPOption(layers.DHCPOptServerID, dstIP.To4()),
	)

	buf := gopacket.NewSerializeBuffer()
	opts := gopacket.SerializeOptions{
		ComputeChecksums: true,
		FixLengths:       true,
	}

	err = gopacket.SerializeLayers(buf, opts, &dhcp)
	if err != nil {
		return err
	}

	_, err = conn.Write(buf.Bytes())
	return err
}

// networkDHCPv6Release sends a DHCPv6 release packet to a DHCP server.
func networkDHCPv6Release(srcDUID string, srcIAID string, srcIP net.IP, dstIP net.IP, dstDUID string) error {
	dstAddr, err := net.ResolveUDPAddr("udp6", fmt.Sprintf("[%s]:547", dstIP.String()))
	if err != nil {
		return err
	}
	conn, err := net.DialUDP("udp6", nil, dstAddr)
	if err != nil {
		return err
	}
	defer conn.Close()

	// Construct a DHCPv6 packet pretending to be from the source IP and MAC supplied.
	dhcp := layers.DHCPv6{
		MsgType: layers.DHCPv6MsgTypeRelease,
	}

	// Convert Server DUID from string to byte array
	dstDUIDRaw, err := hex.DecodeString(strings.Replace(dstDUID, ":", "", -1))
	if err != nil {
		return err
	}

	// Convert DUID from string to byte array
	srcDUIDRaw, err := hex.DecodeString(strings.Replace(srcDUID, ":", "", -1))
	if err != nil {
		return err
	}

	// Convert IAID string to int
	srcIAIDRaw, err := strconv.Atoi(srcIAID)
	if err != nil {
		return err
	}

	// Build the Identity Association details option manually (as not provided by gopacket).
	iaAddr := networkDHCPv6CreateIAAddress(srcIP)
	ianaRaw := networkDHCPv6CreateIANA(srcIAIDRaw, iaAddr)

	// Add options to DHCP release packet.
	dhcp.Options = append(dhcp.Options,
		layers.NewDHCPv6Option(layers.DHCPv6OptServerID, dstDUIDRaw),
		layers.NewDHCPv6Option(layers.DHCPv6OptClientID, srcDUIDRaw),
		layers.NewDHCPv6Option(layers.DHCPv6OptIANA, ianaRaw),
	)

	buf := gopacket.NewSerializeBuffer()
	opts := gopacket.SerializeOptions{
		ComputeChecksums: true,
		FixLengths:       true,
	}

	err = gopacket.SerializeLayers(buf, opts, &dhcp)
	if err != nil {
		return err
	}

	_, err = conn.Write(buf.Bytes())
	return err
}

// networkDHCPv6CreateIANA creates a DHCPv6 Identity Association for Non-temporary Address (rfc3315 IA_NA) option.
func networkDHCPv6CreateIANA(IAID int, IAAddr []byte) []byte {
	data := make([]byte, 12)
	binary.BigEndian.PutUint32(data[0:4], uint32(IAID)) // Identity Association Identifier
	binary.BigEndian.PutUint32(data[4:8], uint32(0))    // T1
	binary.BigEndian.PutUint32(data[8:12], uint32(0))   // T2
	data = append(data, IAAddr...)                      // Append the IA Address details
	return data
}

// networkDHCPv6CreateIAAddress creates a DHCPv6 Identity Association Address (rfc3315) option.
func networkDHCPv6CreateIAAddress(IP net.IP) []byte {
	data := make([]byte, 28)
	binary.BigEndian.PutUint16(data[0:2], uint16(layers.DHCPv6OptIAAddr)) // Sub-Option type
	binary.BigEndian.PutUint16(data[2:4], uint16(24))                     // Length (fixed at 24 bytes)
	copy(data[4:20], IP)                                                  // IPv6 address to be released
	binary.BigEndian.PutUint32(data[20:24], uint32(0))                    // Preferred liftetime
	binary.BigEndian.PutUint32(data[24:28], uint32(0))                    // Valid lifetime
	return data
}
