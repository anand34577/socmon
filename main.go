//go:build linux

// socmon: a btop-style monitor for Rockchip and Allwinner SBCs
// (RK3588/RK3576/RK3566, Allwinner A733/A527; Radxa Cubie A7A/A5E, ROCK 5B...).
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"
)

func main() {
	interval := flag.Duration("i", time.Second, "refresh interval")
	once := flag.Bool("once", false, "print one JSON snapshot and exit")
	frame := flag.String("frame", "", "print one dashboard frame at WxH (e.g. 180x50) and exit")
	color := flag.String("color", "auto", "colour mode: auto (trust $COLORTERM), true (24-bit), 256")
	flag.Parse()
	switch *color {
	case "true":
		trueColor = true
	case "256":
		trueColor = false
	}
	tickSec = interval.Seconds()

	col := NewCollector()

	if *frame != "" {
		var w, h int
		if _, err := fmt.Sscanf(*frame, "%dx%d", &w, &h); err != nil || w < 40 || h < 10 {
			fmt.Fprintln(os.Stderr, "-frame wants WxH, e.g. 180x50")
			os.Exit(2)
		}
		col.Sample(true) // prime delta counters
		time.Sleep(time.Second)
		s := col.Sample(true)
		record(s)
		fmt.Print(render(s, w, h, sortCPU), reset, "\n")
		return
	}

	if *once || !isTTY() {
		col.Sample(true) // prime delta counters
		time.Sleep(500 * time.Millisecond)
		s := col.Sample(true)
		s.Procs = nil
		e := json.NewEncoder(os.Stdout)
		e.SetIndent("", "  ")
		if err := e.Encode(s); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		return
	}

	restore := makeRaw()
	fmt.Print("\x1b[?1049h\x1b[?25l")
	cleanup := func() {
		fmt.Print("\x1b[?25h\x1b[?1049l")
		restore()
	}
	defer cleanup()

	keys := make(chan byte, 8)
	go func() {
		buf := make([]byte, 1)
		for {
			if n, err := os.Stdin.Read(buf); n == 1 && err == nil {
				keys <- buf[0]
			} else if err != nil {
				close(keys)
				return
			}
		}
	}()
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	winch := make(chan os.Signal, 1)
	signal.Notify(winch, syscall.SIGWINCH)

	sortBy := sortCPU
	tick := time.NewTicker(*interval)
	defer tick.Stop()
	last := col.Sample(true)
	record(last)
	// Repaint without resampling, so resizes and key presses do not skew deltas.
	paint := func() {
		w, h := termSize()
		fmt.Print(render(last, w, h, sortBy))
	}
	paint()
	for {
		select {
		case <-tick.C:
			last = col.Sample(true)
			record(last)
			paint()
		case <-winch:
			fmt.Print("\x1b[2J")
			paint()
		case k, ok := <-keys:
			if !ok {
				return
			}
			switch k {
			case 'q', 'Q', 3:
				return
			case 'c':
				sortBy = sortCPU
			case 'm':
				sortBy = sortMem
			case 'i':
				sortBy = sortIO
			case 's':
				sortBy = (sortBy + 1) % sortModes
			default:
				continue
			}
			paint()
		case <-sig:
			return
		}
	}
}
