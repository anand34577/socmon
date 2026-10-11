//go:build linux

package main

import (
	"fmt"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

var reCoreCount = regexp.MustCompile(`(?:MP|MC)(\d+)`)

// tickSec is the refresh interval, used to label history windows.
var tickSec = 1.0

// ── readings ────────────────────────────────────────────────────────────────

// tempFor is the hottest zone whose name contains any of subs.
func tempFor(s *Snapshot, subs ...string) float64 {
	t := -1.0
	for _, z := range s.Temps {
		n := strings.ToLower(z.Name)
		if strings.Contains(n, "idle") { // Allwinner cpu*_idle_zone duplicate the cpu sensors
			continue
		}
		for _, sub := range subs {
			if strings.Contains(n, sub) {
				t = math.Max(t, z.C)
				break
			}
		}
	}
	return t
}

func accel(s *Snapshot, kind string) (Accel, bool) {
	for _, a := range s.Accels {
		if a.Kind == kind {
			return a, true
		}
	}
	return Accel{}, false
}

var bigCores = map[string]bool{"A57": true, "A72": true, "A73": true, "A75": true, "A76": true,
	"A77": true, "A78": true, "A710": true, "A715": true, "A720": true}

func roleOf(typ string, clusters int) string {
	switch {
	case clusters <= 1:
		return "All cores"
	case bigCores[typ]:
		return "Performance"
	}
	return "Efficiency"
}

// coreSummary is "2×A76 + 6×A55", big cores first.
func coreSummary(s *Snapshot) string {
	var order []string
	n := map[string]int{}
	for _, k := range s.CPU.Cores {
		if _, ok := n[k.Type]; !ok {
			order = append(order, k.Type)
		}
		n[k.Type]++
	}
	sort.SliceStable(order, func(i, j int) bool { return bigCores[order[i]] && !bigCores[order[j]] })
	var parts []string
	for _, t := range order {
		name := t
		if name == "" {
			name = "core"
		}
		parts = append(parts, fmt.Sprintf("%d×%s", n[t], name))
	}
	return strings.Join(parts, " + ")
}

func throttled(s *Snapshot) []Cooling {
	var out []Cooling
	for _, c := range s.Cooling {
		t := strings.ToLower(c.Type)
		if c.Cur > 0 && (strings.Contains(t, "cpu") || strings.Contains(t, "devfreq") || strings.Contains(t, "gpu") || strings.Contains(t, "npu")) {
			out = append(out, c)
		}
	}
	return out
}

// health lists anything that needs attention, worst colour first.
func health(s *Snapshot) (string, rgb) {
	var issues []string
	col := cOK
	if t := tempFor(s, ""); t >= 85 {
		issues, col = append(issues, fmt.Sprintf("Overheating %.0f°C", t)), cBad
	} else if t >= 75 {
		issues, col = append(issues, fmt.Sprintf("Running hot %.0f°C", t)), cWarn
	}
	warn := func(msg string) {
		issues = append(issues, msg)
		if col == cOK {
			col = cWarn
		}
	}
	if len(throttled(s)) > 0 {
		warn("Thermal throttling")
	}
	if pctOf(s.Mem.Used, s.Mem.Total) >= 90 {
		warn("Memory almost full")
	}
	for _, m := range s.Mounts {
		if pctOf(m.Used, m.Used+m.Avail) >= 90 {
			warn(m.Path + " almost full")
			break
		}
	}
	if s.CPU.Total >= 90 {
		warn("Processor maxed out")
	}
	if s.CPU.Times.IOWait >= 25 {
		warn("Waiting on disk")
	}
	if len(issues) == 0 {
		return "All good", cOK
	}
	return strings.Join(issues, " · "), col
}

func tempText(t float64) string {
	return paint(tempCol(t), fmt.Sprintf("%.0f°C", t)) + " " + dim(tempWord(t))
}

// tempShort is the temperature alone, for tight key/value grids.
func tempShort(t float64) string { return paint(tempCol(t), fmt.Sprintf("%.1f°C", t)) }

func ghz(khz int64) string {
	if khz <= 0 {
		return "—"
	}
	if khz >= 1e6 {
		return fmt.Sprintf("%.2fG", float64(khz)/1e6)
	}
	return fmt.Sprintf("%dM", khz/1000)
}

func dur(sec float64) string {
	d := time.Duration(sec) * time.Second
	if d >= time.Minute {
		return fmt.Sprintf("%dm%02ds", int(d.Minutes()), int(d.Seconds())%60)
	}
	return fmt.Sprintf("%ds", int(d.Seconds()))
}

// historyFill draws a titled area chart into a card's spare rows.
func historyFill(title, key string, top float64, col rgb, f func(float64) string) func(in, rows int) []string {
	return func(in, rows int) []string {
		vals := hist[key]
		if rows < 3 || len(vals) < 2 {
			return nil
		}
		if len(vals) > in {
			vals = vals[len(vals)-in:]
		}
		sum, peak := 0.0, 0.0
		for _, v := range vals {
			sum += v
			peak = math.Max(peak, v)
		}
		head := split(dim(title+" · last "+dur(float64(len(vals))*tickSec)),
			dim("avg ")+text(f(sum/float64(len(vals))))+dim("  peak ")+text(f(peak)), in)
		return append([]string{"", head}, graph(vals, in, rows-2, top, col)...)
	}
}

// dualFill splits spare rows between two rate charts (e.g. in/out).
func dualFill(a, ka string, ca rgb, b, kb string, cb rgb) func(in, rows int) []string {
	return func(in, rows int) []string {
		if rows < 6 || len(hist[ka]) < 2 {
			return nil
		}
		half := func(title, key string, col rgb, h int) []string {
			vals := hist[key]
			if len(vals) > in {
				vals = vals[len(vals)-in:]
			}
			peak := 0.0
			for _, v := range vals {
				peak = math.Max(peak, v)
			}
			head := split(dim(title+" · last "+dur(float64(len(vals))*tickSec)), dim("peak ")+text(rate(peak)), in)
			return append([]string{head}, graph(vals, in, h-1, 0, col)...)
		}
		h1 := (rows - 1) / 2
		out := append([]string{""}, half(a, ka, ca, h1)...)
		return append(out, half(b, kb, cb, rows-1-h1)...)
	}
}

// usersOf lists processes holding a hardware block open, busiest first.
func usersOf(s *Snapshot, kinds ...string) []Proc {
	var out []Proc
	for _, p := range s.Procs {
		for _, h := range p.HW {
			for _, k := range kinds {
				if h == k {
					out = append(out, p)
					goto next
				}
			}
		}
	next:
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].GPU != out[j].GPU {
			return out[i].GPU > out[j].GPU
		}
		return out[i].CPU > out[j].CPU
	})
	return out
}

