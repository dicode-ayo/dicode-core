package config

import (
	"os"
	"strings"
)

// DataDirEnvVar names the environment variable that overrides the configured
// data directory.
const DataDirEnvVar = "DICODE_DATA_DIR"

// DefaultDataDirName is the data directory under $HOME used when neither the
// environment nor the config names one.
const DefaultDataDirName = ".dicode"

// ResolveDataDir answers which directory holds the SQLite database, the
// sources, the run logs and the control socket. raw is the config's unexpanded
// data_dir (may be empty), configDir the directory holding dicode.yaml, and
// home the user's home directory ("" when it cannot be determined).
//
// Every process that has to name that directory resolves it here: the daemon
// through applyDefaults, the CLI through cliDataDirFor, and onboarding when it
// writes the first config. They must agree, because the CLI dials whatever
// socket sits at the path it computes and sends the subcommand's request over
// it — a path the daemon is not listening on is at best a dead command, and at
// worst gets a second daemon started against a directory that already has
// one.
//
// DICODE_DATA_DIR outranks the config. The config is discovered from wherever
// the process happens to be standing; the env var is someone stating which
// installation they mean. It is also what makes the Docker image's
// `ENV DICODE_DATA_DIR=/data` redirect state into the mounted volume, both
// before onboarding has written a config and after it has baked a path in.
//
// An unrecognized ${VAR} is left standing rather than emptied: ${DATADIR} is
// not yet bound while data_dir is itself being expanded, and substituting or
// rejecting one here would point the callers at different directories.
func ResolveDataDir(raw, configDir, home string) string {
	if d := os.Getenv(DataDirEnvVar); d != "" {
		return d
	}
	if raw != "" {
		return expandDataDirVars(raw, home, configDir)
	}
	if home == "" {
		return ""
	}
	return home + "/" + DefaultDataDirName
}

// expandDataDirVars applies the expansion applyDefaults performs on every path
// field, restricted to the variables bound at the point data_dir is resolved.
func expandDataDirVars(value, home, configDir string) string {
	if strings.HasPrefix(value, "~/") && home != "" {
		value = home + value[1:]
	}
	return expandVars(value, map[string]string{"HOME": home, "CONFIGDIR": configDir})
}
