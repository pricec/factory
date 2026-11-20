package wire

import (
	"math"
	"strings"
)

// Limits applied by HostInfo.Sanitize (the factory never trusts a foreman's
// host report to be small).
const (
	maxHostIPs   = 64
	maxHostDisks = 16
	maxHostStr   = 256
)

// HostInfo is the structured `host` object a foreman reports at pairing and
// on every capabilities push (spec §6.7.1). It is descriptive only - never
// used for job matching (that is what capabilities are for). Every field is
// optional on the wire; a foreman that predates the feature sends none.
type HostInfo struct {
	// HostID identifies the machine: first 12 hex chars of
	// sha256(machine-id), never the raw id. Foremen on the same box share it.
	HostID   string `json:"host_id"`
	Hostname string `json:"hostname,omitempty"`
	// IPs are the non-loopback, non-link-local addresses per interface.
	IPs []HostIP `json:"ips,omitempty"`
	// OS is PRETTY_NAME from /etc/os-release (falls back to GOOS).
	OS     string `json:"os,omitempty"`
	Kernel string `json:"kernel,omitempty"`
	Arch   string `json:"arch,omitempty"`
	CPUs   int    `json:"cpus,omitempty"`
	// MemTotalBytes / MemAvailBytes come from /proc/meminfo.
	MemTotalBytes int64 `json:"mem_total_bytes,omitempty"`
	MemAvailBytes int64 `json:"mem_avail_bytes,omitempty"`
	// SwapTotalBytes / SwapFreeBytes come from /proc/meminfo (0 total = no swap).
	SwapTotalBytes int64 `json:"swap_total_bytes,omitempty"`
	SwapFreeBytes  int64 `json:"swap_free_bytes,omitempty"`
	// Load is the 1/5/15-minute load average (/proc/loadavg); nil when
	// unavailable. A pointer so a genuine 0.00 is still reported.
	Load *HostLoad `json:"load,omitempty"`
	// CPUUsagePct is whole-machine CPU busy % (0-100) over the interval
	// between this foreman's previous two reports (delta of /proc/stat).
	// nil on the first report after start or when unavailable.
	CPUUsagePct *float64 `json:"cpu_usage_pct,omitempty"`
	// CPUModel is the first "model name" from /proc/cpuinfo.
	CPUModel string `json:"cpu_model,omitempty"`
	// PrimaryIP is the first non-virtual address (IPv4 preferred), the one
	// an operator would use to reach the machine; "" when none.
	PrimaryIP string `json:"primary_ip,omitempty"`
	// Disks reports "/" and the foreman's work dir.
	Disks    []HostDisk `json:"disks,omitempty"`
	UptimeS  int64      `json:"uptime_s,omitempty"`
	Reported string     `json:"reported_at,omitempty"` // RFC3339, set by the factory on receipt
}

// HostLoad is the classic 1/5/15-minute load average.
type HostLoad struct {
	One     float64 `json:"1m"`
	Five    float64 `json:"5m"`
	Fifteen float64 `json:"15m"`
}

// HostIP is one interface address. Virtual marks container/bridge noise
// (docker0, br-*, veth*, ...): kept for completeness, but not an address an
// operator would use to reach the machine.
type HostIP struct {
	Iface   string `json:"iface"`
	Addr    string `json:"addr"`
	Virtual bool   `json:"virtual,omitempty"`
}

// HostDisk is a filesystem's capacity for Path.
type HostDisk struct {
	Path       string `json:"path"`
	TotalBytes int64  `json:"total_bytes"`
	FreeBytes  int64  `json:"free_bytes"`
}

func clip(s string) string {
	if len(s) > maxHostStr {
		return s[:maxHostStr]
	}
	return s
}

// Sanitize bounds a reported HostInfo in place and reports whether it is
// usable (it must carry a host_id; without one the foreman cannot be
// grouped, so the report is dropped).
func (h *HostInfo) Sanitize() bool {
	if h == nil {
		return false
	}
	h.HostID = strings.TrimSpace(clip(h.HostID))
	if h.HostID == "" {
		return false
	}
	h.Hostname, h.OS, h.Kernel, h.Arch = clip(h.Hostname), clip(h.OS), clip(h.Kernel), clip(h.Arch)
	h.CPUModel, h.PrimaryIP = clip(h.CPUModel), clip(h.PrimaryIP)
	if h.CPUUsagePct != nil {
		v := math.Min(100, math.Max(0, *h.CPUUsagePct))
		if math.IsNaN(*h.CPUUsagePct) {
			h.CPUUsagePct = nil
		} else {
			h.CPUUsagePct = &v
		}
	}
	if len(h.IPs) > maxHostIPs {
		h.IPs = h.IPs[:maxHostIPs]
	}
	for i := range h.IPs {
		h.IPs[i].Iface, h.IPs[i].Addr = clip(h.IPs[i].Iface), clip(h.IPs[i].Addr)
	}
	if len(h.Disks) > maxHostDisks {
		h.Disks = h.Disks[:maxHostDisks]
	}
	for i := range h.Disks {
		h.Disks[i].Path = clip(h.Disks[i].Path)
	}
	return true
}