// userLine folds users into "ffmpeg ×6 · go2rtc · jellyfin".
func userLine(ps []Proc, withGPU bool) string {
	var order []string
	n, gpu := map[string]int{}, map[string]float64{}
	for _, p := range ps {
		name := p.Name
		if _, ok := n[name]; !ok {
			order = append(order, name)
		}
		n[name]++
		gpu[name] += p.GPU
	}
	var parts []string
	for _, name := range order {
		t := text(name)
		if n[name] > 1 {
			t += dim(fmt.Sprintf(" ×%d", n[name]))
		}
		if withGPU && gpu[name] >= 0.5 {
			t += " " + paint(heat(gpu[name]), pct(gpu[name]))
		}
		parts = append(parts, t)
	}
	return strings.Join(parts, dim(" · "))
}

var hwCol = map[string]rgb{"VPU": cMedia, "RGA": mix(cMedia, cDim, 0.4), "GPU": cGPU, "NPU": cNPU, "CAM": cDim}

func hwTags(hw []string) string {
	var parts []string
	for _, h := range hw {
		parts = append(parts, paint(hwCol[h], h))
	}
	return strings.Join(parts, " ")
}

// ── processor ───────────────────────────────────────────────────────────────

func cpuBody(s *Snapshot, in int) []string {
	p, t := s.CPU.Total, s.CPU.Times
	head := big(loadCol(p), pct(p)) + "  " + text(loadWord(p))
	parts := []string{dim("usr ") + text(pct(t.User)), dim("sys ") + text(pct(t.System)),
		dim("iowait ") + paint(map[bool]rgb{true: cWarn, false: cText}[t.IOWait >= 10], pct(t.IOWait)), dim("irq ") + text(pct(t.IRQ))}
	if t.Steal > 0.5 {
		parts = append(parts, dim("steal ")+text(pct(t.Steal)))
	}
	br := strings.Join(parts, "  ")
	for len(parts) > 1 && vlen(head)+vlen(br)+2 > in {
		parts = parts[:len(parts)-1]
		br = strings.Join(parts, "  ")
	}
	body := []string{
		split(head, br, in),
		spark(hist["cpu"], in, 100, rgb{}),
	}

	// Per-core grid, column-major so each cluster stays in its own column.
	hasFreq := false // the A733 vendor kernel has no cpufreq at all
	for _, k := range s.CPU.Cores {
		hasFreq = hasFreq || k.FreqKHz > 0
	}
	cores := append([]Core(nil), s.CPU.Cores...)
	sort.Slice(cores, func(i, j int) bool { return cores[i].ID < cores[j].ID })
	if n := len(cores); n > 0 {
		ncol := max(1, min(n, (in+2)/34))
		rows := (n + ncol - 1) / ncol
		ncol = (n + rows - 1) / rows
		cw := (in - 2*(ncol-1)) / ncol
		var cells []string
		for _, k := range cores {
			tc := cDim
			if bigCores[k.Type] {
				tc = cCPU
			}
			lab := dim(fmt.Sprintf("cpu%-2d", k.ID)) + " " + paint(tc, fmt.Sprintf("%-4s", k.Type))
			fw := 6
			if !hasFreq {
				fw = 0
			}
			cell := lab + " " + hbar(k.Usage, max(3, cw-10-7-fw)) + padL(paint(loadCol(k.Usage), pct(k.Usage)), 6)
			if hasFreq {
				cell += padL(text(ghz(k.FreqKHz)), 6)
			}
			cells = append(cells, cell)
		}
		body = append(body, grid(cells, ncol, in, 2)...)
	}

	// Clusters: share, clock against its ceiling. The governor is listed once
	// when every cluster shares it, which is the usual case.
	gov, same := "", true
	for i, cl := range s.CPU.Clusters {
		if i == 0 {
			gov = cl.Governor
		}
		same = same && cl.Governor == gov
	}
	for _, cl := range s.CPU.Clusters {
		typ, tc := cl.Type, cDim
		if typ == "" {
			typ = "core"
		}
		if bigCores[cl.Type] {
			tc = cCPU
		}
		lab := paint(tc, fmt.Sprintf("%-4s", typ)) + text(fmt.Sprintf(" ×%d ", len(cl.CPUs))) + dim(fmt.Sprintf("%-8s", cpuRange(cl.CPUs)))
		clk := text(fmt.Sprintf("%.2f", float64(cl.CurKHz)/1e6)) + dim(fmt.Sprintf("/%.2f GHz", float64(cl.MaxKHz)/1e6))
		if cl.MaxKHz == 0 {
			clk = dim("no cpufreq")
		}
		if cl.CapKHz > 0 && cl.CapKHz < cl.MaxKHz {
			clk += paint(cWarn, fmt.Sprintf(" cap %.2f", float64(cl.CapKHz)/1e6))
		}
		if !same {
			clk += dim(" " + cl.Governor)
		}
		bw := max(4, in-vlen(lab)-vlen(clk)-8)
		body = append(body, lab+hbar(cl.Usage, bw)+padL(paint(loadCol(cl.Usage), pct(cl.Usage)), 6)+"  "+clk)
	}

	la := dim("load ") + text(fmt.Sprintf("%.2f %.2f %.2f", s.Load[0], s.Load[1], s.Load[2])) +
		dim("  run ") + text(fmt.Sprint(s.CPU.Running)) + dim("  blocked ") +
		paint(map[bool]rgb{true: cWarn, false: cText}[s.CPU.Blocked > 0], fmt.Sprint(s.CPU.Blocked))
	ra := ""
	for _, extra := range []string{dim("ctx ") + text(count(s.CPU.CtxtRate)+"/s"), map[bool]string{true: dim(gov), false: ""}[same]} {
		if extra != "" && vlen(la)+vlen(ra)+vlen(extra)+4 <= in {
			ra = extra + map[bool]string{true: "  ", false: ""}[ra != ""] + ra
		}
	}
	return append(body, split(la, ra, in))
}

