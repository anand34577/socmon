//go:build linux

package main

import (
	"bufio"
	"math"
	"os"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
)

type Core struct {
	ID      int     `json:"id"`
	Usage   float64 `json:"usage"`
	FreqKHz int64   `json:"freq_khz"`
	MaxKHz  int64   `json:"max_khz"`
	Type    string  `json:"type,omitempty"` // e.g. "A76", from MIDR part number
}

// Cluster is one cpufreq policy: on big.LITTLE parts (A733, RK3588, RK3576)
// the big and little cores scale independently.
type Cluster struct {
	CPUs     []int   `json:"cpus"`
	Type     string  `json:"type,omitempty"`
	Governor string  `json:"governor,omitempty"`
	CurKHz   int64   `json:"cur_khz"`
	MinKHz   int64   `json:"min_khz"`
	MaxKHz   int64   `json:"max_khz"`
	CapKHz   int64   `json:"cap_khz"` // scaling_max_freq; below MaxKHz when something limits the cluster
	Usage    float64 `json:"usage"`
}

// CPUTimes splits whole-chip CPU time by where it went, in percent.
type CPUTimes struct {
	User   float64 `json:"user"`
	System float64 `json:"system"`
	IOWait float64 `json:"iowait"`
	IRQ    float64 `json:"irq"`
	Steal  float64 `json:"steal"`
}

type CPUStat struct {
	Total    float64   `json:"total"`
	Times    CPUTimes  `json:"times"`
	Cores    []Core    `json:"cores"`
	Clusters []Cluster `json:"clusters,omitempty"`
	Running  int       `json:"procs_running"`
	Blocked  int       `json:"procs_blocked"`
	CtxtRate float64   `json:"ctxt_per_s"`
	IntrRate float64   `json:"intr_per_s"`
}

type MemStat struct {
	Total     uint64 `json:"total"`
	Used      uint64 `json:"used"` // Total - Available
	Free      uint64 `json:"free"`
	Available uint64 `json:"available"`
	Cached    uint64 `json:"cached"` // page cache + reclaimable slab
	Buffers   uint64 `json:"buffers"`
	Shared    uint64 `json:"shared"`
	Dirty     uint64 `json:"dirty"`
	SwapTotal uint64 `json:"swap_total"`
	SwapUsed  uint64 `json:"swap_used"`
	CmaTotal  uint64 `json:"cma_total,omitempty"` // contiguous pool the VPU/ISP/display allocate from
	CmaFree   uint64 `json:"cma_free,omitempty"`
	CmaUsed   uint64 `json:"cma_used,omitempty"`   // Rockchip BSP: CmaAllocated (its CmaFree excludes released pages)
	ZramOrig  uint64 `json:"zram_orig,omitempty"`  // data stored in zram swap
	ZramCompr uint64 `json:"zram_compr,omitempty"` // RAM that data takes once compressed
}

type Snapshot struct {
	Time      time.Time    `json:"time"`
	Board     BoardInfo    `json:"board"`
	OS        string       `json:"os,omitempty"`
	Uptime    float64      `json:"uptime_s"`
	Load      [3]float64   `json:"loadavg"`
	CPU       CPUStat      `json:"cpu"`
	Mem       MemStat      `json:"mem"`
	Accels    []Accel      `json:"accelerators"`
	Media     []MediaBlock `json:"media_engines,omitempty"`
	Temps     []Temp       `json:"temps"`
	Fans      []Fan        `json:"fans,omitempty"`
	Cooling   []Cooling    `json:"cooling,omitempty"`
	Power     []PowerIn    `json:"power_in,omitempty"`
	NetRx     float64      `json:"net_rx_bytes_per_s"`
	NetTx     float64      `json:"net_tx_bytes_per_s"`
	Ifaces    []Iface      `json:"interfaces,omitempty"`
	Gateway   string       `json:"gateway,omitempty"`
	DNS       string       `json:"dns,omitempty"`
	TCPEstab  int          `json:"tcp_established"`
	TCPListen int          `json:"tcp_listening"`
	DiskRead  float64      `json:"disk_read_bytes_per_s"`
	DiskWrite float64      `json:"disk_write_bytes_per_s"`
	Disks     []Disk       `json:"disks,omitempty"`
	Mounts    []Mount      `json:"mounts,omitempty"`
	DiskTotal uint64       `json:"disk_total"` // root filesystem
	DiskUsed  uint64       `json:"disk_used"`
	Host      string       `json:"hostname"`
	Tasks     int          `json:"tasks"`
	Threads   int          `json:"threads"`
	Root      bool         `json:"root"` // debugfs and other users' fds are only visible to root
	Procs     []Proc       `json:"procs,omitempty"`
}

