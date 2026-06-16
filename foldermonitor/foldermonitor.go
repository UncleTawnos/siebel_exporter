// Package foldermonitor periodically measures the on-disk size of configured
// folders and exposes the results as native Prometheus metrics.
//
// It replaces the standalone disk-space scripts (e.g. 1_CDiscSpaceMonitor_text.py
// and 1_GDiscSpaceMonitor_text.py) that recursively summed folder sizes and
// shipped the result as text logs. The scan runs on its own background ticker
// and the Prometheus Collect path only ever reads cached values, so a slow
// filesystem walk never blocks a metrics scrape.
package foldermonitor

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/BurntSushi/toml"
	"github.com/barkadron/siebel_exporter/log"
	"github.com/prometheus/client_golang/prometheus"
)

const (
	namespace = "siebel"
	subsystem = "folder"
)

// DefaultScanInterval is used when the config omits or has an invalid ScanInterval.
const DefaultScanInterval = time.Hour

// Target describes a single folder to monitor.
type Target struct {
	// Path is the folder to measure.
	Path string `toml:"Path"`
	// ExpandChildren, when true, reports each immediate subfolder of Path as its
	// own series instead of reporting the aggregate size of Path. This mirrors
	// the original drive-monitor scripts, which reported one row per top-level
	// folder of a drive.
	ExpandChildren bool `toml:"ExpandChildren"`
}

// Config is the TOML configuration for folder monitoring.
type Config struct {
	// ScanInterval is a Go duration string (e.g. "1h", "10m") between scans.
	ScanInterval string `toml:"ScanInterval"`
	// Folder is the list of folders to monitor.
	Folder []Target `toml:"Folder"`
}

// LoadConfig reads and parses a folder-monitor TOML config file. The returned
// error wraps os.ErrNotExist when the file is absent, so callers can treat a
// missing default config as "disabled" rather than fatal.
func LoadConfig(path string) (*Config, error) {
	var cfg Config
	if _, err := toml.DecodeFile(path, &cfg); err != nil {
		return nil, err
	}
	return &cfg, nil
}

// Interval returns the parsed scan interval, falling back to DefaultScanInterval
// when unset. A non-nil error indicates an unparsable ScanInterval value.
func (c *Config) Interval() (time.Duration, error) {
	if strings.TrimSpace(c.ScanInterval) == "" {
		return DefaultScanInterval, nil
	}
	return time.ParseDuration(c.ScanInterval)
}

// result is the cached outcome of the most recent scan of a single path.
type result struct {
	path       string
	sizeBytes  int64
	fileCount  int64
	scanErrors int64 // cumulative across scans, so it can be exposed as a counter
	duration   time.Duration
	lastScan   time.Time
	success    bool
}

// Monitor periodically scans configured folders and implements prometheus.Collector.
type Monitor struct {
	targets  []Target
	interval time.Duration

	mu      sync.RWMutex
	results map[string]*result

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup

	sizeDesc       *prometheus.Desc
	filesDesc      *prometheus.Desc
	durationDesc   *prometheus.Desc
	successDesc    *prometheus.Desc
	lastScanDesc   *prometheus.Desc
	scanErrorsDesc *prometheus.Desc
}

// New creates a Monitor for the given targets. Call Start to begin scanning and
// register it with Prometheus.
func New(targets []Target, interval time.Duration) *Monitor {
	ctx, cancel := context.WithCancel(context.Background())
	return &Monitor{
		targets:  targets,
		interval: interval,
		results:  make(map[string]*result),
		ctx:      ctx,
		cancel:   cancel,
		sizeDesc: prometheus.NewDesc(
			prometheus.BuildFQName(namespace, subsystem, "size_bytes"),
			"Total size in bytes of the monitored folder (sum of regular files, symlinks not followed).",
			[]string{"path"}, nil),
		filesDesc: prometheus.NewDesc(
			prometheus.BuildFQName(namespace, subsystem, "files_total"),
			"Number of regular files counted in the monitored folder during the last scan.",
			[]string{"path"}, nil),
		durationDesc: prometheus.NewDesc(
			prometheus.BuildFQName(namespace, subsystem, "scan_duration_seconds"),
			"Duration of the last folder scan in seconds.",
			[]string{"path"}, nil),
		successDesc: prometheus.NewDesc(
			prometheus.BuildFQName(namespace, subsystem, "scan_success"),
			"Whether the last folder scan completed (1) or failed to start (0).",
			[]string{"path"}, nil),
		lastScanDesc: prometheus.NewDesc(
			prometheus.BuildFQName(namespace, subsystem, "last_scan_timestamp_seconds"),
			"Unix timestamp of the last folder scan.",
			[]string{"path"}, nil),
		scanErrorsDesc: prometheus.NewDesc(
			prometheus.BuildFQName(namespace, subsystem, "scan_errors_total"),
			"Cumulative number of per-entry errors (e.g. permission denied) encountered while walking the folder.",
			[]string{"path"}, nil),
	}
}