// cpuRange is "cpu4-7" for a contiguous set, else "cpu0,2".
func cpuRange(ids []int) string {
	if len(ids) == 0 {
		return ""
	}
	contig := true
	for i := 1; i < len(ids); i++ {
		contig = contig && ids[i] == ids[i-1]+1
	}
	if len(ids) == 1 {
		return fmt.Sprintf("cpu%d", ids[0])
	}
	if contig {
		return fmt.Sprintf("cpu%d-%d", ids[0], ids[len(ids)-1])
	}
	var p []string
	for _, id := range ids {
		p = append(p, fmt.Sprint(id))
	}
	return "cpu" + strings.Join(p, ",")
}

// ── memory ──────────────────────────────────────────────────────────────────

func memBody(s *Snapshot, in int) []string {
	m := s.Mem
	p := pctOf(m.Used, m.Total)
	cache := m.Available - min(m.Available, m.Free) // reclaimable part of what is available
	body := []string{
		split(big(loadCol(p), pct(p))+"  "+text("used"), text(size(m.Used))+dim(" of "+size(m.Total)), in),
		stack(in, struct {
			p float64
			c rgb
		}{p, cMem}, struct {
			p float64
			c rgb
		}{pctOf(cache, m.Total), cCache}),
		paint(cMem, "■") + dim(" apps ") + text(size(m.Used)) + "   " + paint(cCache, "■") + dim(" cache ") + text(size(cache)) +
			"   " + paint(cTrack, "■") + dim(" free ") + text(size(m.Free)),
	}
	pairs := [][2]string{
		{"Available", text(size(m.Available))},
		{"Page cache", text(size(m.Cached))},
		{"Buffers", text(size(m.Buffers))},
		{"Shared", text(size(m.Shared))},
	}
	if m.SwapTotal > 0 {
		sp := pctOf(m.SwapUsed, m.SwapTotal)
		pairs = append(pairs, [2]string{"Swap", paint(map[bool]rgb{true: cWarn, false: cText}[sp > 50], size(m.SwapUsed)) + dim(" / "+size(m.SwapTotal))})
	}
	if m.ZramOrig > 0 && m.ZramCompr > 0 {
		pairs = append(pairs, [2]string{"zram", text(size(m.ZramCompr)) + dim(fmt.Sprintf(" · %.1f×", float64(m.ZramOrig)/float64(m.ZramCompr)))})
	}
	pairs = append(pairs, [2]string{"Dirty", text(size(m.Dirty))})
	if m.CmaTotal > 0 {
		pairs = append(pairs, [2]string{"CMA used", text(size(m.CmaUsed)) + dim(" / "+size(m.CmaTotal))})
	}
	if d, ok := accel(s, "DDR"); ok {
		if d.FreqHz > 0 {
			pairs = append(pairs, [2]string{"DDR clock", text(hz(d.FreqHz))})
		}
		if d.Load >= 0 {
			pairs = append(pairs, [2]string{"DDR load", paint(loadCol(d.Load), pct(d.Load))})
		}
	}
	if t := tempFor(s, "ddr", "dram"); t >= 0 {
		pairs = append(pairs, [2]string{"DDR temp", tempShort(t)})
	}
	return append(body, kvGrid(pairs, in)...)
}

// ── graphics / AI ───────────────────────────────────────────────────────────

func loadHead(a Accel, in int, right string) []string {
	if a.Load < 0 {
		hint := "driver does not report it"
		if a.Note != "" {
			hint = a.Note
		}
		return []string{split(text("Usage not reported"), right, in), dim(hint)}
	}
	v := pct(a.Load)
	if a.Estimated {
		v = "~" + v
	}
	return []string{split(big(loadCol(a.Load), v)+"  "+text(loadWord(a.Load)), right, in)}
}

func clockText(a Accel) string {
	if a.FreqHz <= 0 {
		return ""
	}
	t := text(hz(a.FreqHz))
	if a.MaxHz > 0 {
		t += dim(" / " + hz(a.MaxHz))
	}
	return t
}

func gpuBody(s *Snapshot, a Accel, in int) []string {
	body := loadHead(a, in, clockText(a))
	if a.Load >= 0 {
		body = append(body, spark(hist["gpu"], in, 100, rgb{}))
	}
	var pairs [][2]string
	if t := tempFor(s, "gpu"); t >= 0 {
		pairs = append(pairs, [2]string{"Temperature", tempShort(t)})
	}
	if a.Governor != "" {
		pairs = append(pairs, [2]string{"Governor", text(a.Governor)})
	}
	if a.MemUsed > 0 {
		v := text(size(a.MemUsed))
		if a.MemPeak > a.MemUsed {
			v += dim(" peak " + size(a.MemPeak))
		}
		pairs = append(pairs, [2]string{"Memory", v})
	}
	if a.Estimated {
		pairs = append(pairs, [2]string{"Load source", dim("power-on time")})
	}
	body = append(body, kvGrid(pairs, in)...)
	if us := usersOf(s, "GPU"); len(us) > 0 {
		body = append(body, split(dim("Used by"), userLine(us, true), in))
	} else if s.Root {
		body = append(body, split(dim("Used by"), dim("no apps"), in))
	}
	return body
}

