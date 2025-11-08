package hosts

import (
	"fmt"
	"net"
	"strings"
)

// HostEntry represents a single entry in the hosts file
type HostEntry struct {
	IP        net.IP
	Hostnames []string
}

// String returns the string representation of the host entry
func (h *HostEntry) String() string {
	if len(h.Hostnames) == 0 {
		return ""
	}
	return fmt.Sprintf("%s %s", h.IP.String(), strings.Join(h.Hostnames, " "))
}

// NewHostEntry creates a new host entry
func NewHostEntry(ip net.IP, hostnames []string) *HostEntry {
	return &HostEntry{
		IP:        ip,
		Hostnames: hostnames,
	}
}