// Start launches the background scanner. It performs the first scan asynchronously
// so a slow walk does not delay startup.
func (m *Monitor) Start() {
	log.Infof("foldermonitor: starting with %d target(s), scan interval %s", len(m.targets), m.interval)
	m.wg.Add(1)
	go m.run()
}

// Stop signals the background scanner to terminate and waits for it to finish.
func (m *Monitor) Stop() {
	log.Debugln("foldermonitor: stopping")
	m.cancel()
	m.wg.Wait()
}

func (m *Monitor) run() {
	defer m.wg.Done()
	m.runScan() // immediate first scan
	ticker := time.NewTicker(m.interval)
	defer ticker.Stop()
	for {
		select {
		case <-m.ctx.Done():
			log.Debugln("foldermonitor: scanner received shutdown signal")
			return
		case <-ticker.C:
			m.runScan()
		}
	}
}

// runScan measures every configured target once.
func (m *Monitor) runScan() {
	for _, t := range m.targets {
		if m.ctx.Err() != nil {
			return
		}
		if t.ExpandChildren {
			m.scanChildren(t.Path)
		} else {
			m.scanOne(t.Path)
		}
	}
}

// scanChildren reports each immediate subfolder of root as its own series.
func (m *Monitor) scanChildren(root string) {
	entries, err := os.ReadDir(root)
	if err != nil {
		log.Errorf("foldermonitor: cannot list children of '%s': %v", root, err)
		m.record(root, 0, 0, 1, 0, false)
		return
	}
	for _, e := range entries {
		if m.ctx.Err() != nil {
			return
		}
		if e.IsDir() {
			m.scanOne(filepath.Join(root, e.Name()))
		}
	}
}

// scanOne walks a single folder and records the cached result.
func (m *Monitor) scanOne(path string) {
	start := time.Now()
	size, files, errs := scanFolder(path)
	m.record(path, size, files, errs, time.Since(start), true)
	log.Debugf("foldermonitor: scanned '%s' -> %d bytes, %d files, %d errors", path, size, files, errs)
}

// scanFolder recursively sums the size of regular files under path. Symlinks are
// not followed and per-entry errors are counted and skipped rather than aborting
// the walk, matching the behaviour of the scripts this replaces.
func scanFolder(path string) (size, files, errs int64) {
	walkErr := filepath.WalkDir(path, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			errs++
			log.Debugf("foldermonitor: error accessing '%s': %v", p, err)
			if d != nil && d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			return nil
		}
		info, ierr := d.Info()
		if ierr != nil {
			errs++
			log.Debugf("foldermonitor: cannot stat '%s': %v", p, ierr)
			return nil
		}
		// Skip symlinks (follow_symlinks=false in the original scripts).
		if info.Mode()&fs.ModeSymlink != 0 {
			return nil
		}
		size += info.Size()
		files++
		return nil
	})
	if walkErr != nil {
		errs++
		log.Errorf("foldermonitor: error walking '%s': %v", path, walkErr)
	}
	return size, files, errs
}

func (m *Monitor) record(path string, size, files, errs int64, dur time.Duration, success bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.results[path]
	if !ok {
		r = &result{path: path}
		m.results[path] = r
	}
	r.sizeBytes = size
	r.fileCount = files
	r.scanErrors += errs
	r.duration = dur
	r.lastScan = time.Now()
	r.success = success
}

// Describe implements prometheus.Collector.
func (m *Monitor) Describe(ch chan<- *prometheus.Desc) {
	ch <- m.sizeDesc
	ch <- m.filesDesc
	ch <- m.durationDesc
	ch <- m.successDesc
	ch <- m.lastScanDesc
	ch <- m.scanErrorsDesc
}

// Collect implements prometheus.Collector. It only reads cached scan results and
// never touches the filesystem.
func (m *Monitor) Collect(ch chan<- prometheus.Metric) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, r := range m.results {
		ch <- prometheus.MustNewConstMetric(m.sizeDesc, prometheus.GaugeValue, float64(r.sizeBytes), r.path)
		ch <- prometheus.MustNewConstMetric(m.filesDesc, prometheus.GaugeValue, float64(r.fileCount), r.path)
		ch <- prometheus.MustNewConstMetric(m.durationDesc, prometheus.GaugeValue, r.duration.Seconds(), r.path)
		ch <- prometheus.MustNewConstMetric(m.scanErrorsDesc, prometheus.CounterValue, float64(r.scanErrors), r.path)
		success := 0.0
		if r.success {
			success = 1.0
		}
		ch <- prometheus.MustNewConstMetric(m.successDesc, prometheus.GaugeValue, success, r.path)
		ch <- prometheus.MustNewConstMetric(m.lastScanDesc, prometheus.GaugeValue, float64(r.lastScan.Unix()), r.path)
	}
}