type cpuTick struct{ busy, total uint64 }

type Collector struct {
	board    BoardInfo
	osName   string
	last     time.Time
	prevCPU  map[string]cpuTick
	prevAll  [8]uint64 // aggregate cpu line: user nice system idle iowait irq softirq steal
	prevCtxt [2]uint64 // ctxt, intr
	devfreq  []devfreqDev
	coreType map[int]string
	gpuClk   string // debugfs clk dir for the GPU core clock, when not on devfreq
	gpuPM    pmTracker
	gpuInfo  string // Mali gpuinfo file, e.g. "Mali-G610 4 cores r0p0 0xA867"
	prevNets map[string]uint64
	media    []*mediaDev

	prevProc map[int]procPrev
	prevDRM  map[int]map[string]uint64
	users    map[uint32]string
	devCls   map[string]string

	prevDisk map[string][5]uint64
	diskInfo map[string]*diskMeta
	prevIf   map[string][2]uint64
}

func NewCollector() *Collector {
	c := &Collector{
		board: detectBoard(), osName: osRelease(),
		prevCPU: map[string]cpuTick{}, prevProc: map[int]procPrev{}, prevDRM: map[int]map[string]uint64{},
		devCls: map[string]string{}, users: passwd(),
		prevDisk: map[string][5]uint64{}, diskInfo: map[string]*diskMeta{}, prevIf: map[string][2]uint64{},
	}
	c.discoverDevfreq()
	c.coreType = coreTypes()
	c.gpuClk = findClk("gpu")
	c.gpuPM.dir = findGPUDev()
	c.gpuInfo = findGPUInfo()
	c.discoverMedia()
	return c
}

func osRelease() string {
	for _, l := range strings.Split(readStr("/etc/os-release"), "\n") {
		if v, ok := strings.CutPrefix(l, "PRETTY_NAME="); ok {
			return strings.Trim(v, `"`)
		}
	}
	return ""
}

func isDir(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && fi.IsDir()
}

func exists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

func clampPct(v float64) float64 { return math.Max(0, math.Min(100, v)) }

const clkTck = 100 // USER_HZ is 100 on all Linux arm64 kernels

// ARM MIDR part numbers for the cores found on the supported SoCs.
var armParts = map[string]string{
	"0xd03": "A53", "0xd04": "A35", "0xd05": "A55", "0xd07": "A57", "0xd08": "A72",
	"0xd09": "A73", "0xd0a": "A75", "0xd0b": "A76", "0xd0d": "A77", "0xd41": "A78",
	"0xd46": "A510", "0xd47": "A710", "0xd4d": "A715", "0xd80": "A520", "0xd81": "A720",
}

func coreTypes() map[int]string {
	out := map[int]string{}
	f, err := os.Open("/proc/cpuinfo")
	if err != nil {
		return out
	}
	defer f.Close()
	cur := -1
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		k, v, ok := strings.Cut(sc.Text(), ":")
		if !ok {
			continue
		}
		k, v = strings.TrimSpace(k), strings.TrimSpace(v)
		switch k {
		case "processor":
			cur, _ = strconv.Atoi(v)
		case "CPU part":
			if t, ok := armParts[strings.ToLower(v)]; ok && cur >= 0 {
				out[cur] = t
			}
		}
	}
	return out
}

