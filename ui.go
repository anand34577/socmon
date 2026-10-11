//go:build linux

package main

import (
	"fmt"
	"math"
	"os"
	"strings"
	"unicode/utf8"
)

// ── colour ──────────────────────────────────────────────────────────────────

type rgb struct{ r, g, b uint8 }

var trueColor = func() bool {
	ct := os.Getenv("COLORTERM")
	return strings.Contains(ct, "truecolor") || strings.Contains(ct, "24bit")
}()

func q6(v uint8) int { return (int(v)*5 + 127) / 255 }

// fg emits a truecolor escape, or the nearest xterm-256 cube colour.
func fg(c rgb) string {
	if trueColor {
		return fmt.Sprintf("\x1b[38;2;%d;%d;%dm", c.r, c.g, c.b)
	}
	return fmt.Sprintf("\x1b[38;5;%dm", 16+36*q6(c.r)+6*q6(c.g)+q6(c.b))
}

func bg(c rgb) string {
	if trueColor {
		return fmt.Sprintf("\x1b[48;2;%d;%d;%dm", c.r, c.g, c.b)
	}
	return fmt.Sprintf("\x1b[48;5;%dm", 16+36*q6(c.r)+6*q6(c.g)+q6(c.b))
}

const (
	reset = "\x1b[0m"
	bold  = "\x1b[1m"
)

var (
	cBorder = rgb{58, 64, 80}
	cText   = rgb{225, 229, 238}
	cDim    = rgb{125, 132, 150}
	cFaint  = rgb{80, 86, 102}
	cTrack  = rgb{44, 48, 58}
	cHeadBg = rgb{26, 30, 42}
	cKeyBg  = rgb{50, 56, 74}
	cOK     = rgb{90, 210, 130}
	cWarn   = rgb{240, 190, 80}
	cBad    = rgb{240, 90, 90}

	cCPU   = rgb{100, 170, 255}
	cGPU   = rgb{255, 165, 90}
	cNPU   = rgb{190, 140, 255}
	cMedia = rgb{255, 120, 170}
	cMem   = rgb{90, 210, 200}
	cCache = rgb{70, 120, 190}
	cDisk  = rgb{240, 200, 90}
	cNet   = rgb{110, 220, 160}
	cUp    = rgb{255, 165, 90}
	cSens  = rgb{240, 130, 100}
)

func paint(c rgb, s string) string { return fg(c) + s + reset }
func dim(s string) string          { return paint(cDim, s) }
func faint(s string) string        { return paint(cFaint, s) }
func text(s string) string         { return paint(cText, s) }
func big(c rgb, s string) string   { return bold + paint(c, s) }

func clamp(v, lo, hi float64) float64 { return math.Max(lo, math.Min(hi, v)) }

func mix(a, b rgb, t float64) rgb {
	l := func(x, y uint8) uint8 { return uint8(float64(x) + (float64(y)-float64(x))*t + 0.5) }
	return rgb{l(a.r, b.r), l(a.g, b.g), l(a.b, b.b)}
}

// heat maps 0..100 onto green → amber → red.
func heat(p float64) rgb {
	p = clamp(p, 0, 100)
	switch {
	case p < 50:
		return cOK
	case p < 75:
		return mix(cOK, cWarn, (p-50)/25)
	}
	return mix(cWarn, cBad, (p-75)/25)
}

// ── plain-language status ───────────────────────────────────────────────────

func loadCol(p float64) rgb {
	switch {
	case p >= 85:
		return cBad
	case p >= 60:
		return cWarn
	}
	return cOK
}

func loadWord(p float64) string {
	switch {
	case p < 5:
		return "Idle"
	case p < 30:
		return "Light"
	case p < 60:
		return "Moderate"
	case p < 85:
		return "Busy"
	}
	return "Maxed out"
}

func tempCol(c float64) rgb {
	switch {
	case c >= 85:
		return cBad
	case c >= 75:
		return cWarn
	case c >= 60:
		return mix(cOK, cWarn, (c-60)/15)
	}
	return cOK
}

func tempWord(c float64) string {
	switch {
	case c < 50:
		return "Cool"
	case c < 75:
		return "Normal"
	case c < 85:
		return "Hot"
	}
	return "Too hot"
}

