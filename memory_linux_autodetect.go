//go:build linux

package memory

import (
	"os"
	"strconv"
	"strings"
)

func init() {
	// Detect actual huge page size from /proc/meminfo on Linux.
	// Falls back to 2MB if detection fails (x86_64 default).
	if data, err := os.ReadFile("/proc/meminfo"); err == nil {
		for _, line := range strings.Split(string(data), "\n") {
			if strings.HasPrefix(line, "Hugepagesize:") {
				fields := strings.Fields(line)
				// Format: "Hugepagesize:    2048 kB"
				// fields[0]="Hugepagesize:", fields[1]="2048", fields[2]="kB"
				if len(fields) >= 3 && fields[2] == "kB" {
					if size, err := strconv.ParseUint(fields[1], 10, 64); err == nil {
						HugepageSize = size * 1024
					}
				}
				break
			}
		}
	}
}