func npuBody(s *Snapshot, a Accel, in int) []string {
	body := loadHead(a, in, clockText(a))
	if a.Load >= 0 {
		body = append(body, spark(hist["npu"], in, 100, rgb{}))
	}
	if n := len(a.PerCore); n > 1 {
		// All cores on one row whenever they fit, shortening the label from
		// "Core0" to "C0" on narrow cards; the row then survives height trimming.
		label, ncol := "Core%d ", n
		switch {
		case n*(6+3+5)+2*(n-1) <= in:
		case n*(3+3+5)+2*(n-1) <= in:
			label = "C%d "
		default:
			label, ncol = "C%d ", max(1, (in+2)/13)
		}
		cw := (in - 2*(ncol-1)) / ncol
		lw := len(fmt.Sprintf(label, 0))
		var cells []string
		for i, v := range a.PerCore {
			cells = append(cells, dim(fmt.Sprintf(label, i))+hbar(v, max(3, cw-lw-5))+padL(paint(loadCol(v), pct(v)), 5))
		}
		// row-major: cores read left to right
		for i := 0; i < len(cells); i += ncol {
			body = append(body, strings.Join(cells[i:min(i+ncol, len(cells))], "  "))
		}
	}
	var pairs [][2]string
	if t := tempFor(s, "npu"); t >= 0 {
		pairs = append(pairs, [2]string{"Temperature", tempShort(t)})
	}
	if a.Governor != "" {
		pairs = append(pairs, [2]string{"Governor", text(a.Governor)})
	}
	if a.MemUsed > 0 {
		v := text(size(a.MemUsed))
		if a.MemPeak > a.MemUsed {
			v += dim(" peak " + size(a.MemPeak))
		}
		pairs = append(pairs, [2]string{"Memory", v})
	}
	if a.Driver != "" {
		pairs = append(pairs, [2]string{"Driver", text(a.Driver)})
	}
	if a.State != "" {
		pairs = append(pairs, [2]string{"Queue", text(a.State)})
	}
	body = append(body, kvGrid(pairs, in)...)

	var nets []NPUNet
	active := 0
	for _, n := range a.Nets {
		if n.Count > 0 { // loaded but never run is noise
			nets = append(nets, n)
		}
		if n.Rate > 0 {
			active++
		}
	}
	if len(nets) > 0 {
		body = append(body, split(dim("Models"), text(fmt.Sprintf("%d loaded · %d active", len(nets), active)), in))
		for i, n := range nets {
			if i == 4 {
				body = append(body, dim(fmt.Sprintf("  +%d more", len(nets)-4)))
				break
			}
			r := dim("waiting") + dim(fmt.Sprintf(" · %s runs", count(float64(n.Count))))
			if n.Rate > 0 {
				r = paint(cOK, fmt.Sprintf("%.1f/s", n.Rate)) + dim(fmt.Sprintf(" · %.1f ms · %s runs", float64(n.AvgUs)/1000, count(float64(n.Count))))
			}
			body = append(body, split("  "+text(modelName(n)), r, in))
		}
	}
	if us := usersOf(s, "NPU"); len(us) > 0 {
		body = append(body, split(dim("Used by"), userLine(us, false), in))
	} else if len(nets) == 0 && s.Root {
		body = append(body, split(dim("Used by"), dim("no apps"), in))
	}
	return body
}

// modelName turns the driver's truncated "vehicle_moving_" into "vehicle moving".
func modelName(n NPUNet) string {
	s := strings.TrimSpace(strings.ReplaceAll(strings.Trim(n.Name, "_-"), "_", " "))
	if n.Instances > 1 {
		s += fmt.Sprintf(" ×%d", n.Instances)
	}
	return s
}

// ── video engines ───────────────────────────────────────────────────────────

func mediaBody(s *Snapshot, in int) []string {
	users := usersOf(s, "VPU", "RGA")
	busy, on := 0.0, 0
	for _, m := range s.Media {
		busy = math.Max(busy, m.Busy)
		if m.Status == "active" {
			on++
		}
	}
	var head string
	switch {
	case len(users) > 0 || busy >= 1:
		head = big(cMedia, "● In use")
		if len(users) > 0 {
			head += dim(fmt.Sprintf("  by %d app", len(users))) + dim(map[bool]string{true: "s", false: ""}[len(users) > 1])
		}
	case len(s.Media) == 0:
		head = text("No engine driver found")
	default:
		head = paint(cDim, "○ ") + text("Idle")
	}
	right := ""
	if len(s.Media) > 0 {
		right = text(fmt.Sprint(on)) + dim(fmt.Sprintf(" of %d engines powered", len(s.Media)))
	}
	body := []string{split(head, right, in)}
	if len(s.Media) > 0 {
		body = append(body, spark(hist["media"], in, 100, rgb{}))
		ncol := max(1, min(len(s.Media), (in+2)/25))
		cw := (in - 2*(ncol-1)) / ncol
		var cells []string
		for _, m := range s.Media {
			dot := paint(cTrack, "○")
			if m.Status == "active" {
				dot = paint(cMedia, "●")
			}
			v := dim("off")
			switch {
			case m.Busy >= 0:
				v = pct(m.Busy)
				if m.Estimated {
					v = "~" + v
				}
				v = paint(loadCol(m.Busy), v)
			case m.Status == "active":
				v = paint(cMedia, "on")
			case m.Status == "":
				v = dim("n/a")
			}
			lab := dot + " " + text(fmt.Sprintf("%-9s", m.Name))
			if cw >= 36 {
				lab += dim(fmt.Sprintf("%-9s", m.Role))
			}
			cells = append(cells, lab+hbar(math.Max(0, m.Busy), max(3, cw-vlen(lab)-6))+padL(v, 6))
		}
		body = append(body, grid(cells, ncol, in, 2)...)
	}
	switch {
	case len(users) > 0:
		for i, p := range users {
			if i == 3 {
				body = append(body, dim(fmt.Sprintf("  +%d more · ", len(users)-3))+userLine(users[3:], false))
				break
			}
			right := hwTags(p.HW) + "  " + text(padL(pct(p.CPU/float64(max(1, len(s.CPU.Cores)))), 5)) + dim(" cpu")
			body = append(body, split(faint(fmt.Sprintf("%-7d", p.PID))+text(ellip(p.Cmd, in-vlen(right)-9)), right, in))
		}
	case !s.Root:
		body = append(body, dim("Run as root to see which apps use the engines"))
	default:
		body = append(body, split(dim("Apps using the engines"), dim("none"), in))
	}
	return body
}

// ── network ─────────────────────────────────────────────────────────────────

