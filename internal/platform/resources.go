package platform

import (
	"os"
	"runtime"
	"strconv"
	"strings"
)

// Resources describes the CPU and memory available to the process.
type Resources struct {
	CPUs           int    `json:"cpus"`
	MemoryBytes    int64  `json:"memoryBytes"`
	CgroupVersion  int    `json:"cgroupVersion"`
	CPUDetected    bool   `json:"cpuDetected"`
	MemoryDetected bool   `json:"memoryDetected"`
	CPUSource      string `json:"cpuSource"`
	MemorySource   string `json:"memorySource"`
}

// DetectResources reads cgroup v1/v2 limits, falling back to runtime and /proc.
func DetectResources() Resources {
	res := Resources{CPUs: runtime.NumCPU(), CPUSource: "runtime", CgroupVersion: 0}
	if n, source, ok := detectCPU(); ok {
		res.CPUs = n
		res.CPUDetected = true
		res.CPUSource = source
	}
	if mem, source, version, ok := detectMemory(); ok {
		res.MemoryBytes = mem
		res.MemoryDetected = true
		res.MemorySource = source
		res.CgroupVersion = version
	}
	if res.MemoryBytes == 0 {
		res.MemoryBytes = readMemTotal()
		res.MemorySource = "proc"
	}
	if res.CPUs < 1 {
		res.CPUs = 1
	}
	return res
}

func detectCPU() (int, string, bool) {
	if raw, err := os.ReadFile("/sys/fs/cgroup/cpu.max"); err == nil {
		fields := strings.Fields(string(raw))
		if len(fields) == 2 && fields[0] != "max" {
			quota, err1 := strconv.ParseFloat(fields[0], 64)
			period, err2 := strconv.ParseFloat(fields[1], 64)
			if err1 == nil && err2 == nil && quota > 0 && period > 0 {
				n := int(ceilFloat(quota / period))
				if n < 1 {
					n = 1
				}
				return n, "cgroup_v2", true
			}
		}
	}
	quotaRaw, err1 := os.ReadFile("/sys/fs/cgroup/cpu/cpu.cfs_quota_us")
	periodRaw, err2 := os.ReadFile("/sys/fs/cgroup/cpu/cpu.cfs_period_us")
	if err1 == nil && err2 == nil {
		quota, e1 := strconv.ParseInt(strings.TrimSpace(string(quotaRaw)), 10, 64)
		period, e2 := strconv.ParseInt(strings.TrimSpace(string(periodRaw)), 10, 64)
		if e1 == nil && e2 == nil && quota > 0 && period > 0 {
			n := int(ceilFloat(float64(quota) / float64(period)))
			if n < 1 {
				n = 1
			}
			return n, "cgroup_v1", true
		}
	}
	if raw, err := os.ReadFile("/sys/fs/cgroup/cpuset.cpus.effective"); err == nil {
		if n := countCPUSet(strings.TrimSpace(string(raw))); n > 0 {
			return n, "cpuset", true
		}
	}
	return 0, "", false
}

func detectMemory() (int64, string, int, bool) {
	if raw, err := os.ReadFile("/sys/fs/cgroup/memory.max"); err == nil {
		value := strings.TrimSpace(string(raw))
		if value != "max" {
			if n, err := strconv.ParseInt(value, 10, 64); err == nil && n > 0 {
				return n, "cgroup_v2", 2, true
			}
		}
	}
	if raw, err := os.ReadFile("/sys/fs/cgroup/memory/memory.limit_in_bytes"); err == nil {
		value := strings.TrimSpace(string(raw))
		if n, err := strconv.ParseInt(value, 10, 64); err == nil && n > 0 && n < 1<<60 {
			return n, "cgroup_v1", 1, true
		}
	}
	return 0, "", 0, false
}

func readMemTotal() int64 {
	raw, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return 0
	}
	for _, line := range strings.Split(string(raw), "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 2 && fields[0] == "MemTotal:" {
			if kb, err := strconv.ParseInt(fields[1], 10, 64); err == nil {
				return kb * 1024
			}
		}
	}
	return 0
}

func countCPUSet(spec string) int {
	total := 0
	for _, part := range strings.Split(spec, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		if strings.Contains(part, "-") {
			bounds := strings.SplitN(part, "-", 2)
			start, err1 := strconv.Atoi(bounds[0])
			end, err2 := strconv.Atoi(bounds[1])
			if err1 == nil && err2 == nil && end >= start {
				total += end - start + 1
			}
			continue
		}
		if _, err := strconv.Atoi(part); err == nil {
			total++
		}
	}
	return total
}

func ceilFloat(v float64) float64 {
	n := float64(int(v))
	if v > n {
		return n + 1
	}
	return n
}
