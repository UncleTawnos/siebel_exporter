package foldermonitor

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// writeFile creates a file with content of the given byte length.
func writeFile(t *testing.T, path string, size int) {
	t.Helper()
	if err := os.WriteFile(path, make([]byte, size), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func TestScanFolderSumsRecursively(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "a.txt"), 100)
	sub := filepath.Join(root, "sub")
	if err := os.Mkdir(sub, 0o750); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(sub, "b.txt"), 250)

	size, files, errs := scanFolder(root)
	if size != 350 {
		t.Errorf("size = %d, want 350", size)
	}
	if files != 2 {
		t.Errorf("files = %d, want 2", files)
	}
	if errs != 0 {
		t.Errorf("errs = %d, want 0", errs)
	}
}

func TestScanFolderMissingPathCountsError(t *testing.T) {
	_, _, errs := scanFolder(filepath.Join(t.TempDir(), "does-not-exist"))
	if errs == 0 {
		t.Errorf("errs = 0, want >0 for missing path")
	}
}

func TestExpandChildrenReportsPerSubfolder(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{"one", "two"} {
		dir := filepath.Join(root, name)
		if err := os.Mkdir(dir, 0o750); err != nil {
			t.Fatal(err)
		}
		writeFile(t, filepath.Join(dir, "f.txt"), 10)
	}
	// A loose file at the root should be ignored in expand mode (only dirs become series).
	writeFile(t, filepath.Join(root, "loose.txt"), 999)

	m := New([]Target{{Path: root, ExpandChildren: true}}, time.Minute)
	m.runScan()

	m.mu.RLock()
	defer m.mu.RUnlock()
	if len(m.results) != 2 {
		t.Fatalf("got %d results, want 2: %+v", len(m.results), m.results)
	}
	for _, name := range []string{"one", "two"} {
		p := filepath.Join(root, name)
		r, ok := m.results[p]
		if !ok {
			t.Errorf("missing result for %s", p)
			continue
		}
		if r.sizeBytes != 10 {
			t.Errorf("%s size = %d, want 10", p, r.sizeBytes)
		}
	}
}

func TestCollectEmitsMetrics(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "a.txt"), 42)

	m := New([]Target{{Path: root}}, time.Minute)
	m.runScan()

	ch := make(chan prometheus.Metric, 16)
	m.Collect(ch)
	close(ch)

	count := 0
	for range ch {
		count++
	}
	// 6 metrics per path: size, files, duration, scan_errors, success, last_scan.
	if count != 6 {
		t.Errorf("collected %d metrics, want 6", count)
	}
}

func TestLoadConfig(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "folder-monitor.toml")
	content := `
ScanInterval = "30m"

[[Folder]]
Path = "/var/log"
ExpandChildren = true

[[Folder]]
Path = "/opt/app"
`
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if len(cfg.Folder) != 2 {
		t.Fatalf("got %d folders, want 2", len(cfg.Folder))
	}
	if !cfg.Folder[0].ExpandChildren || cfg.Folder[1].ExpandChildren {
		t.Errorf("ExpandChildren not parsed correctly: %+v", cfg.Folder)
	}
	interval, err := cfg.Interval()
	if err != nil {
		t.Fatalf("Interval: %v", err)
	}
	if interval != 30*time.Minute {
		t.Errorf("interval = %s, want 30m", interval)
	}
}

func TestConfigIntervalDefaultsAndErrors(t *testing.T) {
	empty := &Config{}
	if got, err := empty.Interval(); err != nil || got != DefaultScanInterval {
		t.Errorf("empty interval = %s, %v; want %s, nil", got, err, DefaultScanInterval)
	}
	bad := &Config{ScanInterval: "not-a-duration"}
	if _, err := bad.Interval(); err == nil {
		t.Errorf("expected error for invalid ScanInterval")
	}
}

func TestLoadConfigMissingFileIsNotExist(t *testing.T) {
	_, err := LoadConfig(filepath.Join(t.TempDir(), "absent.toml"))
	if !os.IsNotExist(err) {
		t.Errorf("err = %v, want os.IsNotExist", err)
	}
}

func TestScanErrorsAccumulate(t *testing.T) {
	m := New(nil, time.Minute)
	missing := filepath.Join(t.TempDir(), "nope")
	m.scanOne(missing)
	m.scanOne(missing)

	m.mu.RLock()
	defer m.mu.RUnlock()
	r := m.results[missing]
	if r == nil || r.scanErrors < 2 {
		t.Errorf("scanErrors = %v, want >=2 (cumulative across scans)", r)
	}
}