func clusters(cores []Core) []Cluster {
	byID := map[int]Core{}
	for _, k := range cores {
		byID[k.ID] = k
	}
	pols, _ := os.ReadDir("/sys/devices/system/cpu/cpufreq")
	var out []Cluster
	for _, p := range pols {
		if !strings.HasPrefix(p.Name(), "policy") {
			continue
		}
		base := "/sys/devices/system/cpu/cpufreq/" + p.Name() + "/"
		cl := Cluster{Governor: readStr(base + "scaling_governor")}
		cl.CurKHz, _ = readInt(base + "scaling_cur_freq")
		cl.MinKHz, _ = readInt(base + "cpuinfo_min_freq")
		cl.MaxKHz, _ = readInt(base + "cpuinfo_max_freq")
		cl.CapKHz, _ = readInt(base + "scaling_max_freq")
		sum, n := 0.0, 0
		for _, s := range strings.Fields(readStr(base + "related_cpus")) {
			id, err := strconv.Atoi(s)
			if err != nil {
				continue
			}
			cl.CPUs = append(cl.CPUs, id)
			if k, ok := byID[id]; ok {
				sum += k.Usage
				n++
				if cl.Type == "" {
					cl.Type = k.Type
				}
			}
		}
		if n > 0 {
			cl.Usage = sum / float64(n)
		}
		if len(cl.CPUs) > 0 {
			out = append(out, cl)
		}
	}
	if len(out) == 0 { // no cpufreq driver (Allwinner A733 vendor kernel): group by core type
		idx := map[string]int{}
		for _, k := range cores {
			i, ok := idx[k.Type]
			if !ok {
				i = len(out)
				idx[k.Type] = i
				out = append(out, Cluster{Type: k.Type})
			}
			out[i].CPUs = append(out[i].CPUs, k.ID)
			out[i].Usage += k.Usage
		}
		for i := range out {
			out[i].Usage /= float64(len(out[i].CPUs))
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CPUs[0] < out[j].CPUs[0] })
	return out
}

func (c *Collector) cpu(dt float64) CPUStat {
	var st CPUStat
	f, err := os.Open("/proc/stat")
	if err != nil {
		return st
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 1024*1024) // the intr line is long
	var ctxt, intr uint64
	for sc.Scan() {
		fs := strings.Fields(sc.Text())
		if len(fs) < 2 {
			continue
		}
		switch fs[0] {
		case "procs_running":
			st.Running, _ = strconv.Atoi(fs[1])
			continue
		case "procs_blocked":
			st.Blocked, _ = strconv.Atoi(fs[1])
			continue
		case "ctxt":
			ctxt, _ = strconv.ParseUint(fs[1], 10, 64)
			continue
		case "intr":
			intr, _ = strconv.ParseUint(fs[1], 10, 64)
			continue
		}
		if len(fs) < 8 || !strings.HasPrefix(fs[0], "cpu") {
			continue
		}
		var raw [8]uint64
		var tot, idle uint64
		for i, s := range fs[1:] {
			v, _ := strconv.ParseUint(s, 10, 64)
			if i < 8 {
				raw[i] = v
				tot += v
			}
			if i == 3 || i == 4 {
				idle += v
			}
		}
		cur := cpuTick{tot - idle, tot}
		prev := c.prevCPU[fs[0]]
		c.prevCPU[fs[0]] = cur
		u := 0.0
		// iowait can go backwards on Linux, so guard busy as well as total.
		if cur.total > prev.total && cur.busy >= prev.busy {
			u = math.Min(100, float64(cur.busy-prev.busy)/float64(cur.total-prev.total)*100)
		}
		if fs[0] == "cpu" {
			st.Total = u
			st.Times = c.cpuTimes(raw)
			continue
		}
		id, _ := strconv.Atoi(fs[0][3:])
		base := "/sys/devices/system/cpu/" + fs[0] + "/cpufreq/"
		fr, _ := readInt(base + "scaling_cur_freq")
		mx, _ := readInt(base + "cpuinfo_max_freq")
		st.Cores = append(st.Cores, Core{id, u, fr, mx, c.coreType[id]})
	}
	if c.prevCtxt[0] != 0 && dt > 0 && ctxt >= c.prevCtxt[0] && intr >= c.prevCtxt[1] {
		st.CtxtRate = float64(ctxt-c.prevCtxt[0]) / dt
		st.IntrRate = float64(intr-c.prevCtxt[1]) / dt
	}
	c.prevCtxt = [2]uint64{ctxt, intr}
	st.Clusters = clusters(st.Cores)
	return st
}