func ifaceLink(it Iface, all []Iface) string {
	switch {
	case it.Physical && !it.Carrier:
		return paint(cBad, "no link")
	case it.Kind == "Wi-Fi" && it.Signal > -100 && it.Signal != 0:
		c := cOK
		if it.Signal < -75 {
			c = cBad
		} else if it.Signal < -65 {
			c = cWarn
		}
		return paint(c, fmt.Sprintf("%d dBm", it.Signal))
	case it.SpeedMbps > 0:
		d := ""
		if it.Duplex != "" && it.Duplex != "full" {
			d = paint(cWarn, " "+it.Duplex)
		} else if it.Duplex == "full" {
			d = dim(" full")
		}
		return text(linkSpeed(it.SpeedMbps)) + d
	case len(it.Ports) > 0:
		for _, p := range all {
			for _, port := range it.Ports {
				if p.Name == port && p.Physical {
					return dim("via ") + text(p.Name) + " " + ifaceLink(p, all)
				}
			}
		}
		return dim("ports " + strings.Join(it.Ports, ","))
	case it.Physical && !it.Carrier:
		return paint(cBad, "no link")
	}
	return ""
}

func netBody(s *Snapshot, in int) []string {
	hw := (in - 2) / 2
	body := []string{
		split(paint(cNet, "▼ ")+big(cNet, rate(s.NetRx))+dim(" in"), dim("out ")+big(cUp, rate(s.NetTx))+paint(cUp, " ▲"), in),
		spark(hist["rx"], hw, 0, cNet) + "  " + spark(hist["tx"], in-hw-2, 0, cUp),
	}
	shown := map[string]bool{}
	for _, it := range s.Ifaces {
		shown[it.Name] = true
	}
	for _, it := range s.Ifaces {
		if it.Master != "" && shown[it.Master] {
			continue // shown through its bridge/bond
		}
		dot := paint(cOK, "●")
		if !it.Carrier {
			dot = paint(cBad, "○")
		}
		ip := ""
		if len(it.IPs) > 0 {
			ip = text(it.IPs[0])
			if len(it.IPs) > 1 {
				ip += dim(fmt.Sprintf(" +%d", len(it.IPs)-1))
			}
		}
		link := ifaceLink(it, s.Ifaces)
		l := dot + " " + bold + text(it.Name) + " " + dim(it.Kind) + "  " + link
		if vlen(l)+vlen(ip)+1 > in {
			l = dot + " " + bold + text(it.Name) + "  " + link // drop the kind word first
		}
		body = append(body, split(l, ip, in))
		if !it.Carrier && len(it.IPs) == 0 {
			continue // an unplugged port needs no second line
		}
		l2 := "  " + paint(cNet, "▼") + text(padL(rate(it.RxBps), 10)) + "  " + paint(cUp, "▲") + text(padL(rate(it.TxBps), 10))
		r2 := dim("Σ ") + text(size(it.RxTotal)) + dim(" / ") + text(size(it.TxTotal))
		if it.Errors+it.Drops > 0 {
			r2 = paint(cWarn, fmt.Sprintf("err %s drop %s  ", count(float64(it.Errors)), count(float64(it.Drops)))) + r2
		}
		// Optional details, added only while they fit beside the totals.
		for _, extra := range []string{it.Driver, fmt.Sprintf("mtu %d", it.MTU), it.MAC} {
			if extra == "" || extra == "usb" || extra == "mtu 1500" || extra == "mtu 0" {
				continue
			}
			if vlen(l2)+3+len(extra)+vlen(r2)+1 > in {
				break
			}
			l2 += dim(" · " + extra)
		}
		body = append(body, split(l2, r2, in))
	}
	pairs := [][2]string{{"Hostname", text(s.Host)}}
	if s.Gateway != "" {
		pairs = append(pairs, [2]string{"Gateway", text(s.Gateway)})
	}
	if s.DNS != "" {
		pairs = append(pairs, [2]string{"DNS", text(s.DNS)})
	}
	pairs = append(pairs, [2]string{"TCP", text(fmt.Sprint(s.TCPEstab)) + dim(" open · ") + text(fmt.Sprint(s.TCPListen)) + dim(" listen")})
	return append(body, kvGrid(pairs, in)...)
}

// ── storage ─────────────────────────────────────────────────────────────────

func mountLine(m Mount, in int) string {
	p := pctOf(m.Used, m.Used+m.Avail) // what df shows: reserved blocks count as used
	right := padL(paint(loadCol(p), pct(p)), 5) + "  " + text(padL(size(m.Used), 8)) + dim(" / ") + text(padL(size(m.Total), 7)) + dim(fmt.Sprintf(" %-5s", m.FS))
	pw := min(16, max(6, in/5))
	bw := max(4, in-pw-2-vlen(right)-1)
	return "  " + text(fmt.Sprintf("%-*s", pw, ellip(m.Path, pw-1))) + hbar(p, bw) + " " + right
}

func diskBody(s *Snapshot, in int) []string {
	hw := (in - 2) / 2
	body := []string{
		split(dim("read ")+big(cDisk, rate(s.DiskRead)), dim("write ")+big(cUp, rate(s.DiskWrite)), in),
		spark(hist["dr"], hw, 0, cDisk) + "  " + spark(hist["dw"], in-hw-2, 0, cUp),
	}
	used := map[int]bool{}
	for _, d := range s.Disks {
		busy := dim("busy ") + paint(loadCol(d.Util), pct(d.Util))
		l := bold + text(d.Name) + " " + paint(cDisk, d.Kind) + " " + text(size(d.Size))
		if d.Temp > 0 {
			l += " " + paint(tempCol(d.Temp), fmt.Sprintf("%.0f°C", d.Temp))
		}
		if room := in - vlen(l) - vlen(busy) - 3; d.Model != "" && room >= 6 {
			l += " " + dim(ellip(d.Model, room))
		}
		body = append(body, split(l, busy, in))
		body = append(body, split("  "+dim("R ")+text(rate(d.ReadBps))+faint(" · "+count(d.ReadIOPS)+" iops"),
			dim("W ")+text(rate(d.WriteBps))+faint(" · "+count(d.WriteIOPS)+" iops"), in))
		for i, m := range s.Mounts {
			if m.Disk == d.Name {
				used[i] = true
				body = append(body, mountLine(m, in))
			}
		}
	}
	var group string
	for i, m := range s.Mounts {
		if used[i] {
			continue
		}
		g := m.Disk
		if g == "" {
			g = "other"
		}
		if g != group {
			group = g
			body = append(body, bold+text(strings.Replace(g, "zfs:", "ZFS pool ", 1)))
		}
		body = append(body, mountLine(m, in))
	}
	return body
}

