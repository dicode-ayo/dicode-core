package config

import (
	"path/filepath"
	"testing"
)

func TestResolveDataDir(t *testing.T) {
	const (
		home      = "/home/tester"
		configDir = "/srv/project"
	)
	tests := []struct {
		name string
		env  string
		raw  string
		home string
		want string
	}{
		{"empty falls back to home", "", "", home, home + "/.dicode"},
		{"absolute", "", "/var/lib/dicode", home, "/var/lib/dicode"},
		{"tilde", "", "~/.dicode", home, home + "/.dicode"},
		{"home var", "", "${HOME}/state", home, home + "/state"},
		{"config dir var", "", "${CONFIGDIR}/.dicode", home, configDir + "/.dicode"},
		// DATADIR is not yet bound while data_dir itself is expanded, so an
		// unrecognised variable survives verbatim rather than emptying.
		{"self-referential datadir", "", "${DATADIR}/x", home, "${DATADIR}/x"},
		{"unknown var", "", "${NOPE}/x", home, "${NOPE}/x"},
		{"home var with home unset", "", "${HOME}/state", "", "/state"},
		{"tilde with home unset", "", "~/x", "", "~/x"},
		{"env with no config value", "/data", "", home, "/data"},
		{"env outranks config value", "/data", "/var/lib/dicode", home, "/data"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(DataDirEnvVar, tc.env)
			if got := ResolveDataDir(tc.raw, configDir, tc.home); got != tc.want {
				t.Errorf("ResolveDataDir(%q, %q, %q) = %q; want %q", tc.raw, configDir, tc.home, got, tc.want)
			}
		})
	}
}

// TestLoadBytes_EnvOutranksConfiguredDataDir pins the daemon to the same
// precedence the CLI uses when it picks the control socket. The two resolve
// the data dir from different inputs, and a disagreement puts the daemon's
// socket somewhere the CLI never dials — so the CLI starts a second daemon,
// which unlinks the live socket and rebinds it.
func TestLoadBytes_EnvOutranksConfiguredDataDir(t *testing.T) {
	dataDir := t.TempDir()
	t.Setenv(DataDirEnvVar, dataDir)

	cfg, err := LoadBytes([]byte("data_dir: /var/lib/dicode\n"), "/srv/project")
	if err != nil {
		t.Fatalf("LoadBytes: %v", err)
	}
	if cfg.DataDir != dataDir {
		t.Errorf("cfg.DataDir = %q; want %q", cfg.DataDir, dataDir)
	}
}

// TestLoadBytes_EnvRedirectsDerivedPaths: everything the config derives from
// the data dir has to follow it, or the daemon writes its SQLite file outside
// the directory it reports as its data dir.
func TestLoadBytes_EnvRedirectsDerivedPaths(t *testing.T) {
	dataDir := t.TempDir()
	t.Setenv(DataDirEnvVar, dataDir)

	cfg, err := LoadBytes([]byte("data_dir: /var/lib/dicode\n"), "/srv/project")
	if err != nil {
		t.Fatalf("LoadBytes: %v", err)
	}
	if want := filepath.Join(dataDir, "data.db"); cfg.Database.Path != want {
		t.Errorf("cfg.Database.Path = %q; want %q", cfg.Database.Path, want)
	}
}
