//go:build linux

package main

import (
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
)

type Proc struct {
	PID      int      `json:"pid"`
	Name     string   `json:"name"`
	Cmd      string   `json:"cmd,omitempty"` // argv joined; empty for kernel threads
	User     string   `json:"user,omitempty"`
	State    string   `json:"state"`
	Threads  int      `json:"threads"`
	CPU      float64  `json:"cpu"` // percent of one core
	RSS      uint64   `json:"rss"`
	MemPct   float64  `json:"mem_pct"`
	ReadBps  float64  `json:"read_bps"`
	WriteBps float64  `json:"write_bps"`
	HW       []string `json:"hw,omitempty"`  // hardware blocks it holds open: VPU, RGA, GPU, NPU, CAM
	GPU      float64  `json:"gpu,omitempty"` // from DRM fdinfo engine time, where the driver reports it
	GPUMem   uint64   `json:"gpu_mem,omitempty"`
}

type procPrev struct{ ticks, rd, wr uint64 }

func passwd() map[uint32]string {
	out := map[uint32]string{}
	for _, l := range strings.Split(readStr("/etc/passwd"), "\n") {
		f := strings.Split(l, ":")
		if len(f) > 2 {
			if id, err := strconv.ParseUint(f[2], 10, 32); err == nil {
				out[uint32(id)] = f[0]
			}
		}
	}
	return out
}

// cmdline returns argv with argv[0] reduced to its basename, so the kernel's
// 15-char comm limit ("pvedaemon worke") does not hide what a process is.
func cmdline(pid string) string {
	args := strings.Fields(readStr("/proc/" + pid + "/cmdline"))
	if len(args) == 0 {
		return ""
	}
	args[0] = filepath.Base(args[0])
	return strings.Join(args, " ")
}

// classifyDev names the hardware block behind a device node, or "".
func classifyDev(p string) string {
	base := filepath.Base(p)
	switch {
	case p == "/dev/mpp_service", p == "/dev/cedar_dev", p == "/dev/vpu_service", p == "/dev/hevc_service",
		strings.HasPrefix(p, "/dev/rkvdec"), strings.HasPrefix(p, "/dev/rkvenc"):
		return "VPU"
	case p == "/dev/rga", strings.HasPrefix(p, "/dev/iep"):
		return "RGA"
	case p == "/dev/mali0", strings.HasPrefix(p, "/dev/pvr"):
		return "GPU"
	case p == "/dev/rknpu", p == "/dev/vipcore", strings.HasPrefix(p, "/dev/accel/"):
		return "NPU"
	case strings.HasPrefix(p, "/dev/video"):
		n := strings.ToLower(readStr("/sys/class/video4linux/" + base + "/name"))
		for _, s := range []string{"dec", "enc", "codec", "hantro", "cedrus", "vpu", "jpeg"} {
			if strings.Contains(n, s) {
				return "VPU"
			}
		}
		if strings.Contains(n, "rga") {
			return "RGA"
		}
		return "CAM"
	case strings.HasPrefix(p, "/dev/dri/"):
		drv, _ := os.Readlink("/sys/class/drm/" + base + "/device/driver")
		switch strings.ToLower(filepath.Base(drv)) {
		case "rknpu":
			return "NPU"
		case "panfrost", "panthor", "mali", "pvrsrvkm", "powervr", "lima":
			return "GPU"
		}
	}
	return ""
}

var hwOrder = map[string]int{"VPU": 0, "RGA": 1, "GPU": 2, "NPU": 3, "CAM": 4}

// scanFDs finds the hardware blocks pid holds open and, for DRM GPU clients,
// the cumulative per-engine busy time (ns) from fdinfo.
func (c *Collector) scanFDs(pid string) (hw []string, eng map[string]uint64) {
	dir := "/proc/" + pid + "/fd/"
	ents, err := os.ReadDir(dir)
	if err != nil {
		return nil, nil
	}
	seen, clients := map[string]bool{}, map[string]bool{}
	for _, e := range ents {
		t, err := os.Readlink(dir + e.Name())
		if err != nil || !strings.HasPrefix(t, "/dev/") {
			continue
		}
		cls, ok := c.devCls[t]
		if !ok {
			cls = classifyDev(t)
			c.devCls[t] = cls
		}
		if cls == "" {
			continue
		}
		if !seen[cls] {
			seen[cls] = true
			hw = append(hw, cls)
		}
		if cls != "GPU" || !strings.HasPrefix(t, "/dev/dri/") {
			continue
		}
		id, tmp := "", map[string]uint64{}
		for _, l := range strings.Split(readStr("/proc/"+pid+"/fdinfo/"+e.Name()), "\n") {
			k, v, ok := strings.Cut(l, ":")
			if !ok {
				continue
			}
			k, v = strings.TrimSpace(k), strings.TrimSpace(v)
			if k == "drm-client-id" {
				id = v
			} else if en, ok := strings.CutPrefix(k, "drm-engine-"); ok && !strings.HasPrefix(en, "capacity") {
				if f := strings.Fields(v); len(f) > 0 {
					tmp[en], _ = strconv.ParseUint(f[0], 10, 64)
				}
			}
		}
		// dup'd fds share a client; count each client once.
		if id != "" && !clients[id] && len(tmp) > 0 {
			clients[id] = true
			if eng == nil {
				eng = map[string]uint64{}
			}
			for k, v := range tmp {
				eng[k] += v
			}
		}
	}
	sort.Slice(hw, func(i, j int) bool { return hwOrder[hw[i]] < hwOrder[hw[j]] })
	return
}