// ── sensors ─────────────────────────────────────────────────────────────────

func sensorName(n string) string {
	n = strings.ToLower(n)
	for _, suf := range []string{"-thermal", "_thermal", "-zone", "_zone", "-temp", "_temp"} {
		n = strings.TrimSuffix(n, suf)
	}
	return n
}

func sensorsBody(s *Snapshot, in int) []string {
	var ts []Temp
	seen := map[string]bool{}
	for _, t := range s.Temps {
		n := sensorName(t.Name)
		if strings.Contains(n, "idle") || seen[n] {
			continue
		}
		seen[n] = true
		ts = append(ts, Temp{n, t.C})
	}
	sort.SliceStable(ts, func(i, j int) bool { return ts[i].Name < ts[j].Name })
	var body []string
	if len(ts) > 0 {
		ncol := max(1, min(len(ts), (in+3)/22))
		cw := (in - 3*(ncol-1)) / ncol
		var cells []string
		for _, t := range ts {
			cells = append(cells, split(dim(ellip(t.Name, cw-8)), paint(tempCol(t.C), fmt.Sprintf("%.1f°C", t.C)), cw))
		}
		body = append(body, grid(cells, ncol, in, 3)...)
	} else {
		body = append(body, dim("No temperature sensors"))
	}
	for _, f := range s.Fans {
		v := ""
		if f.RPM >= 0 {
			v = text(fmt.Sprintf("%d rpm", f.RPM))
		}
		if f.PWM >= 0 {
			v += dim(" duty ") + text(fmt.Sprintf("%d%%", f.PWM))
		}
		body = append(body, kv("Fan "+f.Name, v, in))
	}
	for _, p := range s.Power {
		v := text(fmt.Sprintf("%.2f V", p.Volts))
		if p.Amps >= 0 {
			v += dim(" · ") + text(fmt.Sprintf("%.2f A", p.Amps)) + dim(" · ") + text(fmt.Sprintf("%.1f W", p.Volts*p.Amps))
		}
		body = append(body, kv("Power in", v, in))
	}
	if th := throttled(s); len(th) > 0 {
		var parts []string
		for _, c := range th {
			parts = append(parts, fmt.Sprintf("%s %d/%d", c.Type, c.Cur, c.Max))
		}
		body = append(body, split(paint(cWarn, "▲ Throttling"), paint(cWarn, strings.Join(parts, " · ")), in))
	} else if len(s.Cooling) > 0 {
		body = append(body, split(dim("Thermal throttling"), paint(cOK, "none"), in))
	}
	return body
}

// ── processes ───────────────────────────────────────────────────────────────

const (
	sortCPU = iota
	sortMem
	sortIO
	sortModes
)

var sortNames = []string{"CPU", "memory", "disk I/O"}

type pcol struct {
	head  string
	w     int
	right bool
	drop  int // lower drops first when the command column gets too narrow
	cell  func(p Proc, cores float64) string
}

func procCols(sortBy int) []pcol {
	arrow := func(h string, mode int) string {
		if mode == sortBy {
			return h + "▾"
		}
		return h
	}
	return []pcol{
		{"PID", 7, true, 9, func(p Proc, _ float64) string { return faint(fmt.Sprint(p.PID)) }},
		{"USER", 9, false, 4, func(p Proc, _ float64) string { return dim(ellip(p.User, 8)) }},
		{"S", 2, false, 1, func(p Proc, _ float64) string {
			c := cDim
			switch p.State {
			case "R":
				c = cOK
			case "D":
				c = cBad
			case "Z":
				c = cWarn
			}
			return paint(c, p.State)
		}},
		{"THR", 4, true, 2, func(p Proc, _ float64) string { return dim(fmt.Sprint(p.Threads)) }},
		{"", 9, false, 3, func(p Proc, cores float64) string { return " " + hbar(p.CPU/cores, 7) }},
		{arrow("CPU%", sortCPU), 6, true, 8, func(p Proc, cores float64) string {
			c := p.CPU / cores
			return paint(map[bool]rgb{true: loadCol(c), false: cDim}[c >= 0.05], fmt.Sprintf("%.1f", c))
		}},
		{"MEM%", 6, true, 5, func(p Proc, _ float64) string { return dim(fmt.Sprintf("%.1f", p.MemPct)) }},
		{arrow("RSS", sortMem), 9, true, 7, func(p Proc, _ float64) string { return text(size(p.RSS)) }},
		{arrow("DISK", sortIO), 10, true, 6, func(p Proc, _ float64) string {
			if io := p.ReadBps + p.WriteBps; io > 0 {
				return text(rate(io))
			}
			return faint("—")
		}},
		{"HW", 13, false, 7, func(p Proc, _ float64) string {
			t := hwTags(p.HW)
			if p.GPU >= 0.5 {
				t += " " + paint(cGPU, pct(p.GPU))
			}
			return " " + t
		}},
		{"COMMAND", 0, false, 10, func(p Proc, _ float64) string { return text(p.Cmd) }},
	}
}

