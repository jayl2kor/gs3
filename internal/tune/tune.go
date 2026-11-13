// Package tune picks gs3's transfer defaults for the machine or container it
// runs in.
//
// gs3 is meant to be faster than the AWS CLI out of the box, so its defaults
// are more aggressive than the CLI's (10 concurrent requests, 8 MiB parts).
// But it also runs in Kubernetes steps limited to a fraction of a CPU and a
// few hundred MiB, so the defaults scale with the cgroup limits instead of
// being fixed numbers.
package tune

import (
	"os"
	"runtime"
	"runtime/debug"
	"strconv"
	"strings"

	"gs3/internal/awscfg"
)

// Limits are the resources available to the process.
type Limits struct {
	// CPUs is the CPU quota in cores; fractional under a cgroup limit.
	CPUs float64
	// MemoryBytes is the memory limit, or 0 when unlimited.
	MemoryBytes int64
}

// Settings are the resolved transfer settings.
type Settings struct {
	// Concurrency is the number of S3 requests in flight at once.
	Concurrency int
	QueueSize   int

	MultipartThreshold int64
	ChunkSize          int64
	CopyChunkSize      int64
	// StreamChunkSize and StreamWindow bound the memory of stdin/stdout
	// streaming, the only place parts are buffered.
	StreamChunkSize int64
	StreamWindow    int

	MaxBandwidth int64
	MaxParts     int
	ContentMD5   bool
	BatchDelete  bool
}

const (
	mib = 1 << 20

	// The AWS CLI's defaults, used with GS3_AWS_DEFAULTS=1.
	awsConcurrency = 10
	awsQueueSize   = 1000
	awsChunk       = 8 * mib
)

// Resolve combines, in order of precedence: GS3_* environment variables and
// explicit `s3 =` config values (both already merged into cfg), then defaults
// derived from the container limits.
func Resolve(limits Limits, cfg awscfg.S3Settings, getenv func(string) string) Settings {
	s := Settings{
		QueueSize:          2000,
		MultipartThreshold: 32 * mib,
		ChunkSize:          16 * mib,
		CopyChunkSize:      64 * mib,
		StreamChunkSize:    8 * mib,
		StreamWindow:       4,
		MaxParts:           10000,
		ContentMD5:         true,
		BatchDelete:        true,
	}

	// Small transfers are bound by request latency, not CPU, so concurrency
	// is what buys throughput; 32 per core, within sane bounds.
	s.Concurrency = clamp(int(32*limits.CPUs), 16, 128)
	switch {
	case limits.MemoryBytes > 0 && limits.MemoryBytes <= 256*mib:
		s.Concurrency = min(s.Concurrency, 32)
		s.StreamWindow = 2
	case limits.MemoryBytes > 0 && limits.MemoryBytes <= 512*mib:
		s.Concurrency = min(s.Concurrency, 64)
		s.StreamWindow = 2
	}

	if isOn(getenv("GS3_AWS_DEFAULTS")) {
		s.Concurrency, s.QueueSize = awsConcurrency, awsQueueSize
		s.MultipartThreshold, s.ChunkSize, s.CopyChunkSize = awsChunk, awsChunk, awsChunk
		s.StreamChunkSize, s.StreamWindow = awsChunk, 6
	}

	if cfg.MaxConcurrentRequests > 0 {
		s.Concurrency = cfg.MaxConcurrentRequests
	}
	if cfg.MaxQueueSize > 0 {
		s.QueueSize = cfg.MaxQueueSize
	}
	if cfg.MultipartThreshold > 0 {
		s.MultipartThreshold = cfg.MultipartThreshold
	}
	if cfg.MultipartChunksize > 0 {
		s.ChunkSize = cfg.MultipartChunksize
		s.CopyChunkSize = cfg.MultipartChunksize
		s.StreamChunkSize = cfg.MultipartChunksize
	}
	s.MaxBandwidth = cfg.MaxBandwidth

	if v, err := strconv.Atoi(getenv("GS3_MAX_PARTS")); err == nil && v > 0 {
		s.MaxParts = v
	}
	if v := getenv("GS3_CONTENT_MD5"); v != "" {
		s.ContentMD5 = isOn(v)
	}
	if v := getenv("GS3_BATCH_DELETE"); v != "" {
		s.BatchDelete = isOn(v)
	}
	return s
}

func isOn(v string) bool {
	switch strings.ToLower(v) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

func clamp(v, lo, hi int) int { return min(max(v, lo), hi) }

// Detect reads the cgroup CPU and memory limits (v2, then v1), falling back
// to the machine's CPU count and no memory limit.
func Detect() Limits {
	l := Limits{CPUs: float64(runtime.NumCPU())}
	if quota, ok := cpuQuota(); ok && quota > 0 && quota < l.CPUs {
		l.CPUs = quota
	}
	l.MemoryBytes = memoryLimit()
	return l
}

func readTrimmed(path string) (string, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", false
	}
	return strings.TrimSpace(string(data)), true
}

func cpuQuota() (float64, bool) {
	// cgroup v2: "<quota> <period>" or "max <period>".
	if v, ok := readTrimmed("/sys/fs/cgroup/cpu.max"); ok {
		fields := strings.Fields(v)
		if len(fields) == 2 && fields[0] != "max" {
			quota, err1 := strconv.ParseFloat(fields[0], 64)
			period, err2 := strconv.ParseFloat(fields[1], 64)
			if err1 == nil && err2 == nil && period > 0 {
				return quota / period, true
			}
		}
		return 0, false
	}
	// cgroup v1.
	q, ok1 := readTrimmed("/sys/fs/cgroup/cpu/cpu.cfs_quota_us")
	p, ok2 := readTrimmed("/sys/fs/cgroup/cpu/cpu.cfs_period_us")
	if ok1 && ok2 {
		quota, err1 := strconv.ParseFloat(q, 64)
		period, err2 := strconv.ParseFloat(p, 64)
		if err1 == nil && err2 == nil && quota > 0 && period > 0 {
			return quota / period, true
		}
	}
	return 0, false
}

func memoryLimit() int64 {
	for _, path := range []string{"/sys/fs/cgroup/memory.max", "/sys/fs/cgroup/memory/memory.limit_in_bytes"} {
		v, ok := readTrimmed(path)
		if !ok || v == "max" {
			continue
		}
		n, err := strconv.ParseInt(v, 10, 64)
		// cgroup v1 reports "no limit" as a huge number.
		if err == nil && n > 0 && n < 1<<50 {
			return n
		}
	}
	return 0
}

// ApplyMemoryLimit gives the Go runtime a soft memory limit below the
// container's hard one, so the garbage collector works harder before the
// kernel's OOM killer gets involved. A GOMEMLIMIT set by the user wins.
func ApplyMemoryLimit(limits Limits) {
	if limits.MemoryBytes <= 0 || os.Getenv("GOMEMLIMIT") != "" {
		return
	}
	limit := limits.MemoryBytes / 2
	limit = min(max(limit, 96*mib), 2048*mib)
	debug.SetMemoryLimit(limit)
}
