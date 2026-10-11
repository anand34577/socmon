//go:build linux

package main

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

type Accel struct {
	Kind      string    `json:"kind"` // GPU | NPU | DDR
	Name      string    `json:"name"`
	Load      float64   `json:"load"`                // -1 when the kernel does not expose it
	Estimated bool      `json:"estimated,omitempty"` // Load derived from power-on time, not a busy counter
	PerCore   []float64 `json:"per_core,omitempty"`
	FreqHz    int64     `json:"freq_hz"`
	MinHz     int64     `json:"min_hz,omitempty"`
	MaxHz     int64     `json:"max_hz"`
	Governor  string    `json:"governor,omitempty"`
	Cores     int       `json:"cores,omitempty"`
	Driver    string    `json:"driver,omitempty"`
	Note      string    `json:"note,omitempty"`
	State     string    `json:"state,omitempty"` // e.g. "idle · 0 queued"
	MemUsed   uint64    `json:"mem_used,omitempty"`
	MemPeak   uint64    `json:"mem_peak,omitempty"`
	Nets      []NPUNet  `json:"networks,omitempty"`
}

// NPUNet is one loaded model as reported by viplite's rt_net_profile.
type NPUNet struct {
	Name      string  `json:"name"` // the driver truncates names to 15 chars
	Instances int     `json:"instances"`
	Count     uint64  `json:"infer_count"`
	AvgUs     int64   `json:"avg_infer_us"`
	Rate      float64 `json:"infer_per_s"`
}

// MediaBlock is a fixed-function video/2D engine (Rockchip MPP: RKVDEC,
// RKVENC, VDPU, AV1D, JPEG, RGA, IEP; Allwinner: the Cedar VE).
type MediaBlock struct {
	Name      string  `json:"name"`   // "RKVDEC 0"
	Role      string  `json:"role"`   // "decode"
	Dev       string  `json:"device"` // platform device, e.g. fdc38100.rkvdec-core
	Status    string  `json:"status,omitempty"`
	Busy      float64 `json:"busy"` // -1 when unknown
	Estimated bool    `json:"estimated,omitempty"`
	FreqHz    int64   `json:"freq_hz,omitempty"`
}

type devfreqDev struct{ path, name, kind string }

type mediaDev struct {
	blk     MediaBlock
	pm      pmTracker
	devfreq string
}

const (
	pvrDir   = "/sys/kernel/debug/pvr"
	vipDir   = "/sys/kernel/debug/viplite"
	clkDir   = "/sys/kernel/debug/clk"
	rknpuDir = "/sys/kernel/debug/rknpu"
	maliDbg  = "/sys/kernel/debug/mali0"
)

// pmTracker approximates a block's load as the share of time it was
// runtime-resumed since the last sample. Valid only if the driver actually
// suspends the device when idle; until it has seen a suspend it reports
// nothing rather than a fake 100%.
type pmTracker struct {
	dir  string
	prev [2]int64 // runtime_active_time, runtime_suspended_time (ms)
	seen bool
}

func (p *pmTracker) busy() (float64, bool) {
	if p.dir == "" {
		return -1, false
	}
	act, ok1 := readInt(p.dir + "/power/runtime_active_time")
	sus, ok2 := readInt(p.dir + "/power/runtime_suspended_time")
	if !ok1 || !ok2 {
		return -1, false
	}
	prev := p.prev
	p.prev = [2]int64{act, sus}
	if sus > 0 {
		p.seen = true
	}
	if !p.seen || prev[0] == 0 || act < prev[0] || sus < prev[1] {
		return -1, false
	}
	da, ds := act-prev[0], sus-prev[1]
	if da+ds <= 0 {
		return -1, false
	}
	return clampPct(float64(da) / float64(da+ds) * 100), true
}

func findGPUDev() string {
	for _, pat := range []string{"/sys/devices/platform/*/*.gpu", "/sys/devices/platform/*.gpu"} {
		m, _ := filepath.Glob(pat)
		for _, d := range m {
			if _, ok := readInt(d + "/power/runtime_active_time"); ok {
				return d
			}
		}
	}
	return ""
}

