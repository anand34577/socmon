//go:build linux

package main

import (
	"fmt"
	"math"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"
)

// fixture is a busy RK3588 box: Jellyfin transcoding on the VPU, an RKNN
// model on the NPU, a Proxmox bridge and an NVMe root.
func fixture() *Snapshot {
	s := &Snapshot{
		Time:  time.Date(2026, 10, 9, 11, 2, 7, 0, time.Local),
		Board: BoardInfo{Model: "FriendlyElec CM3588 Plus", SoC: "RK3588", Vendor: "rockchip", GPU: "Mali-G610 MP4", NPU: "RKNPU", NPUTops: 6, NPUCores: 3, Kernel: "6.1.99-rk3588"},
		OS:    "Debian GNU/Linux 12 (bookworm)", Uptime: 28*86400 + 21*3600 + 300, Load: [3]float64{1.92, 1.40, 1.12},
		Host: "app-server-0", Root: true, Gateway: "192.0.2.1", DNS: "192.0.2.1", TCPEstab: 48, TCPListen: 23,
		Tasks: 412, Threads: 1290,
	}
	use := []float64{12, 4, 9, 30, 64, 71, 18, 22}
	for i := 0; i < 8; i++ {
		t, f := "A55", int64(1800000)
		if i >= 4 {
			t, f = "A76", 2256000
		}
		s.CPU.Cores = append(s.CPU.Cores, Core{i, use[i], f, f, t})
	}
	s.CPU.Total, s.CPU.Times = 28.7, CPUTimes{User: 21.3, System: 5.1, IOWait: 0.8, IRQ: 1.5}
	s.CPU.Running, s.CPU.CtxtRate = 3, 18400
	s.CPU.Clusters = []Cluster{
		{[]int{0, 1, 2, 3}, "A55", "schedutil", 1800000, 408000, 1800000, 1800000, 13.7},
		{[]int{4, 5}, "A76", "schedutil", 2256000, 408000, 2352000, 2352000, 67.5},
		{[]int{6, 7}, "A76", "schedutil", 2016000, 408000, 2352000, 2016000, 20},
	}
	gb := uint64(1 << 30)
	s.Mem = MemStat{Total: 15.6 * 1e9, Used: 10.8 * 1e9, Free: 1.1 * 1e9, Available: 4.8 * 1e9, Cached: 3.4 * 1e9, Buffers: 120 << 20,
		Shared: 210 << 20, Dirty: 3 << 20, SwapTotal: 4 * gb, SwapUsed: 310 << 20, ZramOrig: 1200 << 20, ZramCompr: 290 << 20, CmaTotal: 256 << 20, CmaFree: 98 << 20}
	s.Accels = []Accel{
		{Kind: "DDR", Name: "dmc", Load: 23, FreqHz: 2112e6, MaxHz: 2112e6},
		{Kind: "GPU", Name: "fb000000.gpu", Load: 37, FreqHz: 800e6, MinHz: 300e6, MaxHz: 1000e6, Governor: "simple_ondemand", Cores: 4, MemUsed: 412 << 20},
		{Kind: "NPU", Name: "fdab0000.npu", Load: 22, PerCore: []float64{61, 5, 0}, FreqHz: 1000e6, MinHz: 300e6, MaxHz: 1000e6, Governor: "rknpu_ondemand", Driver: "v0.9.8"},
	}
	s.Media = []MediaBlock{
		{"RKVDEC 0", "decode", "fdc38100.rkvdec-core", "active", 48, true, 0},
		{"RKVDEC 1", "decode", "fdc48100.rkvdec-core", "active", 31, true, 0},
		{"AV1D", "AV1 dec", "fdc70000.av1d", "suspended", 0, true, 0},
		{"VDPU", "decode", "fdb50400.vdpu", "suspended", 0, true, 0},
		{"RKVENC 0", "encode", "fdbd0000.rkvenc-core", "active", 27, true, 0},
		{"RKVENC 1", "encode", "fdbe0000.rkvenc-core", "suspended", 0, true, 0},
		{"JPEGD", "JPEG dec", "fdb90000.jpegd", "suspended", -1, false, 0},
		{"RGA 0", "2D/scale", "fdb60000.rga", "active", 12, true, 0},
		{"RGA 1", "2D/scale", "fdb70000.rga", "suspended", 0, true, 0},
		{"IEP", "deint", "fdbb0000.iep", "suspended", 0, true, 0},
	}
	for _, t := range []struct {
		n string
		c float64
	}{{"soc-thermal", 52.3}, {"bigcore0-thermal", 58.1}, {"bigcore1-thermal", 55.4}, {"littlecore-thermal", 51.0},
		{"center-thermal", 50.2}, {"gpu-thermal", 53.6}, {"npu-thermal", 54.9}, {"nvme", 44.9}} {
		s.Temps = append(s.Temps, Temp{t.n, t.c})
	}
	s.Fans = []Fan{{"pwmfan", -1, 35}}
	s.Cooling = []Cooling{{"pwm-fan", 1, 4}, {"cpufreq-cpu6", 1, 9}}
	s.Ifaces = []Iface{
		{Name: "enP4p65s0", Kind: "Ethernet", Physical: true, Driver: "r8169", MAC: "8a:1c:2e:44:91:0f", Master: "vmbr0", Up: true, Carrier: true, SpeedMbps: 2500, Duplex: "full", MTU: 1500, RxBps: 4.1e6, TxBps: 21.4e6, RxTotal: 812 * gb, TxTotal: 2355 * gb},
		{Name: "vmbr0", Kind: "bridge", Ports: []string{"enP4p65s0", "tap101i0"}, MAC: "8a:1c:2e:44:91:0f", Up: true, Carrier: true, MTU: 1500, IPs: []string{"192.0.2.11/24"}, RxBps: 3.9e6, TxBps: 21.2e6, RxTotal: 790 * gb, TxTotal: 2252 * gb},
		{Name: "vmbr0.110", Kind: "VLAN", Up: true, Carrier: true, MTU: 1500, IPs: []string{"198.51.100.11/24"}, MAC: "8a:1c:2e:44:91:0f", RxBps: 1200, TxBps: 800, RxTotal: 3 * gb, TxTotal: gb},
		{Name: "enP3p49s0", Kind: "Ethernet", Physical: true, Driver: "r8169", MAC: "8a:1c:2e:44:91:10", MTU: 1500},
	}
	s.NetRx, s.NetTx = 4.1e6, 21.4e6
	s.Disks = []Disk{
		{"nvme0n1", "NVMe", "Samsung SSD 980 PRO 500GB", 500e9, 18.2e6, 2.1e6, 410, 55, 21, 44.9},
		{"mmcblk0", "eMMC", "A3A444", 62.5e9, 0, 41e3, 0, 6, 0.4, 0},
	}
	s.DiskRead, s.DiskWrite = 18.2e6, 2.14e6
	s.Mounts = []Mount{
		{"/", "/dev/nvme0n1p2", "ext4", "nvme0n1", 454e9, 43.3e9, 387e9},
		{"/boot/efi", "/dev/nvme0n1p1", "vfat", "nvme0n1", 511e6, 61e6, 450e6},
		{"/var/lib/vz", "/dev/mmcblk0p1", "ext4", "mmcblk0", 57.4e9, 36.2e9, 18.2e9},
		{"/rpool", "rpool", "zfs", "zfs:rpool", 1.8e12, 1.1e12, 0.7e12},
	}
	procs := []Proc{
		{PID: 2211, Name: "ffmpeg", Cmd: "ffmpeg -hwaccel rkmpp -i /media/movies/Dune.Part.Two.2024.2160p.mkv -c:v hevc_rkmpp -b:v 8M", User: "jellyfin", State: "R", Threads: 14, CPU: 92, RSS: 412 << 20, HW: []string{"VPU", "RGA"}, ReadBps: 18e6},
		{PID: 2240, Name: "ffmpeg", Cmd: "ffmpeg -hwaccel rkmpp -rtsp_transport tcp -i rtsp://cam2/stream -c:v h264_rkmpp", User: "root", State: "S", Threads: 9, CPU: 21, RSS: 96 << 20, HW: []string{"VPU"}},
		{PID: 1802, Name: "jellyfin", Cmd: "jellyfin --webdir=/usr/share/jellyfin/web --ffmpeg=/usr/lib/jellyfin-ffmpeg/ffmpeg", User: "jellyfin", State: "S", Threads: 41, CPU: 18, RSS: 509 << 20, HW: []string{"GPU"}, GPU: 12, GPUMem: 96 << 20},
		{PID: 3301, Name: "python", Cmd: "python -m rknn_detect --model yolov8n.rknn --cores 0", User: "frigate", State: "S", Threads: 6, CPU: 34, RSS: 380 << 20, HW: []string{"NPU", "RGA"}},
		{PID: 101, Name: "kvm", Cmd: "kvm -id 101 -name truenas,debug-threads=on -m 7168", User: "root", State: "S", Threads: 22, CPU: 24, RSS: 7 * gb},
		{PID: 902, Name: "pvestatd", Cmd: "pvestatd", User: "root", State: "S", Threads: 1, CPU: 2, RSS: 113 << 20, WriteBps: 12e3},
		{PID: 1203, Name: "immich", Cmd: "immich", User: "immich", State: "S", Threads: 18, CPU: 1.2, RSS: 467 << 20},
		{PID: 1250, Name: "postgres", Cmd: "postgres: 15/main: checkpointer", User: "postgres", State: "D", Threads: 1, CPU: 0.6, RSS: 53 << 20, WriteBps: 2.1e6},
	}
	for i := 0; i < 40; i++ {
		procs = append(procs, Proc{PID: 4000 + i, Name: "worker", Cmd: fmt.Sprintf("pvedaemon worker %d", i), User: "root", State: "S", Threads: 1, CPU: float64(i%3) * 0.2, RSS: uint64(150-i) << 20})
	}
	for i := range procs {
		procs[i].MemPct = float64(procs[i].RSS) / float64(s.Mem.Total) * 100
	}
	s.Procs = procs

	hist = history{}
	for i := 0; i < 300; i++ {
		x := float64(i)
		s2 := *s
		s2.CPU.Total = 25 + 20*math.Sin(x/9) + 8*math.Sin(x/2.3)
		s2.NetRx, s2.NetTx = 4e6+3e6*math.Sin(x/7), 20e6+9e6*math.Sin(x/11)
		s2.DiskRead, s2.DiskWrite = math.Max(0, 18e6*math.Sin(x/13)), 2e6+1e6*math.Sin(x/5)
		record(&s2)
	}
	return s
}

