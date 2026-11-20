package foremanapp

import (
	"errors"
	"net"
	"os"
	"strings"
	"testing"

	"github.com/pricec/factory/internal/wire"
)

func fakeProbe(files map[string]string) hostProbe {
	_, v4, _ := net.ParseCIDR("192.168.1.20/24")
	v4.IP = net.ParseIP("192.168.1.20")
	return hostProbe{
		readFile: func(p string) ([]byte, error) {
			if s, ok := files[p]; ok {
				return []byte(s), nil
			}
			return nil, os.ErrNotExist
		},
		hostname: func() (string, error) { return "box1", nil },
		interfaces: func() ([]ifaceAddrs, error) {
			mk := func(cidr string) net.Addr {
				ip, n, _ := net.ParseCIDR(cidr)
				n.IP = ip
				return n
			}
			return []ifaceAddrs{
				{Name: "lo", Flags: net.FlagLoopback, Addrs: []net.Addr{mk("127.0.0.1/8")}},
				{Name: "eth0", Addrs: []net.Addr{mk("192.168.1.20/24"), mk("2001:db8::5/64"), mk("fe80::1/64")}},
				{Name: "docker0", Addrs: []net.Addr{mk("172.17.0.1/16")}},
				{Name: "veth123", Addrs: []net.Addr{mk("10.9.9.9/24")}},
			}, nil
		},
		statfs: func(p string) (int64, int64, error) {
			if p == "/bad" {
				return 0, 0, errors.New("nope")
			}
			return 1000, 400, nil
		},
		numCPU: func() int { return 8 },
		arch:   "amd64",
		goos:   "linux",
	}
}

func TestCollectHostFacts(t *testing.T) {
	files := map[string]string{
		"/etc/machine-id":            "0123456789abcdef0123456789abcdef\n",
		"/etc/os-release":            "NAME=Debian\nPRETTY_NAME=\"Debian GNU/Linux 13 (trixie)\"\n",
		"/proc/sys/kernel/osrelease": "6.12.1-amd64\n",
		"/proc/uptime":               "12345.67 999.0\n",
		"/proc/meminfo":              "MemTotal:       16000 kB\nMemFree: 1 kB\nMemAvailable:    8000 kB\n",
	}
	h := collectHostFacts(fakeProbe(files), "/work", nil)
	if len(h.HostID) != 12 || strings.Contains(h.HostID, "0123456789ab") {
		t.Errorf("host_id = %q (must be 12 hex of a hash)", h.HostID)
	}
	if h.Hostname != "box1" || h.OS != "Debian GNU/Linux 13 (trixie)" || h.Kernel != "6.12.1-amd64" ||
		h.Arch != "amd64" || h.CPUs != 8 || h.UptimeS != 12345 ||
		h.MemTotalBytes != 16000*1024 || h.MemAvailBytes != 8000*1024 {
		t.Errorf("facts = %+v", h)
	}
	if len(h.Disks) != 2 || h.Disks[0].Path != "/" || h.Disks[1].Path != "/work" || h.Disks[1].FreeBytes != 400 {
		t.Errorf("disks = %+v", h.Disks)
	}
	want := map[string]bool{
		"eth0 192.168.1.20 false": true, "eth0 2001:db8::5 false": true,
		"docker0 172.17.0.1 true": true, "veth123 10.9.9.9 true": true,
	}
	if len(h.IPs) != len(want) {
		t.Fatalf("ips = %+v", h.IPs)
	}
	for _, ip := range h.IPs {
		k := ip.Iface + " " + ip.Addr + " " + map[bool]string{true: "true", false: "false"}[ip.Virtual]
		if !want[k] {
			t.Errorf("unexpected ip %s", k)
		}
	}
}

func TestHostIDSourcesAndStability(t *testing.T) {
	id := func(files map[string]string, host string) string { return hostID(fakeProbe(files), host) }
	a := id(map[string]string{"/etc/machine-id": "abc"}, "h1")
	if a != id(map[string]string{"/etc/machine-id": "abc"}, "other-hostname") {
		t.Error("same machine-id must give same host_id regardless of hostname")
	}
	dbus := id(map[string]string{"/var/lib/dbus/machine-id": "abc"}, "h1")
	if dbus != a {
		t.Error("dbus fallback should hash the same machine-id")
	}
	if id(nil, "h1") == id(nil, "h2") || id(nil, "h1") != id(nil, "h1") {
		t.Error("hostname fallback must be stable and distinguish hosts")
	}
	if id(map[string]string{"/etc/machine-id": "  \n"}, "h1") != id(nil, "h1") {
		t.Error("blank machine-id should fall through")
	}
	if len(id(nil, "")) != 12 {
		t.Error("empty hostname still yields an id")
	}
}