func findGPUInfo() string {
	for _, pat := range []string{"/sys/class/misc/mali0/device/gpuinfo", "/sys/devices/platform/*gpu*/gpuinfo", "/sys/devices/platform/*/*gpu*/gpuinfo"} {
		if m, _ := filepath.Glob(pat); len(m) > 0 {
			return m[0]
		}
	}
	return ""
}

// findClk picks the fastest debugfs clock whose name contains sub, skipping
// bus/interface clocks, which is the core clock on sunxi parts.
func findClk(sub string) string {
	ents, _ := os.ReadDir(clkDir)
	best, bestRate := "", int64(0)
	for _, e := range ents {
		n := strings.ToLower(e.Name())
		if !strings.Contains(n, sub) {
			continue
		}
		skip := false
		// "pll-gpu" (840MHz) feeds the divider; "gpu0" (600MHz) is the core clock.
		for _, b := range []string{"pll", "auto", "bus", "ahb", "apb", "axi", "mbus", "gate", "rst"} {
			if strings.Contains(n, b) {
				skip = true
				break
			}
		}
		if skip {
			continue
		}
		if r, ok := readInt(clkDir + "/" + e.Name() + "/clk_rate"); ok && r > bestRate {
			best, bestRate = clkDir+"/"+e.Name(), r
		}
	}
	return best
}

// accel returns the first entry of kind, appending a placeholder if none exists.
func accelOf(out *[]Accel, kind, name string) *Accel {
	for i := range *out {
		if (*out)[i].Kind == kind {
			return &(*out)[i]
		}
	}
	*out = append(*out, Accel{Kind: kind, Name: name, Load: -1})
	return &(*out)[len(*out)-1]
}

var (
	// core_loading: "NPU Loading -----> Core0:  1%"
	//               "Current Loading -----> Core0: Idle, Task Count=0, Estimated Time=0 us"
	reVipLoad  = regexp.MustCompile(`Core(\d+):\s*(\d+)\s*%`)
	reVipState = regexp.MustCompile(`Core\d+:\s*([A-Za-z]+),\s*Task Count=(\d+)`)
)

// viplite fills NPU stats from the Allwinner/VeriSilicon viplite debugfs
// (formats verified on Radxa Cubie A7A, VIP9000 "ver1=0x9000").
func (c *Collector) viplite(a *Accel, dt float64) {
	cl := readStr(vipDir + "/core_loading")
	if a.Load < 0 {
		var per []float64
		sum := 0.0
		for _, m := range reVipLoad.FindAllStringSubmatch(cl, -1) {
			v, _ := strconv.ParseFloat(m[2], 64)
			per, sum = append(per, v), sum+v
		}
		if len(per) > 0 {
			a.Load, a.PerCore = sum/float64(len(per)), per
		}
	}
	var states []string
	for _, m := range reVipState.FindAllStringSubmatch(cl, -1) {
		states = append(states, strings.ToLower(m[1])+" · "+m[2]+" queued")
	}
	a.State = strings.Join(states, " / ")

	// mem_profile data row: "|  0x0329aff8  |  0x0329aff8  |  64  |  0"
	for _, l := range strings.Split(readStr(vipDir+"/mem_profile"), "\n") {
		f := strings.Split(l, "|")
		if len(f) >= 3 && strings.HasPrefix(strings.TrimSpace(f[1]), "0x") {
			a.MemUsed, _ = strconv.ParseUint(strings.TrimPrefix(strings.TrimSpace(f[1]), "0x"), 16, 64)
			a.MemPeak, _ = strconv.ParseUint(strings.TrimPrefix(strings.TrimSpace(f[2]), "0x"), 16, 64)
			break
		}
	}

	// rt_net_profile rows:
	// "|  0x80080000  |  1  | 0  |  vehicle_moving_  |  1231  |  11698  us  |  11707030  |"
	next := map[string]uint64{}
	for _, l := range strings.Split(readStr(vipDir+"/rt_net_profile"), "\n") {
		f := strings.Split(l, "|")
		if len(f) < 8 || !strings.HasPrefix(strings.TrimSpace(f[1]), "0x") {
			continue
		}
		handle := strings.TrimSpace(f[1])
		cnt, _ := strconv.ParseUint(strings.TrimSpace(f[5]), 10, 64)
		n := NPUNet{Name: strings.TrimSpace(f[4]), Instances: 1, Count: cnt}
		if avg := strings.Fields(f[6]); len(avg) > 0 {
			n.AvgUs, _ = strconv.ParseInt(avg[0], 10, 64)
		}
		if p, ok := c.prevNets[handle]; ok && dt > 0 && cnt >= p {
			n.Rate = float64(cnt-p) / dt
		}
		next[handle] = cnt
		// The same model loaded twice (e.g. one per camera) shows up as two
		// identical names, so fold the copies into one row.
		merged := false
		for i := range a.Nets {
			if m := &a.Nets[i]; m.Name == n.Name {
				if tot := m.Count + n.Count; tot > 0 {
					m.AvgUs = int64((float64(m.AvgUs)*float64(m.Count) + float64(n.AvgUs)*float64(n.Count)) / float64(tot))
				}
				m.Instances++
				m.Count += n.Count
				m.Rate += n.Rate
				merged = true
				break
			}
		}
		if !merged {
			a.Nets = append(a.Nets, n)
		}
	}
	c.prevNets = next
	sort.SliceStable(a.Nets, func(i, j int) bool {
		if a.Nets[i].Rate != a.Nets[j].Rate {
			return a.Nets[i].Rate > a.Nets[j].Rate
		}
		return a.Nets[i].Count > a.Nets[j].Count
	})
}

