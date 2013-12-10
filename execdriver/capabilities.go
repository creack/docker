package execdriver

import (
	"fmt"
	"io/ioutil"
	"os"
	"path"
	"strings"
)

var capabilities *Capabilities

func init() {
	GetCapabilities()
}

type Capabilities struct {
	MemoryLimit            bool
	SwapLimit              bool
	IPv4ForwardingDisabled bool
	AppArmor               bool
}

// GetCapabilities look for the cgroup mountpoint and check for memory capabilities.
// It also checks for the ipv4 forwarding and for the AppArmor presence. It returns
// a Capabilities struct pointer filled with the retreived data.
func GetCapabilities() *Capabilities {
	if capabilities != nil {
		return capabilities
	}
	capabilities = &Capabilities{}
	if cgroupMemoryMountpoint, err := findCgroupMountpoint("memory"); err == nil {
		_, err1 := os.Stat(path.Join(cgroupMemoryMountpoint, "memory.limit_in_bytes"))
		_, err2 := os.Stat(path.Join(cgroupMemoryMountpoint, "memory.soft_limit_in_bytes"))
		_, err3 := os.Stat(path.Join(cgroupMemoryMountpoint, "memory.memsw.limit_in_bytes"))

		capabilities.MemoryLimit = err1 == nil && err2 == nil
		capabilities.SwapLimit = err3 == nil
	}

	content, err := ioutil.ReadFile("/proc/sys/net/ipv4/ip_forward")
	capabilities.IPv4ForwardingDisabled = err != nil || len(content) == 0 || content[0] != '1'

	// Check if AppArmor seems to be enabled on this system.
	_, err4 := os.Stat("/sys/kernel/security/apparmor")
	capabilities.AppArmor = err4 == nil

	return capabilities
}

func findCgroupMountpoint(cgroupType string) (string, error) {
	output, err := ioutil.ReadFile("/proc/mounts")
	if err != nil {
		return "", err
	}

	// /proc/mounts has 6 fields per line, one mount per line, e.g.
	// cgroup /sys/fs/cgroup/devices cgroup rw,relatime,devices 0 0
	for _, line := range strings.Split(string(output), "\n") {
		parts := strings.Split(line, " ")
		if len(parts) == 6 && parts[2] == "cgroup" {
			for _, opt := range strings.Split(parts[3], ",") {
				if opt == cgroupType {
					return parts[1], nil
				}
			}
		}
	}

	return "", fmt.Errorf("cgroup mountpoint not found for %s", cgroupType)
}