func TestCollectHostFactsMissingEverything(t *testing.T) {
	p := fakeProbe(nil)
	p.interfaces = func() ([]ifaceAddrs, error) { return nil, errors.New("x") }
	h := collectHostFacts(p, "/bad", nil)
	if h.HostID == "" || h.OS != "linux" || h.MemTotalBytes != 0 || len(h.IPs) != 0 || len(h.Disks) != 1 {
		t.Errorf("degraded facts = %+v", h)
	}
}

func TestRealHostFactsSmoke(t *testing.T) {
	h := collectHostFacts(realHostProbe(), t.TempDir(), nil)
	if h.HostID == "" || h.CPUs < 1 {
		t.Errorf("real facts = %+v", h)
	}
}

func TestCollectHostFactsLoadSwapCPU(t *testing.T) {
	files := map[string]string{
		"/proc/meminfo": "MemTotal: 16000 kB\nMemAvailable: 8000 kB\nSwapTotal: 4000 kB\nSwapFree: 1000 kB\n",
		"/proc/loadavg": "0.00 1.50 2.25 1/200 999\n",
		"/proc/cpuinfo": "processor : 0\nmodel name\t: Test CPU @ 3GHz\n",
		"/proc/stat":    "cpu  100 0 100 800 0 0 0 0 0 0\ncpu0 1 1 1 1\n",
	}
	p := fakeProbe(files)
	var cpu cpuSampler
	h := collectHostFacts(p, "/work", &cpu)
	if h.SwapTotalBytes != 4000*1024 || h.SwapFreeBytes != 1000*1024 {
		t.Errorf("swap = %d/%d", h.SwapTotalBytes, h.SwapFreeBytes)
	}
	if h.Load == nil || h.Load.One != 0 || h.Load.Five != 1.5 || h.Load.Fifteen != 2.25 {
		t.Errorf("load = %+v", h.Load)
	}
	if h.CPUModel != "Test CPU @ 3GHz" || h.PrimaryIP != "192.168.1.20" {
		t.Errorf("model/primary = %q %q", h.CPUModel, h.PrimaryIP)
	}
	if h.CPUUsagePct != nil {
		t.Errorf("first sample must have no cpu usage, got %v", *h.CPUUsagePct)
	}
	// +100 busy-ish ticks (user 50 + system 50), +100 idle => 50%.
	files["/proc/stat"] = "cpu  150 0 150 900 0 0 0 0 0 0\n"
	h = collectHostFacts(p, "/work", &cpu)
	if h.CPUUsagePct == nil || *h.CPUUsagePct != 50 {
		t.Errorf("cpu usage = %v, want 50", h.CPUUsagePct)
	}
	// No elapsed ticks, or counters going backwards: no figure, no panic.
	if v := cpu.sample(p); v != nil {
		t.Errorf("no-delta sample = %v", *v)
	}
	files["/proc/stat"] = "cpu  1 0 1 1 0 0 0 0\n"
	if v := cpu.sample(p); v != nil {
		t.Errorf("regressed counters = %v", *v)
	}
}

func TestPrimaryIP(t *testing.T) {
	v := func(i, a string, virt bool) wire.HostIP { return wire.HostIP{Iface: i, Addr: a, Virtual: virt} }
	if got := primaryIP([]wire.HostIP{v("docker0", "172.17.0.1", true), v("eth0", "2001:db8::5", false), v("eth0", "10.0.0.2", false)}); got != "10.0.0.2" {
		t.Errorf("want ipv4 non-virtual, got %q", got)
	}
	if got := primaryIP([]wire.HostIP{v("docker0", "172.17.0.1", true), v("eth0", "2001:db8::5", false)}); got != "2001:db8::5" {
		t.Errorf("ipv6 fallback, got %q", got)
	}
	if got := primaryIP([]wire.HostIP{v("docker0", "172.17.0.1", true)}); got != "" {
		t.Errorf("only virtual: got %q", got)
	}
}

func TestHostFactsDegradeWithoutProc(t *testing.T) {
	var cpu cpuSampler
	h := collectHostFacts(fakeProbe(nil), "/work", &cpu)
	if h.Load != nil || h.CPUUsagePct != nil || h.SwapTotalBytes != 0 || h.CPUModel != "" {
		t.Errorf("expected zero/nil metrics, got %+v", h)
	}
}