var kindRules = []struct {
	kind string
	subs []string
}{
	{"NPU", []string{"npu", "vip", "nna"}},
	{"GPU", []string{"gpu", "mali", "bxm", "pvr"}},
	{"DDR", []string{"dmc", "ddr", "dram"}},
}

func (c *Collector) discoverDevfreq() {
	ents, _ := os.ReadDir("/sys/class/devfreq")
	for _, e := range ents {
		n := strings.ToLower(e.Name())
	rules:
		for _, r := range kindRules {
			for _, s := range r.subs {
				if strings.Contains(n, s) {
					c.devfreq = append(c.devfreq, devfreqDev{"/sys/class/devfreq/" + e.Name(), e.Name(), r.kind})
					break rules
				}
			}
		}
	}
}

var (
	reLoadAt   = regexp.MustCompile(`^(\d+)@`)
	rePercent  = regexp.MustCompile(`(\d+)\s*%`)
	rePVR      = regexp.MustCompile(`(?i)GPU\s+Utili[sz]ation[^0-9]*(\d+)\s*%`)
	reGPUCores = regexp.MustCompile(`(\d+)\s+cores?`)
	reGPUNode  = regexp.MustCompile(`^[0-9a-f]+\.gpu`)
	reMppLoad  = regexp.MustCompile(`^(\S+)\s+load:\s*([\d.]+)%\s+utilization:\s*([\d.]+)%`)
	reRgaSched = regexp.MustCompile(`scheduler\[\d+\]:\s*(\S+)\s+load\s*=\s*(\d+)%`)
)

// devLoad reads utilisation from the files the Rockchip/Mali/PVR drivers expose.
func devLoad(dir string) float64 {
	if s := readStr(dir + "/load"); s != "" { // Rockchip: "34@500000000Hz"
		if m := reLoadAt.FindStringSubmatch(s); m != nil {
			v, _ := strconv.ParseFloat(m[1], 64)
			return v
		}
	}
	for _, f := range []string{"/utilisation", "/gpuload", "/device/utilisation", "/device/gpuload"} {
		if v, ok := readInt(dir + f); ok {
			return float64(v)
		}
	}
	return -1
}

// freqRange is the lowest and highest step the devfreq device can run at.
func freqRange(dir string) (lo, hi int64) {
	for _, s := range strings.Fields(readStr(dir + "/available_frequencies")) {
		if v, err := strconv.ParseInt(s, 10, 64); err == nil {
			if lo == 0 || v < lo {
				lo = v
			}
			hi = max(hi, v)
		}
	}
	if hi == 0 {
		lo, _ = readInt(dir + "/min_freq")
		hi, _ = readInt(dir + "/max_freq")
	}
	return
}

