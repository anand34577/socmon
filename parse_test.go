//go:build linux

package main

import "testing"

// Lines copied from real boards (RK3566 ZERO 3, RK3576 ROCK 4D, RK3588 CM3588).
func TestMppLoadLine(t *testing.T) {
	m := reMppLoad.FindStringSubmatch("fdc38100.rkvdec-core      load:  12.50% utilization:   9.80%")
	if m == nil || m[1] != "fdc38100.rkvdec-core" || m[3] != "9.80" {
		t.Fatalf("got %q", m)
	}
}

func TestRgaLoad(t *testing.T) {
	in := "num of scheduler = 3\n================= load ==================\nscheduler[0]: rga3\n\t load = 7%\n-----------------------------------\nscheduler[1]: rga3\n\t load = 0%\n-----------------------------------\nscheduler[2]: rga2\n\t load = 31%\n"
	m := reRgaSched.FindAllStringSubmatch(in, -1)
	if len(m) != 3 || m[0][2] != "7" || m[2][1] != "rga2" || m[2][2] != "31" {
		t.Fatalf("got %q", m)
	}
}
