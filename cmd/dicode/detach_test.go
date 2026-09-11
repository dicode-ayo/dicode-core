package main

import (
	"os"
	"path/filepath"
	"testing"
)

// TestCliDataDirFor_NamedConfigWins: `dicode daemon --detach --config X` acts
// on the daemon belonging to X, not on whichever dicode.yaml the working
// directory happens to hold — the two can name different data directories,
// and so different control sockets.
func TestCliDataDirFor_NamedConfigWins(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("DICODE_DATA_DIR", "")
	t.Chdir(dir)
	writeConfigYAML(t, dir, "data_dir: \"${CONFIGDIR}/.cwd-dicode\"\n")

	// Elsewhere, so the test also pins ${CONFIGDIR} to the named config's own
	// directory rather than the working one.
	elsewhere := t.TempDir()
	named := filepath.Join(elsewhere, "other.yaml")
	if err := os.WriteFile(named, []byte("data_dir: \"${CONFIGDIR}/.named-dicode\"\n"), 0o600); err != nil {
		t.Fatalf("write other.yaml: %v", err)
	}

	want := filepath.Join(elsewhere, ".named-dicode")
	if got := cliDataDirFor(named); got != want {
		t.Errorf("cliDataDirFor(%q) = %q; want %q", named, got, want)
	}
}

// TestCliDataDirFor_EnvStillOutranksConfig: DICODE_DATA_DIR is someone stating
// which daemon they mean, and outranks a config path just as it outranks a
// discovered one.
func TestCliDataDirFor_EnvStillOutranksConfig(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("DICODE_DATA_DIR", "/data")
	t.Chdir(dir)
	writeConfigYAML(t, dir, "data_dir: \"${CONFIGDIR}/.cwd-dicode\"\n")

	if got := cliDataDirFor(filepath.Join(dir, "dicode.yaml")); got != "/data" {
		t.Errorf("cliDataDirFor() = %q; want /data", got)
	}
}
