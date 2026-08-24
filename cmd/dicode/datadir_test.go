package main

import (
	"os"
	"path/filepath"
	"testing"
)

// writeConfigYAML drops a dicode.yaml with the given body into dir.
func writeConfigYAML(t *testing.T, dir, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, "dicode.yaml"), []byte(body), 0o600); err != nil {
		t.Fatalf("write dicode.yaml: %v", err)
	}
}

// TestCliDataDir_ConfigDataDirWins: `dicode init` writes
// data_dir: ${CONFIGDIR}/.dicode and the daemon puts its socket there, so the
// CLI has to resolve the same directory rather than $HOME.
func TestCliDataDir_ConfigDataDirWins(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("DICODE_DATA_DIR", "")
	t.Chdir(dir)
	writeConfigYAML(t, dir, "data_dir: \"${CONFIGDIR}/.dicode\"\n")

	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	want := filepath.Join(wd, ".dicode")
	if got := cliDataDir(); got != want {
		t.Errorf("cliDataDir() = %q; want %q", got, want)
	}
}

// TestCliDataDir_ConfigWithoutDataDirDefaultsToHome mirrors pkg/config's
// applyDefaults, which defaults an absent data_dir to $HOME/.dicode.
func TestCliDataDir_ConfigWithoutDataDirDefaultsToHome(t *testing.T) {
	dir := t.TempDir()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("DICODE_DATA_DIR", "")
	t.Chdir(dir)
	writeConfigYAML(t, dir, "log_level: info\n")

	want := filepath.Join(home, ".dicode")
	if got := cliDataDir(); got != want {
		t.Errorf("cliDataDir() = %q; want %q", got, want)
	}
}

// TestCliDataDir_EnvOutranksConfig: the config is discovered from wherever the
// process is standing, so a caller running the CLI from an unrelated directory
// that happens to hold a dicode.yaml must still reach the daemon they named.
func TestCliDataDir_EnvOutranksConfig(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("DICODE_DATA_DIR", "/data")
	t.Chdir(dir)
	writeConfigYAML(t, dir, "data_dir: \"${CONFIGDIR}/.dicode\"\n")

	if got := cliDataDir(); got != "/data" {
		t.Errorf("cliDataDir() = %q; want /data", got)
	}
}

// TestCliDataDir_NoConfigHonorsEnv covers the pre-onboarding window the
// Docker image relies on: no config has been written yet, so DICODE_DATA_DIR
// is the only signal, and onboarding will bake the same path in.
func TestCliDataDir_NoConfigHonorsEnv(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("DICODE_DATA_DIR", "/data")
	t.Chdir(t.TempDir())

	if got := cliDataDir(); got != "/data" {
		t.Errorf("cliDataDir() = %q; want /data", got)
	}
}

func TestCliDataDir_NoConfigFallsBackToHome(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("DICODE_DATA_DIR", "")
	t.Chdir(t.TempDir())

	want := filepath.Join(home, ".dicode")
	if got := cliDataDir(); got != want {
		t.Errorf("cliDataDir() = %q; want %q", got, want)
	}
}

// TestCliDataDir_MalformedConfigFallsBack: reporting the parse error is the
// daemon's job, so the CLI has to get far enough to let it.
func TestCliDataDir_MalformedConfigFallsBack(t *testing.T) {
	dir := t.TempDir()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("DICODE_DATA_DIR", "")
	t.Chdir(dir)
	writeConfigYAML(t, dir, "data_dir: [this is not a string\n")

	want := filepath.Join(home, ".dicode")
	if got := cliDataDir(); got != want {
		t.Errorf("cliDataDir() = %q; want %q", got, want)
	}
}

func TestExpandConfigDataDir(t *testing.T) {
	const home = "/home/tester"
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	tests := []struct {
		name  string
		value string
		home  string
		want  string
	}{
		{"empty", "", home, ""},
		{"absolute", "/var/lib/dicode", home, "/var/lib/dicode"},
		{"tilde", "~/.dicode", home, home + "/.dicode"},
		{"home var", "${HOME}/state", home, home + "/state"},
		{"config dir var", "${CONFIGDIR}/.dicode", home, filepath.Join(wd, ".dicode")},
		// pkg/config binds only HOME and CONFIGDIR while data_dir is
		// expanded, so anything else survives verbatim into the daemon's data
		// dir and has to survive here too.
		{"self-referential datadir", "${DATADIR}/x", home, "${DATADIR}/x"},
		{"unknown var", "${NOPE}/x", home, "${NOPE}/x"},
		// applyDefaults discards the UserHomeDir error and expandHome leaves
		// ~ alone on it, so an unset home empties ${HOME} but not ~.
		{"home var with home unset", "${HOME}/state", "", "/state"},
		{"tilde with home unset", "~/x", "", "~/x"},
		{"absolute with home unset", "/srv/dicode", "", "/srv/dicode"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := expandConfigDataDir(tc.value, tc.home, wd); got != tc.want {
				t.Errorf("expandConfigDataDir(%q, %q) = %q; want %q", tc.value, tc.home, got, tc.want)
			}
		})
	}
}

func TestCliDataDir_UnknownVariableMatchesDaemon(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("DICODE_DATA_DIR", "")
	t.Chdir(dir)
	writeConfigYAML(t, dir, "data_dir: \"${DATADIR}/state\"\n")

	if got := cliDataDir(); got != "${DATADIR}/state" {
		t.Errorf("cliDataDir() = %q; want the literal the daemon uses", got)
	}
}

// TestCliDataDir_IgnoresConfigOwnedByAnotherUser: dicode.yaml is read from the
// working directory, which the user does not necessarily control. A config
// they do not own must not choose where this process appends the daemon log
// (which carries the first-run passphrase) or sends request payloads.
func TestCliDataDir_IgnoresConfigOwnedByAnotherUser(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("root owns every file, so the gate cannot be exercised")
	}
	// /etc/hosts stands in for a regular file owned by another user; the test
	// only needs cliDataDir to refuse a config it does not own.
	fi, err := os.Lstat("/etc/hosts")
	if err != nil || ownedByCurrentUser(fi) {
		t.Skip("need a regular file owned by another user")
	}

	dir := t.TempDir()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("DICODE_DATA_DIR", "")
	t.Chdir(dir)
	if err := os.Symlink("/etc/hosts", filepath.Join(dir, "dicode.yaml")); err != nil {
		t.Skipf("symlink: %v", err)
	}

	want := filepath.Join(home, ".dicode")
	if got := cliDataDir(); got != want {
		t.Errorf("cliDataDir() = %q; want the trusted default %q", got, want)
	}
}

// TestCliDataDir_SymlinkedConfigIsRefused: Lstat, not Stat — a symlink the
// user owns pointing at someone else's file would otherwise pass the gate.
func TestCliDataDir_SymlinkedConfigIsRefused(t *testing.T) {
	dir := t.TempDir()
	home := t.TempDir()
	target := filepath.Join(t.TempDir(), "planted.yaml")
	if err := os.WriteFile(target, []byte("data_dir: /tmp/attacker\n"), 0o600); err != nil {
		t.Fatalf("write target: %v", err)
	}
	t.Setenv("HOME", home)
	t.Setenv("DICODE_DATA_DIR", "")
	t.Chdir(dir)
	if err := os.Symlink(target, filepath.Join(dir, "dicode.yaml")); err != nil {
		t.Skipf("symlink: %v", err)
	}

	want := filepath.Join(home, ".dicode")
	if got := cliDataDir(); got != want {
		t.Errorf("cliDataDir() = %q; want the trusted default %q", got, want)
	}
}