// maliMem parses kbase's gpu_memory: a device total line ("mali0  <pages>"),
// then one line per context. Older kbase prints "kctx-0x.. <pages> <tgid>",
// the CSF (Valhall) builds print "kctx-0x.. <comm> <pid> <pages>".
func maliMem() (total uint64, perPID map[int]uint64) {
	perPID = map[int]uint64{}
	page := uint64(os.Getpagesize())
	for _, l := range strings.Split(readStr(maliDbg+"/gpu_memory"), "\n") {
		f := strings.Fields(l)
		if len(f) < 2 {
			continue
		}
		switch {
		case strings.HasPrefix(f[0], "mali"):
			if v, err := strconv.ParseUint(f[1], 10, 64); err == nil {
				total = v * page
			}
		case strings.HasPrefix(f[0], "kctx-"):
			pagesF, pidF := f[1], ""
			if len(f) >= 4 {
				pagesF, pidF = f[len(f)-1], f[len(f)-2]
			} else if len(f) == 3 {
				pidF = f[2]
			}
			pages, err1 := strconv.ParseUint(pagesF, 10, 64)
			pid, err2 := strconv.Atoi(pidF)
			if err1 == nil && err2 == nil {
				perPID[pid] += pages * page
			}
		}
	}
	return
}

func (c *Collector) accels(dt, drmGPU float64) ([]Accel, map[int]uint64) {
	var out []Accel
	for _, d := range c.devfreq {
		a := Accel{Kind: d.kind, Name: d.name, Load: -1, Governor: readStr(d.path + "/governor")}
		// The rknpu devfreq "load" reads 100 whenever the clock is up, even with
		// the NPU powered off; NPU load comes from debugfs instead.
		if d.kind != "NPU" {
			a.Load = devLoad(d.path)
		}
		a.FreqHz, _ = readInt(d.path + "/cur_freq")
		a.MinHz, a.MaxHz = freqRange(d.path)
		out = append(out, a)
	}
	// Rockchip NPU exposes per-core load in debugfs (root only):
	// "NPU load:  Core0:  0%, Core1:  0%, Core2:  0%,"
	if isDir(rknpuDir) {
		n := accelOf(&out, "NPU", "rknpu")
		var per []float64
		sum := 0.0
		for _, m := range rePercent.FindAllStringSubmatch(readStr(rknpuDir+"/load"), -1) {
			v, _ := strconv.ParseFloat(m[1], 64)
			per = append(per, v)
			sum += v
		}
		if len(per) > 0 {
			n.Load, n.PerCore = sum/float64(len(per)), per
		}
		if v := readStr(rknpuDir + "/version"); v != "" {
			_, after, ok := strings.Cut(v, ":")
			if !ok {
				after = v
			}
			n.Driver = strings.TrimSpace(after)
		}
		if n.FreqHz == 0 {
			n.FreqHz, _ = readInt(rknpuDir + "/freq")
		}
	}

	var gpuMem map[int]uint64
	if g := findAccel(out, "GPU"); g != nil || c.gpuInfo != "" || isDir(maliDbg) {
		g = accelOf(&out, "GPU", "mali")
		if m := reGPUCores.FindStringSubmatch(readStr(c.gpuInfo)); m != nil {
			g.Cores, _ = strconv.Atoi(m[1])
		}
		if g.Load < 0 && c.gpuInfo != "" { // Mali kbase also exposes utilisation next to gpuinfo
			if v, ok := readInt(filepath.Dir(c.gpuInfo) + "/utilisation"); ok {
				g.Load = float64(v)
			}
		}
		var tot uint64
		if tot, gpuMem = maliMem(); tot > 0 {
			g.MemUsed = tot
		}
	}

	// Allwinner A733: the PowerVR BXM-4-64 has no devfreq node on the Radxa BSP,
	// so the GPU comes from pvr debugfs and its clock from the clk tree.
	if pvr := isDir(pvrDir); pvr || c.gpuPM.dir != "" {
		name := "pvr"
		if c.gpuPM.dir != "" {
			name = filepath.Base(c.gpuPM.dir)
		}
		g := accelOf(&out, "GPU", name)
		// The Cubie A7A DDK build omits "GPU Utilisation" from status; other DDK
		// builds print it there.
		if g.Load < 0 && pvr {
			if m := rePVR.FindStringSubmatch(readStr(pvrDir + "/status")); m != nil {
				g.Load, _ = strconv.ParseFloat(m[1], 64)
			}
		}
		if g.FreqHz == 0 && c.gpuClk != "" {
			g.FreqHz, _ = readInt(c.gpuClk + "/clk_rate")
		}
		if pvr {
			ds := map[string]uint64{}
			for _, l := range strings.Split(readStr(pvrDir+"/driver_stats"), "\n") {
				if f := strings.Fields(l); len(f) == 2 {
					ds[f[0]], _ = strconv.ParseUint(f[1], 10, 64)
				}
			}
			g.MemUsed = ds["MemoryUsageAllocGPUMemUMA"] + ds["MemoryUsageAllocGPUMemLMA"] + ds["MemoryUsageDmaBufImport"]
			g.MemPeak = ds["MemoryUsageAllocGPUMemUMAMax"] + ds["MemoryUsageAllocGPUMemLMAMax"] + ds["MemoryUsageDmaBufImportMax"]
		}
	}
	if g := findAccel(out, "GPU"); g != nil {
		// Panfrost/Panthor (mainline) have no load file but report per-client
		// engine time in DRM fdinfo; runtime PM is the last resort.
		if g.Load < 0 && drmGPU >= 0 {
			g.Load = drmGPU
		}
		pm, ok := c.gpuPM.busy() // sample every tick to keep the deltas fresh
		if g.Load < 0 && ok {
			g.Load, g.Estimated = pm, true
		}
	}
	// A GPU node with no driver bound (e.g. panfrost failed to probe) still
	// gets a card, so a missing driver is visible rather than silent.
	if findAccel(out, "GPU") == nil {
		m, _ := filepath.Glob("/sys/devices/platform/*.gpu*")
		for _, d := range m {
			if !isDir(d+"/driver") && reGPUNode.MatchString(filepath.Base(d)) {
				out = append(out, Accel{Kind: "GPU", Name: filepath.Base(d), Load: -1,
					Note: "no GPU driver loaded for " + filepath.Base(d) + " (see dmesg)"})
				break
			}
		}
	}
	// Allwinner NPU (VeriSilicon VIP, "viplite" driver): devfreq gives the clock,
	// debugfs may give utilisation.
	if isDir(vipDir) {
		c.viplite(accelOf(&out, "NPU", "viplite"), dt)
	}

	root := os.Geteuid() == 0
	for i := range out {
		a := &out[i]
		switch {
		case strings.HasPrefix(a.Note, "no GPU driver"):
		case a.Load >= 0 || a.Kind == "DDR":
			a.Note = ""
		case !root:
			a.Note = "run as root for debugfs load"
		default:
			a.Note = "driver exposes no load counter"
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Kind < out[j].Kind })
	return out, gpuMem
}

