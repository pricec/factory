package foremanapp

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"

	"github.com/pricec/factory/internal/wire"
)

// hostProbe abstracts every system read so fact collection is testable.
// The zero value is not usable; use realHostProbe.
type hostProbe struct {
	readFile   func(path string) ([]byte, error)
	hostname   func() (string, error)
	interfaces func() ([]ifaceAddrs, error)
	statfs     func(path string) (total, free int64, err error)
	numCPU     func() int
	arch       string
	goos       string
}

// ifaceAddrs is one network interface with its addresses.
type ifaceAddrs struct {
	Name  string
	Flags net.Flags
	Addrs []net.Addr
}

func realHostProbe() hostProbe {
	return hostProbe{
		readFile: os.ReadFile,
		hostname: os.Hostname,
		interfaces: func() ([]ifaceAddrs, error) {
			ifs, err := net.Interfaces()
			if err != nil {
				return nil, err
			}
			var out []ifaceAddrs
			for _, i := range ifs {
				addrs, err := i.Addrs()
				if err != nil {
					continue
				}
				out = append(out, ifaceAddrs{Name: i.Name, Flags: i.Flags, Addrs: addrs})
			}
			return out, nil
		},
		statfs: func(path string) (int64, int64, error) {
			var st syscall.Statfs_t
			if err := syscall.Statfs(path, &st); err != nil {
				return 0, 0, err
			}
			bs := int64(st.Bsize)
			return int64(st.Blocks) * bs, int64(st.Bavail) * bs, nil
		},
		numCPU: runtime.NumCPU,
		arch:   runtime.GOARCH,
		goos:   runtime.GOOS,
	}
}

// collectHostFacts gathers the host object (spec §6.7.1). Every probe is
// best-effort: a failed read leaves its fields zero; only host_id is
// always set (it falls back to the hostname, then "unknown").
func collectHostFacts(p hostProbe, workDir string) wire.HostInfo {
	hostname, _ := p.hostname()
	h := wire.HostInfo{
		HostID:   hostID(p, hostname),
		Hostname: hostname,
		IPs:      hostIPs(p),
		OS:       osPrettyName(p),
		Kernel:   kernelRelease(p),
		Arch:     p.arch,
		CPUs:     p.numCPU(),
		UptimeS:  uptimeSeconds(p),
	}
	h.MemTotalBytes, h.MemAvailBytes = memInfo(p)
	seen := map[string]bool{}
	for _, path := range []string{"/", workDir} {
		if path == "" || seen[path] {
			continue
		}
		seen[path] = true
		total, free, err := p.statfs(path)
		if err != nil {
			continue
		}
		h.Disks = append(h.Disks, wire.HostDisk{Path: path, TotalBytes: total, FreeBytes: free})
	}
	return h
}

// hostID is a stable, non-reversible machine identity: the first 12 hex
// chars of sha256 over /etc/machine-id, else /var/lib/dbus/machine-id,
// else the hostname. Foremen on one box therefore share it.
func hostID(p hostProbe, hostname string) string {
	src := ""
	for _, path := range []string{"/etc/machine-id", "/var/lib/dbus/machine-id"} {
		if b, err := p.readFile(path); err == nil {
			if s := strings.TrimSpace(string(b)); s != "" {
				src = "machine-id:" + s
				break
			}
		}
	}
	if src == "" {
		if hostname == "" {
			hostname = "unknown"
		}
		src = "hostname:" + hostname
	}
	sum := sha256.Sum256([]byte(src))
	return hex.EncodeToString(sum[:])[:12]
}

// virtualIface reports container/bridge noise interface names.
func virtualIface(name string) bool {
	for _, pre := range []string{"docker", "br-", "veth", "virbr", "cni", "flannel", "cali", "lxcbr", "lxdbr", "tun", "tap"} {
		if strings.HasPrefix(name, pre) {
			return true
		}
	}
	return false
}

// hostIPs lists non-loopback, non-link-local unicast addresses per
// interface, marking virtual-interface noise instead of dropping it.
func hostIPs(p hostProbe) []wire.HostIP {
	ifs, err := p.interfaces()
	if err != nil {
		return nil
	}
	var out []wire.HostIP
	for _, i := range ifs {
		if i.Flags&net.FlagLoopback != 0 {
			continue
		}
		for _, a := range i.Addrs {
			var ip net.IP
			switch v := a.(type) {
			case *net.IPNet:
				ip = v.IP
			case *net.IPAddr:
				ip = v.IP
			}
			if ip == nil || ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsUnspecified() || ip.IsMulticast() {
				continue
			}
			out = append(out, wire.HostIP{Iface: i.Name, Addr: ip.String(), Virtual: virtualIface(i.Name)})
		}
	}
	return out
}

func osPrettyName(p hostProbe) string {
	for _, path := range []string{"/etc/os-release", "/usr/lib/os-release"} {
		b, err := p.readFile(path)
		if err != nil {
			continue
		}
		sc := bufio.NewScanner(strings.NewReader(string(b)))
		for sc.Scan() {
			if v, ok := strings.CutPrefix(sc.Text(), "PRETTY_NAME="); ok {
				return strings.Trim(strings.TrimSpace(v), `"'`)
			}
		}
	}
	return p.goos
}

func kernelRelease(p hostProbe) string {
	b, err := p.readFile("/proc/sys/kernel/osrelease")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

func uptimeSeconds(p hostProbe) int64 {
	b, err := p.readFile("/proc/uptime")
	if err != nil {
		return 0
	}
	f := strings.Fields(string(b))
	if len(f) == 0 {
		return 0
	}
	v, err := strconv.ParseFloat(f[0], 64)
	if err != nil {
		return 0
	}
	return int64(v)
}

// memInfo parses MemTotal and MemAvailable (kB) from /proc/meminfo.
func memInfo(p hostProbe) (total, avail int64) {
	b, err := p.readFile("/proc/meminfo")
	if err != nil {
		return 0, 0
	}
	for _, line := range strings.Split(string(b), "\n") {
		name, rest, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		f := strings.Fields(rest)
		if len(f) == 0 {
			continue
		}
		kb, err := strconv.ParseInt(f[0], 10, 64)
		if err != nil {
			continue
		}
		switch name {
		case "MemTotal":
			total = kb * 1024
		case "MemAvailable":
			avail = kb * 1024
		}
	}
	return total, avail
}

// hostWorkDir is the directory whose disk the host report covers: the
// foreman's data dir (where workers run and jobs accumulate).
func (a *App) hostWorkDir() string {
	if a.cfg.DataDir == "" {
		return ""
	}
	if abs, err := filepath.Abs(a.cfg.DataDir); err == nil {
		return abs
	}
	return a.cfg.DataDir
}

// hostFacts collects the current host report with the real probes.
func (a *App) hostFacts() wire.HostInfo {
	return collectHostFacts(realHostProbe(), a.hostWorkDir())
}