// ── formatting ──────────────────────────────────────────────────────────────

func size(b uint64) string {
	f, units, i := float64(b), []string{"B", "KB", "MB", "GB", "TB"}, 0
	for f >= 1024 && i < len(units)-1 {
		f /= 1024
		i++
	}
	if i == 0 || f >= 100 {
		return fmt.Sprintf("%.0f %s", f, units[i])
	}
	return fmt.Sprintf("%.1f %s", f, units[i])
}

func rate(bps float64) string { return size(uint64(bps)) + "/s" }

func hz(v int64) string {
	switch {
	case v >= 1e9:
		return fmt.Sprintf("%.2f GHz", float64(v)/1e9)
	case v > 0:
		return fmt.Sprintf("%d MHz", v/1e6)
	}
	return "—"
}

func linkSpeed(mbps int64) string {
	if mbps >= 1000 {
		return fmt.Sprintf("%g Gb/s", float64(mbps)/1000)
	}
	return fmt.Sprintf("%d Mb/s", mbps)
}

func count(v float64) string {
	switch {
	case v >= 1e6:
		return fmt.Sprintf("%.1fM", v/1e6)
	case v >= 1e4:
		return fmt.Sprintf("%.0fk", v/1e3)
	case v >= 1e3:
		return fmt.Sprintf("%.1fk", v/1e3)
	}
	return fmt.Sprintf("%.0f", v)
}

func pct(p float64) string {
	if p > 0 && p < 9.95 {
		return fmt.Sprintf("%.1f%%", p)
	}
	return fmt.Sprintf("%.0f%%", p)
}

func pctOf(used, total uint64) float64 {
	if total == 0 {
		return 0
	}
	return float64(used) / float64(total) * 100
}

func uptime(sec float64) string {
	d, h, m := int(sec)/86400, int(sec)/3600%24, int(sec)/60%60
	switch {
	case d > 0:
		return fmt.Sprintf("%dd %dh %dm", d, h, m)
	case h > 0:
		return fmt.Sprintf("%dh %dm", h, m)
	}
	return fmt.Sprintf("%dm", m)
}

// ── text layout ─────────────────────────────────────────────────────────────

// clip truncates to w visible columns, passing ANSI escapes through.
func clip(s string, w int) string {
	var sb strings.Builder
	vis := 0
	for i := 0; i < len(s); {
		if s[i] == 0x1b {
			j := strings.IndexByte(s[i:], 'm')
			if j < 0 {
				break
			}
			sb.WriteString(s[i : i+j+1])
			i += j + 1
			continue
		}
		if vis >= w {
			break
		}
		_, n := utf8.DecodeRuneInString(s[i:])
		sb.WriteString(s[i : i+n])
		i += n
		vis++
	}
	return sb.String() + reset
}

// ellip truncates plain text to w columns with a trailing ellipsis.
func ellip(s string, w int) string {
	if w <= 0 {
		return ""
	}
	if utf8.RuneCountInString(s) <= w {
		return s
	}
	r := []rune(s)
	return string(r[:w-1]) + "…"
}

func vlen(s string) int {
	n := 0
	for i := 0; i < len(s); {
		if s[i] == 0x1b {
			j := strings.IndexByte(s[i:], 'm')
			if j < 0 {
				break
			}
			i += j + 1
			continue
		}
		_, sz := utf8.DecodeRuneInString(s[i:])
		i += sz
		n++
	}
	return n
}

func pad(s string, w int) string {
	if d := w - vlen(s); d > 0 {
		return s + strings.Repeat(" ", d)
	}
	return s
}

func padL(s string, w int) string {
	if d := w - vlen(s); d > 0 {
		return strings.Repeat(" ", d) + s
	}
	return s
}

// fit pads or clips s to exactly w columns.
func fit(s string, w int) string { return pad(clip(s, w), w) }

// split puts l at the left edge and r at the right edge of a w-wide line.
func split(l, r string, w int) string {
	if vlen(l)+vlen(r)+1 > w {
		l = clip(l, max(0, w-vlen(r)-1))
	}
	return l + strings.Repeat(" ", max(1, w-vlen(l)-vlen(r))) + r
}

