package main

import (
	"net/http"
	"os"
	"runtime"
	"strconv"
	"strings"
)

// Linux reports current and lifetime peak resident memory in KiB.
func memoryUsage(w http.ResponseWriter, _ *http.Request, _ string) {
	var stats runtime.MemStats
	runtime.ReadMemStats(&stats)
	out := M{"heapBytes": stats.HeapAlloc, "sampledAt": now()}
	if runtime.GOOS == "linux" {
		status, _ := os.ReadFile("/proc/self/status")
		for _, line := range strings.Split(string(status), "\n") {
			name, value, _ := strings.Cut(line, ":")
			if name != "VmRSS" && name != "VmHWM" {
				continue
			}
			fields := strings.Fields(value)
			if len(fields) != 2 || fields[1] != "kB" {
				continue
			}
			kib, err := strconv.ParseUint(fields[0], 10, 54)
			if err != nil {
				continue
			}
			if name == "VmRSS" {
				out["rssBytes"] = kib * 1024
			} else {
				out["peakRssBytes"] = kib * 1024
			}
		}
	}
	jsonOut(w, 200, out)
}