func findAccel(out []Accel, kind string) *Accel {
	for i := range out {
		if out[i].Kind == kind {
			return &out[i]
		}
	}
	return nil
}

// ── video / 2D engines ──────────────────────────────────────────────────────

var mediaRules = []struct{ sub, label, role string }{
	{"rkvdec", "RKVDEC", "decode"},
	{"av1d", "AV1D", "AV1 dec"},
	{"vdpu", "VDPU", "decode"},
	{"rkvenc", "RKVENC", "encode"},
	{"vepu", "VEPU", "encode"},
	{"jpegd", "JPEGD", "JPEG dec"},
	{"jpege", "JPEGE", "JPEG enc"},
	{"avsd", "AVSD", "AVS+ dec"},
	{"vdpp", "VDPP", "post-proc"},
	{"rga", "RGA", "2D/scale"},
	{"iep", "IEP", "deint"},
	{"video-codec", "VPU", "codec"},
	{"cedar", "VE", "codec"},
}

var reVE = regexp.MustCompile(`^ve\d*$`)

// discoverMedia finds the codec and 2D blocks that have a driver bound. The
// MPP control units ("-ccu"), IOMMUs and the mpp-srv aggregator are skipped.
func (c *Collector) discoverMedia() {
	const base = "/sys/bus/platform/devices/"
	ents, _ := os.ReadDir(base)
	var found []*mediaDev
	for _, e := range ents {
		full := e.Name()
		_, short, ok := strings.Cut(full, ".")
		if !ok {
			short = full
		}
		s := strings.ToLower(short)
		// Skip IOMMUs, MPP control units, the mpp-srv aggregator, NoC bus
		// monitors ("nocp-gpu-vpu-rga-venc") and clock gates ("aclk_rkvdec0_pre").
		skip := false
		for _, x := range []string{"mmu", "ccu", "srv", "nocp", "clk", "opp", "pd_"} {
			skip = skip || strings.Contains(s, x)
		}
		if skip {
			continue
		}
		label, role := "", ""
		for _, r := range mediaRules {
			if strings.Contains(s, r.sub) {
				label, role = r.label, r.role
				break
			}
		}
		if label == "" && reVE.MatchString(s) {
			label, role = "VE", "codec"
		}
		if label == "" || !isDir(base+full+"/driver") || !codecDriver(base+full) {
			continue
		}
		m := &mediaDev{blk: MediaBlock{Name: label, Role: role, Dev: full, Busy: -1}, pm: pmTracker{dir: base + full}}
		if isDir("/sys/class/devfreq/" + full) {
			m.devfreq = "/sys/class/devfreq/" + full
		}
		found = append(found, m)
	}
	sort.SliceStable(found, func(i, j int) bool {
		if found[i].blk.Name != found[j].blk.Name {
			return mediaOrder(found[i].blk.Name) < mediaOrder(found[j].blk.Name)
		}
		return found[i].blk.Dev < found[j].blk.Dev
	})
	count, idx := map[string]int{}, map[string]int{}
	for _, m := range found {
		count[m.blk.Name]++
	}
	for _, m := range found {
		if n := m.blk.Name; count[n] > 1 {
			m.blk.Name = n + " " + strconv.Itoa(idx[n])
			idx[n]++
		}
	}
	c.media = found
}