// kv is a dim label with its value pushed to the right edge.
func kv(k, v string, w int) string { return split(dim(k), v, w) }

// grid lays cells out in n columns separated by gap, filling column-major so
// related items stay together.
func grid(cells []string, n, w, gap int) []string {
	if n < 1 || len(cells) == 0 {
		return nil
	}
	n = min(n, len(cells))
	rows := (len(cells) + n - 1) / n
	cw := (w - gap*(n-1)) / n
	var out []string
	for r := 0; r < rows; r++ {
		var line string
		for c := 0; c < n; c++ {
			i := c*rows + r
			cell := ""
			if i < len(cells) {
				cell = cells[i]
			}
			if c > 0 {
				line += strings.Repeat(" ", gap)
			}
			if c == n-1 {
				line += fit(cell, w-(cw+gap)*(n-1))
			} else {
				line += fit(cell, cw)
			}
		}
		out = append(out, line)
	}
	return out
}

// kvGrid renders label/value pairs two per row when there is room; a pair
// too long for half a row gets a row of its own.
func kvGrid(pairs [][2]string, w int) []string {
	n := 1
	if w >= 52 {
		n = 2
	}
	cw := (w - 2*(n-1)) / n
	var out, row []string
	flush := func() {
		if len(row) > 0 {
			out = append(out, strings.Join(row, "  "))
			row = nil
		}
	}
	for _, p := range pairs {
		if n > 1 && vlen(p[0])+vlen(p[1])+2 > cw {
			flush()
			out = append(out, kv(p[0], p[1], w))
			continue
		}
		row = append(row, kv(p[0], p[1], cw))
		if len(row) == n {
			flush()
		}
	}
	flush()
	return out
}

// ── meters ──────────────────────────────────────────────────────────────────

// bar is a thin meter with half-cell precision. With col == nil each filled
// cell takes the heat colour of its position, so a full bar reads green → red.
func bar(p float64, w int, col *rgb) string {
	if w < 1 {
		return ""
	}
	halves := int(clamp(p, 0, 100)/100*float64(w*2) + 0.5)
	if p > 0.5 && halves == 0 {
		halves = 1 // keep any real activity visible
	}
	var sb strings.Builder
	for i := 0; i < w; i++ {
		c := cTrack
		ch := "━"
		if 2*i+1 < halves || 2*i+1 == halves {
			if col != nil {
				c = *col
			} else {
				c = heat(float64(i+1) / float64(w) * 100)
			}
			if 2*i+1 == halves {
				ch = "╸"
			}
		}
		sb.WriteString(fg(c) + ch)
	}
	return sb.String() + reset
}

func hbar(p float64, w int) string        { return bar(p, w, nil) }
func cbar(p float64, w int, c rgb) string { return bar(p, w, &c) }

// stack draws segments (percent, colour) end to end on one track.
func stack(w int, segs ...struct {
	p float64
	c rgb
}) string {
	var sb strings.Builder
	used := 0
	acc := 0.0
	for _, s := range segs {
		acc += s.p
		end := min(w, int(clamp(acc, 0, 100)/100*float64(w)+0.5))
		if n := end - used; n > 0 {
			sb.WriteString(fg(s.c) + strings.Repeat("━", n))
			used = end
		}
	}
	sb.WriteString(fg(cTrack) + strings.Repeat("━", w-used) + reset)
	return sb.String()
}

var sparkRunes = []rune(" ▁▂▃▄▅▆▇█")

// heatColor passed as a series colour means "colour by value".
var heatColor = rgb{}

// spark is a one-row history; the newest sample is on the right. top <= 0
// scales to the window's peak.
func spark(vals []float64, w int, top float64, col rgb) string {
	if w < 1 {
		return ""
	}
	if top <= 0 {
		top = 1
		for _, v := range vals {
			top = math.Max(top, v)
		}
	}
	var sb strings.Builder
	if missing := w - len(vals); missing > 0 {
		sb.WriteString(fg(cTrack) + strings.Repeat("▁", missing))
	}
	if len(vals) > w {
		vals = vals[len(vals)-w:]
	}
	for _, v := range vals {
		lvl := int(clamp(v/top, 0, 1)*8 + 0.5)
		c := col
		if col == heatColor {
			c = heat(v / top * 100)
		}
		if lvl == 0 {
			sb.WriteString(fg(cTrack) + "▁")
			continue
		}
		sb.WriteString(fg(c) + string(sparkRunes[lvl]))
	}
	return sb.String() + reset
}