var ansi = regexp.MustCompile(`\x1b\[[0-9;]*[A-Za-z]`)

// TestRender checks every frame line is exactly the terminal width at a few
// sizes, and writes the frames to $SOCMON_PREVIEW for eyeballing.
func TestRender(t *testing.T) {
	s := fixture()
	var dump strings.Builder
	for _, sz := range [][2]int{{170, 44}, {160, 45}, {210, 56}, {120, 40}, {80, 50}} {
		out := render(s, sz[0], sz[1], sortCPU)
		lines := strings.Split(strings.TrimPrefix(out, "\x1b[H"), "\r\n")
		if len(lines) != sz[1] {
			t.Errorf("%dx%d: %d lines", sz[0], sz[1], len(lines))
		}
		for i, l := range lines {
			if n := vlen(strings.TrimSuffix(strings.TrimSuffix(l, "\x1b[J"), "\x1b[K")); n > sz[0] {
				t.Errorf("%dx%d line %d is %d wide", sz[0], sz[1], i, n)
			}
		}
		if plain := ansi.ReplaceAllString(out, ""); !strings.Contains(plain, "Core2") && !strings.Contains(plain, "C2 ") {
			t.Errorf("%dx%d: NPU core 2 not visible", sz[0], sz[1])
		}
		fmt.Fprintf(&dump, "==== %dx%d ====\n%s\n", sz[0], sz[1], ansi.ReplaceAllString(out, ""))
		if p := os.Getenv("SOCMON_PREVIEW"); p != "" {
			os.WriteFile(fmt.Sprintf("%s.%dx%d.ans", p, sz[0], sz[1]), []byte(out), 0o644)
		}
	}
	if p := os.Getenv("SOCMON_PREVIEW"); p != "" {
		os.WriteFile(p, []byte(dump.String()), 0o644)
	}
}
