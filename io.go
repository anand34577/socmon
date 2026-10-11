//go:build linux

package main

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
)

// ── storage ─────────────────────────────────────────────────────────────────

type Disk struct {
	Name      string  `json:"name"`
	Kind      string  `json:"kind"` // NVMe | eMMC | SD card | USB | SATA | RAID
	Model     string  `json:"model,omitempty"`
	Size      uint64  `json:"size"`
	ReadBps   float64 `json:"read_bps"`
	WriteBps  float64 `json:"write_bps"`
	ReadIOPS  float64 `json:"read_iops"`
	WriteIOPS float64 `json:"write_iops"`
	Util      float64 `json:"util"` // share of time with I/O in flight
	Temp      float64 `json:"temp_c,omitempty"`
}

type Mount struct {
	Path   string `json:"path"`
	Source string `json:"source"`
	FS     string `json:"fs"`
	Disk   string `json:"disk,omitempty"` // whole-disk name the filesystem lives on
	Total  uint64 `json:"total"`
	Used   uint64 `json:"used"`
	Avail  uint64 `json:"avail"`
}

type diskMeta struct {
	ok          bool
	kind, model string
	size        uint64
	temp        string
}

func (c *Collector) diskMeta(name string) *diskMeta {
	if m, ok := c.diskInfo[name]; ok {
		return m
	}
	m := &diskMeta{}
	c.diskInfo[name] = m
	for _, p := range []string{"loop", "ram", "zram", "dm-", "sr", "fd", "nbd", "mtd"} {
		if strings.HasPrefix(name, p) {
			return m
		}
	}
	if strings.Contains(name, "boot") || strings.Contains(name, "rpmb") {
		return m
	}
	base := "/sys/block/" + name
	sz, ok := readInt(base + "/size")
	if !ok || sz == 0 {
		return m
	}
	m.ok, m.size = true, uint64(sz)*512
	switch {
	case strings.HasPrefix(name, "nvme"):
		m.kind = "NVMe"
	case strings.HasPrefix(name, "mmcblk"):
		m.kind = "eMMC"
		if readStr(base+"/device/type") == "SD" {
			m.kind = "SD card"
		}
	case strings.HasPrefix(name, "sd"):
		m.kind = "SATA"
		if real, _ := filepath.EvalSymlinks(base); strings.Contains(real, "/usb") {
			m.kind = "USB"
		}
	case strings.HasPrefix(name, "md"):
		m.kind = "RAID"
	default:
		m.kind = "disk"
	}
	m.model = readStr(base + "/device/model")
	if m.model == "" {
		m.model = readStr(base + "/device/name") // MMC card name
	}
	for _, pat := range []string{base + "/device/hwmon/hwmon*/temp1_input", base + "/device/device/hwmon/hwmon*/temp1_input", base + "/device/hwmon*/temp1_input"} {
		if g, _ := filepath.Glob(pat); len(g) > 0 {
			m.temp = g[0]
			break
		}
	}
	return m
}

func (c *Collector) disks(dt float64) (out []Disk, rd, wr float64) {
	b, _ := os.ReadFile("/proc/diskstats")
	next := map[string][5]uint64{}
	for _, l := range strings.Split(string(b), "\n") {
		f := strings.Fields(l)
		if len(f) < 14 {
			continue
		}
		m := c.diskMeta(f[2])
		if !m.ok {
			continue
		}
		u := func(i int) uint64 { v, _ := strconv.ParseUint(f[i], 10, 64); return v }
		cur := [5]uint64{u(3), u(5), u(7), u(9), u(12)} // reads, sectors read, writes, sectors written, io ms
		next[f[2]] = cur
		d := Disk{Name: f[2], Kind: m.kind, Model: m.model, Size: m.size}
		if m.temp != "" {
			if t, ok := readInt(m.temp); ok {
				d.Temp = float64(t) / 1000
			}
		}
		if p, ok := c.prevDisk[f[2]]; ok && dt > 0 && cur[0] >= p[0] && cur[2] >= p[2] {
			d.ReadIOPS = float64(cur[0]-p[0]) / dt
			d.ReadBps = float64(cur[1]-p[1]) * 512 / dt
			d.WriteIOPS = float64(cur[2]-p[2]) / dt
			d.WriteBps = float64(cur[3]-p[3]) * 512 / dt
			d.Util = clampPct(float64(cur[4]-p[4]) / (dt * 1000) * 100)
		}
		rd += d.ReadBps
		wr += d.WriteBps
		out = append(out, d)
	}
	c.prevDisk = next
	return
}

