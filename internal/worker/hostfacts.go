package worker

// Host facts the admission step checks against. Every reader is a
// replaceable field on the Deployer so tests can pin a budget scenario
// (small RAM, low disk, many vCPUs) without touching the real host.

import (
	"bufio"
	"fmt"
	"os"
	"runtime"
	"strconv"
	"strings"
	"syscall"
)

// DockerDataPath is where the daemon stores containers and images; the
// disk guard watches the partition holding it.
const DockerDataPath = "/var/lib/docker"

// readMemTotalMB parses MemTotal from /proc/meminfo.
func readMemTotalMB() (int, error) {
	f, err := os.Open("/proc/meminfo")
	if err != nil {
		return 0, fmt.Errorf("open /proc/meminfo: %w", err)
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "MemTotal:") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			return 0, fmt.Errorf("parse MemTotal line %q", line)
		}
		kb, err := strconv.ParseInt(fields[1], 10, 64)
		if err != nil {
			return 0, fmt.Errorf("parse MemTotal value %q: %w", fields[1], err)
		}
		return int(kb / 1024), nil
	}
	if err := sc.Err(); err != nil {
		return 0, fmt.Errorf("read /proc/meminfo: %w", err)
	}
	return 0, fmt.Errorf("MemTotal not found in /proc/meminfo")
}

// statfsFreeMB reports the space available to an unprivileged process
// (Bavail, not Bfree: reserved blocks are not ours to hand out) on the
// filesystem holding path.
func statfsFreeMB(path string) (int, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, fmt.Errorf("statfs %s: %w", path, err)
	}
	free := int64(st.Bavail) * int64(st.Frsize)
	return int(free / (1024 * 1024)), nil
}

// numCPU counts logical CPUs visible to the process.
func numCPU() int { return runtime.NumCPU() }