func mediaOrder(label string) int {
	for i, r := range mediaRules {
		if r.label == label {
			return i
		}
	}
	return len(mediaRules)
}

// codecDriver is true when the bound driver is a codec/2D driver, so stray
// platform devices whose names merely contain "rga" or "vdec" are ignored.
func codecDriver(dev string) bool {
	l, _ := os.Readlink(dev + "/driver")
	d := strings.ToLower(filepath.Base(l))
	for _, k := range []string{"mpp", "rga", "iep", "cedar", "hantro", "rkvdec", "cedrus", "vpu", "vdpp", "vdec", "venc", "jpeg", "av1"} {
		if strings.Contains(d, k) {
			return true
		}
	}
	return false
}

// mppLoad reads the Rockchip MPP service's own accounting, refreshed every
// load_interval ms: "fdc38100.rkvdec-core  load: 12.50% utilization: 9.80%".
// utilization is hardware busy time, which is what the card shows.
func mppLoad() map[string]float64 {
	out := map[string]float64{}
	for _, l := range strings.Split(readStr("/proc/mpp_service/load"), "\n") {
		if m := reMppLoad.FindStringSubmatch(strings.TrimSpace(l)); m != nil {
			v, _ := strconv.ParseFloat(m[3], 64)
			out[m[1]] = v
		}
	}
	return out
}

// rgaLoad lists per-scheduler load from the RGA multicore driver in core
// order, which is also the RGA devices' address order.
func rgaLoad() []float64 {
	var out []float64
	for _, m := range reRgaSched.FindAllStringSubmatch(readStr("/sys/kernel/debug/rkrga/load"), -1) {
		v, _ := strconv.ParseFloat(m[2], 64)
		out = append(out, v)
	}
	return out
}

func (c *Collector) mediaSample(dt float64) []MediaBlock {
	var out []MediaBlock
	mpp, rga := mppLoad(), rgaLoad()
	rgaIdx := 0
	for _, m := range c.media {
		b := m.blk
		b.Status = readStr(m.pm.dir + "/power/runtime_status")
		pm, pmOK := m.pm.busy() // sample every tick to keep the deltas fresh
		if m.devfreq != "" {
			b.FreqHz, _ = readInt(m.devfreq + "/cur_freq")
		}
		isRGA := strings.HasPrefix(b.Name, "RGA")
		switch v, ok := mpp[b.Dev]; {
		case ok:
			b.Busy, b.Estimated = v, false
		case isRGA && rgaIdx < len(rga):
			b.Busy, b.Estimated = rga[rgaIdx], false
		case pmOK:
			// devfreq "load" on these blocks is not a busy counter (it reads
			// 100 while the block is off), so power-on time is the fallback.
			b.Busy, b.Estimated = pm, true
		}
		if isRGA {
			rgaIdx++
		}
		out = append(out, b)
	}
	return out
}