var realFS = map[string]bool{"ext2": true, "ext3": true, "ext4": true, "xfs": true, "btrfs": true, "f2fs": true,
	"vfat": true, "exfat": true, "ntfs": true, "ntfs3": true, "fuseblk": true, "zfs": true, "jfs": true, "bcachefs": true}

// blockDisk maps a block device (partition, dm or whole disk) to its disk.
func blockDisk(name string, depth int) string {
	base := "/sys/class/block/" + name
	if sl, _ := os.ReadDir(base + "/slaves"); len(sl) > 0 && depth < 4 {
		return blockDisk(sl[0].Name(), depth+1)
	}
	if exists(base + "/partition") {
		if real, err := filepath.EvalSymlinks(base); err == nil {
			return filepath.Base(filepath.Dir(real))
		}
	}
	return name
}

// mounts lists real filesystems once each (bind mounts and ZFS datasets of
// one pool collapse to the shortest path), root first.
func mounts() []Mount {
	byKey := map[string]Mount{}
	for _, l := range strings.Split(readStr("/proc/self/mounts"), "\n") {
		f := strings.Fields(l)
		if len(f) < 3 || !realFS[f[2]] {
			continue
		}
		path := strings.ReplaceAll(f[1], `\040`, " ")
		skip := false
		for _, p := range []string{"/proc", "/sys", "/dev", "/run", "/snap", "/var/lib/docker", "/var/lib/containers", "/var/lib/kubelet"} {
			if path == p || strings.HasPrefix(path, p+"/") {
				skip = true
			}
		}
		if skip {
			continue
		}
		var st syscall.Stat_t
		if syscall.Stat(path, &st) != nil {
			continue
		}
		m := Mount{Path: path, Source: f[0], FS: f[2]}
		key := fmt.Sprint(st.Dev)
		if f[2] == "zfs" {
			pool, _, _ := strings.Cut(f[0], "/")
			key, m.Disk = "zfs:"+pool, "zfs:"+pool
		} else {
			maj := (st.Dev>>8)&0xfff | (st.Dev>>32)&^0xfff
			mnr := st.Dev&0xff | (st.Dev>>12)&^0xff
			if link, err := os.Readlink(fmt.Sprintf("/sys/dev/block/%d:%d", maj, mnr)); err == nil {
				m.Disk = blockDisk(filepath.Base(link), 0)
			}
		}
		if prev, ok := byKey[key]; ok && len(prev.Path) <= len(path) {
			continue
		}
		var fs syscall.Statfs_t
		if syscall.Statfs(path, &fs) != nil || fs.Blocks == 0 {
			continue
		}
		bs := uint64(fs.Bsize)
		m.Total, m.Avail, m.Used = fs.Blocks*bs, fs.Bavail*bs, (fs.Blocks-fs.Bfree)*bs
		byKey[key] = m
	}
	out := make([]Mount, 0, len(byKey))
	for _, m := range byKey {
		out = append(out, m)
	}
	sort.Slice(out, func(i, j int) bool {
		if (out[i].Path == "/") != (out[j].Path == "/") {
			return out[i].Path == "/"
		}
		return out[i].Path < out[j].Path
	})
	return out
}

// ── network ─────────────────────────────────────────────────────────────────

type Iface struct {
	Name      string   `json:"name"`
	Kind      string   `json:"kind"` // Ethernet | Wi-Fi | bridge | bond | VLAN | virtual
	Physical  bool     `json:"physical"`
	Driver    string   `json:"driver,omitempty"`
	MAC       string   `json:"mac,omitempty"`
	Master    string   `json:"master,omitempty"` // bridge/bond this port belongs to
	Ports     []string `json:"ports,omitempty"`
	Up        bool     `json:"up"`
	Carrier   bool     `json:"carrier"`
	SpeedMbps int64    `json:"speed_mbps,omitempty"`
	Duplex    string   `json:"duplex,omitempty"`
	MTU       int64    `json:"mtu"`
	Signal    int      `json:"signal_dbm,omitempty"`
	IPs       []string `json:"ips,omitempty"`
	RxBps     float64  `json:"rx_bps"`
	TxBps     float64  `json:"tx_bps"`
	RxTotal   uint64   `json:"rx_total"`
	TxTotal   uint64   `json:"tx_total"`
	Errors    uint64   `json:"errors"`
	Drops     uint64   `json:"drops"`
}

var virtualIf = []string{"lo", "docker", "veth", "br-", "tap", "fwbr", "fwpr", "fwln", "virbr", "lxc", "tun", "p2p", "dummy", "sit", "ip6tnl", "gre", "erspan", "can", "wg", "cni", "flannel", "kube"}

