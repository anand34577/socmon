//go:build linux

package main

import (
	"os"
	"strings"
)

// SoCProfile holds static facts about a supported chip. Runtime values (load,
// frequency) always come from sysfs; the profile only supplies labels and
// capacity hints so the UI is meaningful on every board.
type SoCProfile struct {
	Name    string
	Vendor  string // "rockchip" | "allwinner"
	Match   []string
	CPU     string
	GPU     string
	NPU     string
	NPUTops float64
	NPUCore int
}

var profiles = []SoCProfile{
	{"RK3588", "rockchip", []string{"rk3588"}, "4xA76 + 4xA55", "Mali-G610 MP4", "RKNPU", 6, 3},
	{"RK3576", "rockchip", []string{"rk3576"}, "4xA72 + 4xA53", "Mali-G52 MC3", "RKNPU", 6, 2},
	{"RK3566", "rockchip", []string{"rk3566", "rk3568"}, "4xA55", "Mali-G52 2EE", "RKNPU", 0.8, 1},
	{"Allwinner A733", "allwinner", []string{"a733", "sun60i"}, "2xA76 + 6xA55", "IMG BXM-4-64", "VIP9000", 3, 1},
	{"Allwinner A527/T527", "allwinner", []string{"a527", "t527", "sun55i"}, "8xA55", "Mali-G57 MC1", "VIP", 2, 1},
}

// boardNames maps device-tree compatible/model fragments to marketing names.
var boardNames = []struct{ Match, Name string }{
	{"cubie-a7a", "Radxa Cubie A7A"},
	{"cubie-a7s", "Radxa Cubie A7S"},
	{"cubie-a5e", "Radxa Cubie A5E"},
	{"rock-5b", "Radxa ROCK 5B"},
	{"rock-5a", "Radxa ROCK 5A"},
	{"rock-4d", "Radxa ROCK 4D"},
	{"rock-3a", "Radxa ROCK 3A"},
	{"rock-3c", "Radxa ROCK 3C"},
}

type BoardInfo struct {
	Model    string  `json:"model"`
	Board    string  `json:"board,omitempty"`
	SoC      string  `json:"soc"`
	Vendor   string  `json:"vendor,omitempty"`
	CPU      string  `json:"cpu,omitempty"`
	GPU      string  `json:"gpu,omitempty"`
	NPU      string  `json:"npu,omitempty"`
	NPUTops  float64 `json:"npu_tops,omitempty"`
	NPUCores int     `json:"npu_cores,omitempty"`
	Kernel   string  `json:"kernel"`
}

func detectBoard() BoardInfo {
	b := BoardInfo{Model: readStr("/proc/device-tree/model"), Kernel: readStr("/proc/sys/kernel/osrelease")}
	hay := strings.ToLower(readStr("/proc/device-tree/compatible") + " " + b.Model)
	hay = strings.ReplaceAll(hay, " ", "-")
	for _, n := range boardNames {
		if strings.Contains(hay, n.Match) {
			b.Board = n.Name
			break
		}
	}
	for i := range profiles {
		for _, m := range profiles[i].Match {
			if strings.Contains(hay, m) {
				p := profiles[i]
				b.SoC, b.Vendor, b.CPU, b.GPU, b.NPU, b.NPUTops, b.NPUCores = p.Name, p.Vendor, p.CPU, p.GPU, p.NPU, p.NPUTops, p.NPUCore
				return b
			}
		}
	}
	b.SoC = "unknown"
	if h, err := os.Hostname(); err == nil && b.Model == "" {
		b.Model = h
	}
	return b
}