func procsBody(s *Snapshot, in, rows, sortBy int) []string {
	var ps []Proc
	for _, p := range s.Procs {
		if p.Cmd != "" { // kernel threads mean nothing to most people
			ps = append(ps, p)
		}
	}
	key := func(p Proc) float64 {
		switch sortBy {
		case sortMem:
			return float64(p.RSS)
		case sortIO:
			return p.ReadBps + p.WriteBps
		}
		return p.CPU
	}
	sort.SliceStable(ps, func(i, j int) bool {
		if a, b := key(ps[i]), key(ps[j]); a != b {
			return a > b
		}
		return ps[i].RSS > ps[j].RSS
	})
	// Per-process CPU is share of the whole chip, so it adds up to the headline.
	cores := float64(max(1, len(s.CPU.Cores)))

	table := func(ps []Proc, w int) []string {
		cols := procCols(sortBy)
		for {
			fixed := 0
			for _, c := range cols {
				fixed += c.w + 1
			}
			if w-fixed >= 30 || len(cols) <= 4 {
				break
			}
			lo := -1
			for i, c := range cols {
				if c.head != "COMMAND" && (lo < 0 || c.drop < cols[lo].drop) {
					lo = i
				}
			}
			cols = append(cols[:lo], cols[lo+1:]...)
		}
		fixed := 0
		for _, c := range cols {
			fixed += c.w + 1
		}
		cmdW := max(8, w-fixed+1)
		line := func(cell func(c pcol) string) string {
			var sb strings.Builder
			for i, c := range cols {
				cw := c.w
				if c.head == "COMMAND" {
					cw = cmdW
				}
				v := cell(c)
				if c.right {
					sb.WriteString(padL(clip(v, cw), cw))
				} else {
					sb.WriteString(fit(v, cw))
				}
				if i < len(cols)-1 {
					sb.WriteString(" ")
				}
			}
			return sb.String()
		}
		out := []string{line(func(c pcol) string {
			if strings.HasSuffix(c.head, "▾") {
				return bold + text(c.head)
			}
			return dim(c.head)
		})}
		for _, p := range ps {
			out = append(out, line(func(c pcol) string { return c.cell(p, cores) }))
		}
		return out
	}

	n := rows - 1
	if (in-3)/2 < 96 || len(ps) <= n { // one table unless a second shows more apps at full detail
		return table(ps[:min(n, len(ps))], in)
	}
	lw := (in - 3) / 2
	left, right := table(ps[:n], lw), table(ps[n:min(2*n, len(ps))], in-3-lw)
	sep := fg(cBorder) + " │ " + reset
	for i := range left {
		r := ""
		if i < len(right) {
			r = right[i]
		}
		left[i] = fit(left[i], lw) + sep + r
	}
	return left
}

// ── layout ──────────────────────────────────────────────────────────────────

func headerLine(s *Snapshot, w int) string {
	name := s.Board.Board
	if name == "" {
		name = s.Board.Model
	}
	sep := faint("  │  ")
	segs := []string{bold + paint(cText, name)}
	if s.Board.SoC != "unknown" {
		segs = append(segs, paint(cCPU, s.Board.SoC)+dim("  "+coreSummary(s)))
	}
	segs = append(segs, dim("up ")+text(uptime(s.Uptime)))
	if s.OS != "" {
		segs = append(segs, dim(s.OS))
	}
	if s.Board.Kernel != "" {
		segs = append(segs, dim("Linux "+s.Board.Kernel))
	}
	msg, col := health(s)
	right := paint(col, "● ") + bold + paint(col, msg) + "   " + bold + text(s.Time.Format("15:04:05"))
	for len(segs) > 1 && vlen(" "+strings.Join(segs, sep))+vlen(right)+3 > w {
		segs = segs[:len(segs)-1]
	}
	line := split(" "+strings.Join(segs, sep), right+" ", w)
	b := bg(cHeadBg)
	return b + strings.ReplaceAll(fit(line, w), reset, reset+b) + reset
}

func footerLine(s *Snapshot, w, sortBy int) string {
	key := func(k, what string) string {
		return bg(cKeyBg) + bold + paint(cText, " "+k+" ") + " " + dim(what) + "  "
	}
	l := " " + key("q", "quit") + key("s", "sort: "+sortNames[sortBy]) + key("c", "CPU") + key("m", "memory") + key("i", "disk I/O")
	r := faint(fmt.Sprintf("socmon · every %gs ", tickSec))
	if !s.Root {
		r = paint(cWarn, "not root: GPU/NPU load and app hardware use are hidden  ") + r
	}
	return split(l, r, w)
}

type panel struct {
	title, meta string
	col         rgb
	body        func(in int) []string
	fill        func(in, rows int) []string // optional content for spare rows
}

func panels(s *Snapshot) map[string]*panel {
	pctF := func(v float64) string { return pct(v) }
	cpuMeta := dim(fmt.Sprintf("%d cores · %s", len(s.CPU.Cores), coreSummary(s)))
	if t := tempFor(s, "cpu", "core", "soc"); t >= 0 {
		cpuMeta = paint(tempCol(t), fmt.Sprintf("%.0f°C", t)) + dim(" · ") + cpuMeta
	}
	tempF := func(v float64) string { return fmt.Sprintf("%.0f°C", v) }
	p := map[string]*panel{
		"cpu": {"Processor", cpuMeta, cCPU,
			func(in int) []string { return cpuBody(s, in) }, historyFill("CPU", "cpu", 100, rgb{}, pctF)},
		"mem": {"Memory", dim(size(s.Mem.Total)), cMem,
			func(in int) []string { return memBody(s, in) }, historyFill("Memory used", "mem", 100, cMem, pctF)},
		"net": {"Network", dim(fmt.Sprintf("%d interfaces", len(s.Ifaces))), cNet,
			func(in int) []string { return netBody(s, in) }, dualFill("Download", "rx", cNet, "Upload", "tx", cUp)},
		"disk": {"Storage", dim(fmt.Sprintf("%d disks · %d filesystems", len(s.Disks), len(s.Mounts))), cDisk,
			func(in int) []string { return diskBody(s, in) }, dualFill("Disk read", "dr", cDisk, "Disk write", "dw", cUp)},
		"sens": {"Sensors", "", cSens,
			func(in int) []string { return sensorsBody(s, in) }, historyFill("Hottest sensor", "temp", 0, cSens, tempF)},
	}
	if t := tempFor(s, ""); t > 0 {
		p["sens"].meta = dim("hottest ") + paint(tempCol(t), fmt.Sprintf("%.0f°C", t))
	}
	if a, ok := accel(s, "GPU"); ok {
		t := "Graphics"
		if s.Board.GPU != "" {
			t += " · " + s.Board.GPU
		}
		meta := ""
		if a.Cores == 0 { // panfrost has no gpuinfo; fall back to the SoC profile ("MC3", "MP4")
			if m := reCoreCount.FindStringSubmatch(s.Board.GPU); m != nil {
				a.Cores, _ = strconv.Atoi(m[1])
			}
		}
		if a.Cores > 0 {
			meta = dim(fmt.Sprintf("%d shader cores", a.Cores))
		}
		p["gpu"] = &panel{t, meta, cGPU, func(in int) []string { return gpuBody(s, a, in) }, historyFill("GPU", "gpu", 100, rgb{}, pctF)}
	}
	if a, ok := accel(s, "NPU"); ok {
		t := "AI Accelerator"
		if s.Board.NPUTops > 0 {
			t += fmt.Sprintf(" · %g TOPS", s.Board.NPUTops)
		}
		cores := len(a.PerCore)
		if cores == 0 {
			cores = s.Board.NPUCores
		}
		meta := dim(s.Board.NPU)
		if cores > 1 {
			meta = dim(fmt.Sprintf("%s · %d cores", s.Board.NPU, cores))
		}
		p["npu"] = &panel{t, meta, cNPU, func(in int) []string { return npuBody(s, a, in) }, historyFill("NPU", "npu", 100, rgb{}, pctF)}
	}
	if len(s.Media) > 0 || len(usersOf(s, "VPU", "RGA")) > 0 {
		meta := "V4L2"
		switch s.Board.Vendor {
		case "rockchip":
			meta = "Rockchip MPP"
		case "allwinner":
			meta = "Allwinner VE"
		}
		p["media"] = &panel{"Video Engines", dim(meta), cMedia, func(in int) []string { return mediaBody(s, in) },
			historyFill("Busiest engine", "media", 100, rgb{}, pctF)}
	}
	return p
}