func ipMap() map[string][]string {
	out := map[string][]string{}
	ifs, _ := net.Interfaces()
	for _, i := range ifs {
		addrs, _ := i.Addrs()
		for _, a := range addrs {
			if ipn, ok := a.(*net.IPNet); ok && ipn.IP.To4() != nil {
				ones, _ := ipn.Mask.Size()
				out[i.Name] = append(out[i.Name], fmt.Sprintf("%s/%d", ipn.IP, ones))
			}
		}
	}
	return out
}

func wifiSignal() map[string]int {
	out := map[string]int{}
	for _, l := range strings.Split(readStr("/proc/net/wireless"), "\n") {
		name, rest, ok := strings.Cut(l, ":")
		f := strings.Fields(rest)
		if !ok || len(f) < 3 {
			continue
		}
		if v, err := strconv.ParseFloat(strings.TrimSuffix(f[2], "."), 64); err == nil && v != 0 {
			out[strings.TrimSpace(name)] = int(v)
		}
	}
	return out
}

// ifaces reports every interface someone would connect through: physical
// ports, plus bridges/VLANs that carry an address. Totals count physical
// ports only, so traffic forwarded through a bridge is not counted twice.
func (c *Collector) ifaces(dt float64) (out []Iface, rx, tx float64) {
	b, _ := os.ReadFile("/proc/net/dev")
	ips, sig := ipMap(), wifiSignal()
	next := map[string][2]uint64{}
	var allRx, allTx float64
	for _, l := range strings.Split(string(b), "\n") {
		name, rest, ok := strings.Cut(l, ":")
		f := strings.Fields(rest)
		if !ok || len(f) < 16 {
			continue
		}
		name = strings.TrimSpace(name)
		virtual := false
		for _, p := range virtualIf {
			if strings.HasPrefix(name, p) {
				virtual = true
				break
			}
		}
		base := "/sys/class/net/" + name
		phys := exists(base + "/device")
		if virtual || (!phys && len(ips[name]) == 0) {
			continue
		}
		u := func(i int) uint64 { v, _ := strconv.ParseUint(f[i], 10, 64); return v }
		it := Iface{Name: name, Physical: phys, IPs: ips[name], MAC: readStr(base + "/address"),
			RxTotal: u(0), TxTotal: u(8), Errors: u(2) + u(10), Drops: u(3) + u(11)}
		it.MTU, _ = readInt(base + "/mtu")
		it.Up = readStr(base+"/operstate") == "up" || (readStr(base+"/operstate") == "unknown" && readStr(base+"/carrier") == "1")
		// A Wi-Fi radio that is not associated reports carrier=1 but "dormant".
		it.Carrier = readStr(base+"/carrier") == "1" && readStr(base+"/operstate") != "dormant"
		if drv, err := os.Readlink(base + "/device/driver"); err == nil {
			it.Driver = filepath.Base(drv)
		}
		if m, err := os.Readlink(base + "/master"); err == nil {
			it.Master = filepath.Base(m)
		}
		switch {
		case exists(base+"/wireless") || exists(base+"/phy80211"):
			it.Kind, it.Signal = "Wi-Fi", sig[name]
		case isDir(base + "/bridge"):
			it.Kind = "bridge"
			ps, _ := os.ReadDir(base + "/brif")
			for _, p := range ps {
				it.Ports = append(it.Ports, p.Name())
			}
		case isDir(base + "/bonding"):
			it.Kind = "bond"
			it.Ports = strings.Fields(readStr(base + "/bonding/slaves"))
		case exists("/proc/net/vlan/" + name):
			it.Kind = "VLAN"
		case phys:
			it.Kind = "Ethernet"
		default:
			it.Kind = "virtual"
		}
		// Bridges, VLANs and veths report a made-up 10 Gb/s; only ports and
		// bonds have a real link speed.
		if it.Carrier && (phys || it.Kind == "bond") {
			if sp, ok := readInt(base + "/speed"); ok && sp > 0 {
				it.SpeedMbps = sp
			}
			it.Duplex = readStr(base + "/duplex")
		}
		cur := [2]uint64{it.RxTotal, it.TxTotal}
		next[name] = cur
		if p, ok := c.prevIf[name]; ok && dt > 0 && cur[0] >= p[0] && cur[1] >= p[1] {
			it.RxBps, it.TxBps = float64(cur[0]-p[0])/dt, float64(cur[1]-p[1])/dt
		}
		if phys {
			rx += it.RxBps
			tx += it.TxBps
		}
		allRx += it.RxBps
		allTx += it.TxBps
		out = append(out, it)
	}
	c.prevIf = next
	hasPhys := false
	for _, it := range out {
		hasPhys = hasPhys || it.Physical
	}
	if !hasPhys {
		rx, tx = allRx, allTx
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Carrier != out[j].Carrier {
			return out[i].Carrier
		}
		return out[i].Name < out[j].Name
	})
	return
}

