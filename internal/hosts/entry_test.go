package hosts

import (
	"net"
	"testing"
)

func TestHostEntry_String(t *testing.T) {
	tests := []struct {
		name  string
		entry HostEntry
		want  string
	}{
		{
			name: "single hostname",
			entry: HostEntry{
				IP:        net.ParseIP("172.20.0.2"),
				Hostnames: []string{"app1"},
			},
			want: "172.20.0.2 app1",
		},
		{
			name: "multiple hostnames",
			entry: HostEntry{
				IP:        net.ParseIP("172.20.0.3"),
				Hostnames: []string{"app2", "app2.saltbox", "myapp"},
			},
			want: "172.20.0.3 app2 app2.saltbox myapp",
		},
		{
			name: "empty hostnames",
			entry: HostEntry{
				IP:        net.ParseIP("172.20.0.4"),
				Hostnames: []string{},
			},
			want: "",
		},
		{
			name: "IPv6 address",
			entry: HostEntry{
				IP:        net.ParseIP("2001:db8::1"),
				Hostnames: []string{"ipv6host"},
			},
			want: "2001:db8::1 ipv6host",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.entry.String()
			if got != tt.want {
				t.Errorf("HostEntry.String() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestNewHostEntry(t *testing.T) {
	ip := net.ParseIP("10.0.0.1")
	hostnames := []string{"test1", "test2"}

	entry := NewHostEntry(ip, hostnames)

	if !entry.IP.Equal(ip) {
		t.Errorf("NewHostEntry() IP = %v, want %v", entry.IP, ip)
	}

	if len(entry.Hostnames) != len(hostnames) {
		t.Errorf("NewHostEntry() Hostnames length = %d, want %d", len(entry.Hostnames), len(hostnames))
	}

	for i, h := range entry.Hostnames {
		if h != hostnames[i] {
			t.Errorf("NewHostEntry() Hostnames[%d] = %q, want %q", i, h, hostnames[i])
		}
	}
}