// shaveOrder ranks cards by what to cut first when the screen is short.
var shaveOrder = map[string]int{"sens": 0, "disk": 1, "net": 2, "mem": 3, "npu": 4, "gpu": 5, "media": 6, "cpu": 7}

func render(s *Snapshot, w, ht, sortBy int) string {
	ps := panels(s)
	var layout [][]string
	switch {
	case w >= 150:
		mid := []string{}
		for _, k := range []string{"gpu", "npu", "media"} {
			if ps[k] != nil {
				mid = append(mid, k)
			}
		}
		right := []string{"net", "disk", "sens"}
		if len(mid) < 2 {
			mid, right = append(mid, "disk"), []string{"net", "sens"}
		}
		layout = [][]string{{"cpu", "mem"}, mid, right}
	case w >= 96:
		left, right := []string{"cpu", "mem", "disk"}, []string{}
		for _, k := range []string{"gpu", "npu", "media"} {
			if ps[k] != nil {
				right = append(right, k)
			}
		}
		layout = [][]string{left, append(right, "net", "sens")}
	default:
		col := []string{"cpu", "mem"}
		for _, k := range []string{"gpu", "npu", "media"} {
			if ps[k] != nil {
				col = append(col, k)
			}
		}
		layout = [][]string{append(col, "net", "disk", "sens")}
	}

	n := len(layout)
	cw := (w - (n - 1)) / n
	widths := make([]int, n)
	bodies := make([][][]string, n)
	colH := make([]int, n)
	topH := 0
	for i, col := range layout {
		widths[i] = cw
		if i == n-1 {
			widths[i] = w - (n-1)*(cw+1) // last column absorbs the rounding
		}
		for _, k := range col {
			b := ps[k].body(widths[i] - 4)
			bodies[i] = append(bodies[i], b)
			colH[i] += len(b) + 2
		}
		topH = max(topH, colH[i])
	}

	avail := ht - 2 // header + footer
	minProcs := 12
	if len(s.Procs) == 0 {
		minProcs = 0
	}
	// Keep room for the process table unless the terminal is too short to
	// show the cards at all, in which case the cards win.
	if limit := avail - minProcs; topH > limit {
		if limit >= 14 {
			topH = limit
		} else {
			topH = min(topH, avail)
		}
	}

	var cols [][]string
	for i, col := range layout {
		// Too tall: shave lines off the least important card first, keeping
		// a few lines of each while possible. Too short: the last card gets
		// the spare rows, filled with its history chart.
		for _, keep := range []int{3, 1} {
			for colH[i] > topH {
				j := -1
				for k, key := range col {
					if len(bodies[i][k]) > keep && (j < 0 || shaveOrder[key] < shaveOrder[col[j]]) {
						j = k
					}
				}
				if j < 0 {
					break
				}
				bodies[i][j] = bodies[i][j][:len(bodies[i][j])-1]
				colH[i]--
			}
		}
		last := len(col) - 1
		if spare := topH - colH[i]; spare > 0 {
			if f := ps[col[last]].fill; f != nil {
				bodies[i][last] = append(bodies[i][last], f(widths[i]-4, spare)...)
			}
		}
		var lines []string
		for k, key := range col {
			h := len(bodies[i][k]) + 2
			if k == last {
				h = topH - len(lines)
			}
			p := ps[key]
			lines = append(lines, card(p.title, p.meta, p.col, bodies[i][k], widths[i], h)...)
		}
		cols = append(cols, lines)
	}

	L := []string{headerLine(s, w)}
	for r := 0; r < topH; r++ {
		var line string
		for i := range cols {
			if i > 0 {
				line += " "
			}
			if r < len(cols[i]) {
				line += cols[i][r]
			} else {
				line += strings.Repeat(" ", widths[i])
			}
		}
		L = append(L, line)
	}
	if rest := avail - topH; rest >= 4 && len(s.Procs) > 0 {
		meta := dim(fmt.Sprintf("%d tasks · %d threads · sorted by %s", s.Tasks, s.Threads, sortNames[sortBy]))
		L = append(L, card("Processes", meta, cText, procsBody(s, w-4, rest-2, sortBy), w, rest)...)
	}
	if len(L) > ht-1 {
		L = L[:max(0, ht-1)]
	}
	for len(L) < ht-1 {
		L = append(L, "")
	}
	L = append(L, footerLine(s, w, sortBy))
	for i := range L {
		L[i] = clip(L[i], w) + "\x1b[K"
	}
	return "\x1b[H" + strings.Join(L, "\r\n") + "\x1b[J"
}