func gateway() string {
	for _, l := range strings.Split(readStr("/proc/net/route"), "\n")[1:] {
		f := strings.Fields(l)
		if len(f) < 3 || f[1] != "00000000" {
			continue
		}
		v, err := strconv.ParseUint(f[2], 16, 32)
		if err != nil {
			continue
		}
		return fmt.Sprintf("%d.%d.%d.%d", v&0xff, v>>8&0xff, v>>16&0xff, v>>24)
	}
	return ""
}

// dnsServer prefers the real upstream over systemd-resolved's stub.
func dnsServer() string {
	stub := ""
	for _, f := range []string{"/run/systemd/resolve/resolv.conf", "/etc/resolv.conf"} {
		for _, l := range strings.Split(readStr(f), "\n") {
			if v, ok := strings.CutPrefix(strings.TrimSpace(l), "nameserver"); ok {
				v = strings.TrimSpace(v)
				if !strings.HasPrefix(v, "127.") {
					return v
				}
				stub = v
			}
		}
	}
	return stub
}

func tcpCounts() (estab, listen int) {
	for _, f := range []string{"/proc/net/tcp", "/proc/net/tcp6"} {
		for _, l := range strings.Split(readStr(f), "\n") {
			fs := strings.Fields(l)
			if len(fs) < 4 {
				continue
			}
			switch fs[3] {
			case "01":
				estab++
			case "0A":
				listen++
			}
		}
	}
	return
}

// ── sensors ─────────────────────────────────────────────────────────────────

type Temp struct {
	Name string  `json:"name"`
	C    float64 `json:"c"`
}

type Fan struct {
	Name string `json:"name"`
	RPM  int64  `json:"rpm"` // -1 when the fan has no tach
	PWM  int    `json:"pwm"` // duty in percent, -1 when unknown
}

// Cooling is a thermal cooling device; cur_state > 0 on a cpufreq/devfreq
// device means that block is being throttled.
type Cooling struct {
	Type string `json:"type"`
	Cur  int64  `json:"cur"`
	Max  int64  `json:"max"`
}

// PowerIn is board input power from a USB-PD controller or a VIN monitor.
type PowerIn struct {
	Name  string  `json:"name"`
	Volts float64 `json:"volts"`
	Amps  float64 `json:"amps"` // -1 when only voltage is measured
}

func sensors() (temps []Temp, fans []Fan, cool []Cooling, power []PowerIn) {
	ents, _ := os.ReadDir("/sys/class/thermal")
	for _, e := range ents {
		p := "/sys/class/thermal/" + e.Name()
		switch {
		case strings.HasPrefix(e.Name(), "thermal_zone"):
			if v, ok := readInt(p + "/temp"); ok {
				temps = append(temps, Temp{readStr(p + "/type"), float64(v) / 1000})
			}
		case strings.HasPrefix(e.Name(), "cooling_device"):
			cur, _ := readInt(p + "/cur_state")
			mx, _ := readInt(p + "/max_state")
			if mx > 0 {
				cool = append(cool, Cooling{readStr(p + "/type"), cur, mx})
			}
		}
	}
	hw, _ := os.ReadDir("/sys/class/hwmon")
	for _, e := range hw {
		p := "/sys/class/hwmon/" + e.Name()
		name := readStr(p + "/name")
		if name == "nvme" || name == "drivetemp" {
			if v, ok := readInt(p + "/temp1_input"); ok {
				n := 0
				for _, t := range temps {
					if strings.HasPrefix(t.Name, name) {
						n++
					}
				}
				temps = append(temps, Temp{fmt.Sprintf("%s%d", name, n), float64(v) / 1000})
			}
		}
		if strings.Contains(name, "psy") || strings.Contains(name, "vin") {
			if mv, ok := readInt(p + "/in0_input"); ok && mv > 0 {
				pw := PowerIn{Name: name, Volts: float64(mv) / 1000, Amps: -1}
				if ma, ok := readInt(p + "/curr1_input"); ok {
					pw.Amps = float64(ma) / 1000
				}
				power = append(power, pw)
			}
		}
		rpm, hasRPM := readInt(p + "/fan1_input")
		pwm, hasPWM := readInt(p + "/pwm1")
		if !hasRPM && !hasPWM {
			continue
		}
		f := Fan{Name: name, RPM: -1, PWM: -1}
		if hasRPM {
			f.RPM = rpm
		}
		if hasPWM {
			f.PWM = int(pwm * 100 / 255)
		}
		fans = append(fans, f)
	}
	return
}