// procs samples every process. drmGPU is whole-GPU load from DRM fdinfo
// (busiest engine), or -1 when no client reports engine time.
func (c *Collector) procs(dt float64, memTotal uint64) ([]Proc, float64) {
	ents, _ := os.ReadDir("/proc")
	next := make(map[int]procPrev, len(ents))
	nextDRM := map[int]map[string]uint64{}
	engBusy := map[string]float64{}
	var out []Proc
	page := uint64(os.Getpagesize())
	for _, e := range ents {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		b, err := os.ReadFile("/proc/" + e.Name() + "/stat")
		if err != nil {
			continue
		}
		s := string(b)
		lp, rp := strings.IndexByte(s, '('), strings.LastIndexByte(s, ')')
		if lp < 0 || rp < lp || rp+2 > len(s) {
			continue
		}
		fs := strings.Fields(s[rp+2:]) // fs[0] is field 3 (state)
		if len(fs) < 22 {
			continue
		}
		ut, _ := strconv.ParseUint(fs[11], 10, 64)  // field 14
		st, _ := strconv.ParseUint(fs[12], 10, 64)  // field 15
		thr, _ := strconv.Atoi(fs[17])              // field 20
		rss, _ := strconv.ParseUint(fs[21], 10, 64) // field 24
		p := Proc{PID: pid, Name: s[lp+1 : rp], Cmd: cmdline(e.Name()), State: fs[0], Threads: thr, RSS: rss * page}
		if memTotal > 0 {
			p.MemPct = float64(p.RSS) / float64(memTotal) * 100
		}
		var fi syscall.Stat_t
		if syscall.Stat("/proc/"+e.Name(), &fi) == nil {
			if u, ok := c.users[fi.Uid]; ok {
				p.User = u
			} else if u, ok := c.users[fi.Uid-100000]; ok && fi.Uid >= 100000 && fi.Uid < 165536 {
				p.User = "ct:" + u // unprivileged LXC container (Proxmox maps 0 → 100000)
			} else {
				p.User = strconv.Itoa(int(fi.Uid))
			}
		}
		var rd, wr uint64
		for _, l := range strings.Split(readStr("/proc/"+e.Name()+"/io"), "\n") {
			if v, ok := strings.CutPrefix(l, "read_bytes: "); ok {
				rd, _ = strconv.ParseUint(v, 10, 64)
			} else if v, ok := strings.CutPrefix(l, "write_bytes: "); ok {
				wr, _ = strconv.ParseUint(v, 10, 64)
			}
		}
		ticks := ut + st
		next[pid] = procPrev{ticks, rd, wr}
		if pp, ok := c.prevProc[pid]; ok && dt > 0 {
			if ticks >= pp.ticks {
				p.CPU = float64(ticks-pp.ticks) / clkTck / dt * 100
			}
			if rd >= pp.rd && wr >= pp.wr {
				p.ReadBps, p.WriteBps = float64(rd-pp.rd)/dt, float64(wr-pp.wr)/dt
			}
		}
		if p.Cmd != "" {
			var eng map[string]uint64
			p.HW, eng = c.scanFDs(e.Name())
			if eng != nil {
				nextDRM[pid] = eng
				if prev, ok := c.prevDRM[pid]; ok && dt > 0 {
					for k, v := range eng {
						if v >= prev[k] {
							d := float64(v-prev[k]) / (dt * 1e9) * 100
							engBusy[k] += d
							p.GPU = max(p.GPU, clampPct(d))
						}
					}
				}
			}
		}
		out = append(out, p)
	}
	c.prevProc, c.prevDRM = next, nextDRM
	drm := -1.0
	if len(nextDRM) > 0 {
		drm = 0
		for _, v := range engBusy {
			drm = max(drm, clampPct(v))
		}
	}
	return out, drm
}