func (c *Collector) cpuTimes(raw [8]uint64) CPUTimes {
	prev := c.prevAll
	c.prevAll = raw
	var d [8]float64
	tot := 0.0
	for i := range raw {
		if raw[i] >= prev[i] {
			d[i] = float64(raw[i] - prev[i])
		}
		tot += d[i]
	}
	if prev[0] == 0 || tot == 0 {
		return CPUTimes{}
	}
	p := func(v float64) float64 { return v / tot * 100 }
	return CPUTimes{User: p(d[0] + d[1]), System: p(d[2]), IOWait: p(d[4]), IRQ: p(d[5] + d[6]), Steal: p(d[7])}
}

func (c *Collector) mem() MemStat {
	f, err := os.Open("/proc/meminfo")
	if err != nil {
		return MemStat{}
	}
	defer f.Close()
	m := map[string]uint64{}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fs := strings.Fields(sc.Text())
		if len(fs) >= 2 {
			v, _ := strconv.ParseUint(fs[1], 10, 64)
			m[strings.TrimSuffix(fs[0], ":")] = v * 1024
		}
	}
	ms := MemStat{Total: m["MemTotal"], Free: m["MemFree"], Available: m["MemAvailable"],
		Cached: m["Cached"] + m["SReclaimable"], Buffers: m["Buffers"], Shared: m["Shmem"], Dirty: m["Dirty"],
		SwapTotal: m["SwapTotal"], SwapUsed: m["SwapTotal"] - m["SwapFree"], CmaTotal: m["CmaTotal"], CmaFree: m["CmaFree"]}
	ms.Used = ms.Total - ms.Available
	if v, ok := m["CmaAllocated"]; ok {
		ms.CmaUsed = v
	} else if ms.CmaTotal >= ms.CmaFree {
		ms.CmaUsed = ms.CmaTotal - ms.CmaFree
	}
	// mm_stat: orig_data_size compr_data_size mem_used_total ...
	zs, _ := os.ReadDir("/sys/block")
	for _, z := range zs {
		if !strings.HasPrefix(z.Name(), "zram") {
			continue
		}
		if f := strings.Fields(readStr("/sys/block/" + z.Name() + "/mm_stat")); len(f) >= 3 {
			o, _ := strconv.ParseUint(f[0], 10, 64)
			u, _ := strconv.ParseUint(f[2], 10, 64)
			ms.ZramOrig += o
			ms.ZramCompr += u
		}
	}
	return ms
}

func (c *Collector) Sample(withProcs bool) *Snapshot {
	now := time.Now()
	dt := 0.0
	if !c.last.IsZero() {
		dt = now.Sub(c.last).Seconds()
	}
	c.last = now
	s := &Snapshot{Time: now, Board: c.board, OS: c.osName, CPU: c.cpu(dt), Mem: c.mem(), Root: os.Geteuid() == 0}
	drmGPU := -1.0
	if withProcs {
		s.Procs, drmGPU = c.procs(dt, s.Mem.Total)
		s.Tasks = len(s.Procs)
		for _, p := range s.Procs {
			s.Threads += p.Threads
		}
	}
	var gpuMem map[int]uint64
	s.Accels, gpuMem = c.accels(dt, drmGPU)
	for i := range s.Procs {
		s.Procs[i].GPUMem = gpuMem[s.Procs[i].PID]
	}
	s.Media = c.mediaSample(dt)
	s.Temps, s.Fans, s.Cooling, s.Power = sensors()
	s.Ifaces, s.NetRx, s.NetTx = c.ifaces(dt)
	s.Gateway, s.DNS = gateway(), dnsServer()
	s.TCPEstab, s.TCPListen = tcpCounts()
	s.Disks, s.DiskRead, s.DiskWrite = c.disks(dt)
	s.Mounts = mounts()
	s.Host, _ = os.Hostname()
	if up := strings.Fields(readStr("/proc/uptime")); len(up) > 0 {
		s.Uptime, _ = strconv.ParseFloat(up[0], 64)
	}
	if la := strings.Fields(readStr("/proc/loadavg")); len(la) >= 3 {
		for i := 0; i < 3; i++ {
			s.Load[i], _ = strconv.ParseFloat(la[i], 64)
		}
	}
	var fs syscall.Statfs_t
	if syscall.Statfs("/", &fs) == nil {
		s.DiskTotal = fs.Blocks * uint64(fs.Bsize)
		s.DiskUsed = (fs.Blocks - fs.Bfree) * uint64(fs.Bsize)
	}
	return s
}
