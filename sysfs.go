//go:build linux

package main

import (
	"os"
	"strconv"
	"strings"
)

func readStr(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(strings.ReplaceAll(string(b), "\x00", " "))
}

func readInt(path string) (int64, bool) {
	s := readStr(path)
	if s == "" {
		return 0, false
	}
	v, err := strconv.ParseInt(strings.Fields(s)[0], 10, 64)
	return v, err == nil
}