// graph is a multi-row area chart, newest sample on the right.
func graph(vals []float64, w, h int, top float64, col rgb) []string {
	if w < 1 || h < 1 {
		return nil
	}
	if top <= 0 {
		top = 1
		for _, v := range vals {
			top = math.Max(top, v)
		}
	}
	if len(vals) > w {
		vals = vals[len(vals)-w:]
	}
	off := w - len(vals)
	out := make([]string, h)
	for r := 0; r < h; r++ {
		var sb strings.Builder
		rowFromBottom := h - 1 - r
		for x := 0; x < w; x++ {
			if x < off {
				sb.WriteString(" ")
				continue
			}
			v := vals[x-off]
			e := int(clamp(v/top, 0, 1)*float64(h*8) + 0.5) // eighths filled
			cell := e - rowFromBottom*8
			c := col
			if col == heatColor {
				c = heat(float64(rowFromBottom+1) / float64(h) * 100)
			}
			switch {
			case cell >= 8:
				sb.WriteString(fg(c) + "█")
			case cell > 0:
				sb.WriteString(fg(c) + string(sparkRunes[cell]))
			case rowFromBottom == 0:
				sb.WriteString(fg(cTrack) + "▁")
			default:
				sb.WriteString(" ")
			}
		}
		out[r] = sb.String() + reset
	}
	return out
}

// ── history ─────────────────────────────────────────────────────────────────

const histCap = 600

type history map[string][]float64

var hist = history{}

func (h history) add(k string, v float64) {
	s := append(h[k], v)
	if len(s) > histCap {
		s = s[len(s)-histCap:]
	}
	h[k] = s
}

// record keeps the series the dashboard plots; call once per sample.
func record(s *Snapshot) {
	hist.add("cpu", s.CPU.Total)
	for _, k := range s.CPU.Cores {
		hist.add(fmt.Sprintf("core%d", k.ID), k.Usage)
	}
	hist.add("mem", pctOf(s.Mem.Used, s.Mem.Total))
	if a, ok := accel(s, "GPU"); ok && a.Load >= 0 {
		hist.add("gpu", a.Load)
	}
	if a, ok := accel(s, "NPU"); ok && a.Load >= 0 {
		hist.add("npu", a.Load)
	}
	vpu := 0.0
	for _, m := range s.Media {
		vpu = math.Max(vpu, m.Busy)
	}
	if len(s.Media) > 0 {
		hist.add("media", vpu)
	}
	hist.add("rx", s.NetRx)
	hist.add("tx", s.NetTx)
	hist.add("dr", s.DiskRead)
	hist.add("dw", s.DiskWrite)
	if t := tempFor(s, ""); t > 0 {
		hist.add("temp", t)
	}
}

// ── card ────────────────────────────────────────────────────────────────────

// card draws a rounded panel exactly w wide and h tall with the title set
// into the top-left border and meta into the top-right.
func card(title, meta string, col rgb, body []string, w, h int) []string {
	inner := w - 4
	for len(body) < h-2 {
		body = append(body, "")
	}
	body = body[:max(0, h-2)]
	b := fg(cBorder)
	t := clip(" "+bold+fg(col)+title+reset+" ", max(0, w-6))
	m := ""
	if meta != "" {
		m = " " + meta + " "
		if vlen(t)+vlen(m)+5 > w {
			m = ""
		}
	}
	fill := max(0, w-4-vlen(t)-vlen(m))
	out := []string{b + "╭─" + reset + t + b + strings.Repeat("─", fill) + reset + m + b + "─╮" + reset}
	for _, l := range body {
		out = append(out, b+"│"+reset+" "+fit(l, inner)+" "+b+"│"+reset)
	}
	return append(out, b+"╰"+strings.Repeat("─", w-2)+"╯"+reset)
}
