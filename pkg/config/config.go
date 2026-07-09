package config

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/dicode/dicode/internal/gitops"
	"github.com/dicode/dicode/pkg/runtime/containersec"
	"github.com/dicode/dicode/pkg/task"
	"github.com/dicode/dicode/pkg/taskset"
	"gopkg.in/yaml.v3"
)

// ErrLegacySourcesFormat is returned by Load when the config file contains the
// legacy `sources:` array. Operators must migrate to the new `spec.entries`
// shape — see https://github.com/dicode-ayo/dicode-core/issues/261 and
// docs/concepts/sources.md for a side-by-side migration guide.
var ErrLegacySourcesFormat = errors.New(
	"dicode.yaml: legacy `sources` array detected. The format changed: declare\n" +
		"sources as `spec.entries` instead. See\n" +
		"https://github.com/dicode-ayo/dicode-core/issues/261\n" +
		"or docs/concepts/sources.md for the migration. The translation is\n" +
		"mechanical — each `sources[]` entry becomes a `spec.entries.<name>`\n" +
		"with the source's fields nested under `ref`.",
)

// ErrLegacyNotificationsBlock is returned by Load when the config file
// contains the removed `notifications:` block. The daemon-side notification
// subsystem (#279) was deleted; notifications are now delivered by tasks
// via `defaults.on_failure_chain`. Failing fast here prevents the silent
// "alerts go to /dev/null" footgun where YAML's tolerant decode would
// otherwise drop the block without comment.
var ErrLegacyNotificationsBlock = errors.New(
	"dicode.yaml: legacy `notifications` block detected. The daemon-side\n" +
		"notification subsystem was removed (#279). Notifications are now\n" +
		"delivered by tasks: point `defaults.on_failure_chain` at\n" +
		"`buildin/alert`, `buildin/notifications`, or any task you write\n" +
		"yourself for ntfy / Slack / Discord / email / etc. Remove the\n" +
		"`notifications:` block from your config to continue.",
)

// RuntimeConfig configures a managed runtime executor.
// The Version field pins the interpreter version that dicode downloads;
// leave it empty to use the default version bundled with this release.
// Set Disabled: true to prevent the runtime from being registered at startup.
type RuntimeConfig struct {
	// Version pins the interpreter version (e.g. "2.3.3" for Deno,
	// "0.7.3" for uv/Python). If empty the runtime's built-in default is used.
	Version string `yaml:"version,omitempty"`
	// Disabled prevents this runtime from being registered at startup.
	Disabled bool `yaml:"disabled,omitempty"`
}

// DefaultsConfig holds task-level defaults that apply globally unless overridden per-task.
type DefaultsConfig struct {
	// OnFailureChain is the chain target to fire whenever any task fails.
	// Accepts a bare task ID or a structured `{task, params}` form.
	// Per-task on_failure_chain field can override or disable this.
	OnFailureChain task.OnFailureChainSpec `yaml:"on_failure_chain,omitempty"`

	// RunInputs configures run-input persistence. See spec § 4.1.
	RunInputs RunInputsConfig `yaml:"run_inputs,omitempty"`
}

// RunInputsConfig is the global default for run-input persistence (#233).
type RunInputsConfig struct {
	// Enabled toggles persistence globally. Default (nil → true) applied in
	// applyDefaults. Set to false to disable for the entire daemon.
	Enabled *bool `yaml:"enabled,omitempty"`

	// Retention is the maximum age of a persisted input. Older blobs are
	// swept by the run-inputs-cleanup buildin. Default 30d applied in
	// applyDefaults.
	Retention time.Duration `yaml:"retention,omitempty"`

	// StorageTask is the task ID of the configured storage backend.
	// Default "buildin/local-storage" applied in applyDefaults.
	StorageTask string `yaml:"storage_task,omitempty"`

	// BodyFullTextual opts in to persisting non-JSON textual bodies
	// verbatim (XML, plain-text). Footgun: name-based redaction can't
	// reach unstructured content; persisted bodies may contain credentials.
	// Default false.
	BodyFullTextual bool `yaml:"body_full_textual,omitempty"`
}

// IsEnabled returns the effective Enabled value (nil → true).
func (c RunInputsConfig) IsEnabled() bool {
	if c.Enabled == nil {
		return true
	}
	return *c.Enabled
}

// ExecutionConfig tunes how task runs are dispatched by the trigger engine.
type ExecutionConfig struct {
	// MaxConcurrentTasks caps how many task goroutines run in parallel.
	// 0 (default) = unlimited. Extra invocations queue inside the daemon
	// and run as slots free. Daemon-trigger tasks bypass the limit so
	// long-runners can't starve webhook/cron tasks.
	// The DICODE_MAX_CONCURRENT_TASKS env var overrides this value when set.
	MaxConcurrentTasks int `yaml:"max_concurrent_tasks,omitempty"`
}

// RelayConfig configures the WebSocket relay client.
// The relay allows a local dicode instance to receive webhooks from external
// services (GitHub, Slack, etc.) without port forwarding.
type RelayConfig struct {
	Enabled   bool   `yaml:"enabled"`
	ServerURL string `yaml:"server_url"` // wss://relay.dicode.app
	// BrokerURL overrides the OAuth broker base URL. When empty, the daemon
	// derives it from ServerURL by swapping the scheme (wss://host →
	// https://host). Set this when the broker runs on a different host than
	// the WSS relay endpoint, or during local development to point at a
	// broker on a non-TLS port (e.g. http://localhost:5553). Must be http://
	// or https:// when set.
	BrokerURL string `yaml:"broker_url,omitempty"`
}

// ResolvedBrokerURL returns the HTTP(S) OAuth broker base URL that the
// daemon should use for signing /auth URLs and receiving ECIES-encrypted
// token deliveries. If BrokerURL is set it wins; otherwise the URL is
// derived from ServerURL. Returns empty when neither yields a usable URL —
// the daemon treats that as "OAuth broker disabled".
//
// The returned URL never has a trailing slash so callers can safely
// concatenate `+ "/auth/" + provider` without producing a "//" double-
// slash. Operators writing broker_url: https://host/ in dicode.yaml get
// the slash stripped here.
func (r RelayConfig) ResolvedBrokerURL() string {
	var raw string
	switch {
	case r.BrokerURL != "":
		raw = r.BrokerURL
	case strings.HasPrefix(r.ServerURL, "wss://"):
		raw = "https://" + strings.TrimPrefix(r.ServerURL, "wss://")
	case strings.HasPrefix(r.ServerURL, "ws://"):
		raw = "http://" + strings.TrimPrefix(r.ServerURL, "ws://")
	default:
		return ""
	}
	return strings.TrimRight(raw, "/")
}

// AIConfig points the WebUI and CLI at a single task for AI operations.
// The task must have a webhook trigger — /api/ai/chat forwards requests to it,
// and `dicode ai` fires it through the engine.
type AIConfig struct {
	// Task is the task id invoked for AI operations in the WebUI and CLI.
	// Defaults to "buildin/dicodai" — a preset of buildin/ai-agent preloaded
	// with the dicode-task-dev skill. Point it at any ai-agent override to
	// swap providers, skills, or model without changing code.
	Task string `yaml:"task,omitempty"`

	// CreateTask is the task id invoked for AI-first task authoring sessions
	// (`dicode task create --ai`, `dicode task edit`, and the dc-task-create
	// webui component). Defaults to "buildin/task-create" — an ai-agent
	// override preloaded with the dicode-task-create skill and granted the
	// sandbox FS + sources_set_dev_mode permissions it needs to scaffold
	// task files inside a dev-mode clone. Distinct from Task so the
	// read-only chat surface and the writeable authoring agent can be
	// swapped independently. Empty-string and absent both resolve to the
	// default — no explicit-disable state today (matches AI.Task).
	CreateTask string `yaml:"create_task,omitempty"`

	// CreateSessionTTL bounds how long an open authoring session may sit
	// idle before the daemon force-cancels it (releasing the dev-mode lock
	// on its source and cleaning up the sandbox dir). Defaults to 24h.
	// Empty or zero falls through to the default — no explicit-disable
	// state today (matches DefaultsConfig.RunInputs.Retention). Operators
	// who want effectively-never auto-cancel can set a very large value
	// (e.g. 8760h).
	CreateSessionTTL time.Duration `yaml:"create_session_ttl,omitempty"`
}

// TrustPolicy is the per-source / per-task trust declaration under the
// approval block. The only recognised value is "always" (auto-approve);
// empty means the gate applies normally.
type TrustPolicy struct {
	Trust string `yaml:"trust,omitempty"`
}

// ApprovalConfig is the trust-on-change approval gate policy (#392). The
// daemon never writes this block — approval records live in the daemon-owned
// dicode.lock sibling file; dicode.yaml carries only the human-edited policy.
type ApprovalConfig struct {
	// Enabled toggles the gate. nil → true (gate ON by default). When false,
	// every task arms regardless of approval state, but dicode.lock is still
	// maintained as a running inventory.
	Enabled *bool `yaml:"enabled,omitempty"`

	// NotifyTask is fired (manual trigger) on the transition into pending with
	// string params {task_id, hash, approve_url}; delivery (slack/email/ntfy)
	// is that task's concern. It should be a builtin or trusted task — the
	// notify task is itself subject to the gate, so an untrusted one would sit
	// pending and never fire. Empty → WebUI broadcast + WARN log only.
	NotifyTask string `yaml:"notify_task,omitempty"`

	// Sources maps a source name (the first segment of a namespaced task ID,
	// i.e. a spec.entries key) to its trust policy.
	Sources map[string]TrustPolicy `yaml:"sources,omitempty"`

	// Tasks maps a full task ID to a per-task trust override.
	Tasks map[string]TrustPolicy `yaml:"tasks,omitempty"`
}

// IsEnabled returns the effective Enabled value (nil → true).
func (a ApprovalConfig) IsEnabled() bool {
	if a.Enabled == nil {
		return true
	}
	return *a.Enabled
}

// AuditLogConfig tunes the structured security audit log (#45).
type AuditLogConfig struct {
	// RetentionDays is how long audit_log rows are kept before the daemon
	// prunes them. nil (unset) defaults to 30 days; an explicit 0 disables
	// pruning entirely (rows are kept forever). Negative values are
	// rejected by validate.
	RetentionDays *int `yaml:"retention_days,omitempty"`
}

// defaultAuditRetentionDays is applied when audit_log.retention_days is unset.
const defaultAuditRetentionDays = 30

// EffectiveRetentionDays resolves the retention window: nil → 30 (default),
// explicit 0 → 0 (pruning disabled).
func (c AuditLogConfig) EffectiveRetentionDays() int {
	if c.RetentionDays == nil {
		return defaultAuditRetentionDays
	}
	return *c.RetentionDays
}

type Config struct {
	// Spec is the root TaskSet defined inline in dicode.yaml. Its entries
	// declare every source the daemon loads. Overrides on these entries
	// propagate down to the referenced TaskSet using the standard
	// parent.overrides.entries mechanism — see pkg/taskset/resolver.go.
	//
	// Example:
	//   spec:
	//     entries:
	//       buildin:
	//         ref:
	//           path: ${CONFIGDIR}/tasks/buildin/taskset.yaml
	//       examples:
	//         ref:
	//           url: https://github.com/org/examples
	//           branch: main
	//           poll_interval: 5m
	//           auth: { token_env: GITHUB_TOKEN }
	Spec      taskset.TaskSetBody      `yaml:"spec"`
	Database  DatabaseConfig           `yaml:"database"`
	Secrets   SecretsConfig            `yaml:"secrets"`
	Server    ServerConfig             `yaml:"server"`
	Defaults  DefaultsConfig           `yaml:"defaults"`
	Runtimes  map[string]RuntimeConfig `yaml:"runtimes,omitempty"`
	Execution ExecutionConfig          `yaml:"execution,omitempty"`
	Relay     RelayConfig              `yaml:"relay,omitempty"`
	AI        AIConfig                 `yaml:"ai,omitempty"`
	Approval  ApprovalConfig           `yaml:"approval,omitempty"`
	AuditLog  AuditLogConfig           `yaml:"audit_log,omitempty"`
	LogLevel  string                   `yaml:"log_level"`
	DataDir   string                   `yaml:"data_dir"`
	// ContainerSecurity is the operator opt-out for the container runtime
	// security floor (issue #380). By default every dangerous host-config
	// request from a task.yaml (host network namespace, dangerous cap_add,
	// isolation-weakening security_opt, bind mounts of sensitive host paths)
	// is rejected at dispatch; this block lets an operator explicitly allow
	// specific escapes. Appended last to minimize merge conflicts.
	ContainerSecurity ContainerSecurityConfig `yaml:"container_security,omitempty"`

	// SourceSecurity is the operator opt-out for the git-remote SSRF guard
	// (issue #537). By default a source pointed at a loopback/private/link-
	// local/internal host is refused under every scheme; this block names the
	// specific internal hosts/CIDRs an operator trusts. Keyed off
	// `source_security` rather than `sources` because the latter is reserved
	// for the removed legacy source array and rejected outright by Load.
	SourceSecurity SourceSecurityConfig `yaml:"source_security,omitempty"`
}

// SourceSecurityConfig configures the operator opt-out for dicode's git-remote
// SSRF guard (issue #537). The zero value — no block in dicode.yaml — keeps the
// guard fully fail-closed: no internal host is reachable.
//
// Example:
//
//	source_security:
//	  allow_internal_hosts:
//	    - git.corp.internal   # authorises ssh/SCP-shorthand to this host
//	    - 10.0.0.0/8          # also required for http/https to resolved 10.x IPs
//
// A hostname entry authorises the literal-host check that ssh and SCP-shorthand
// remotes get. http/https remotes are additionally re-checked at dial time
// against the *resolved* IP, so those also need the target's IP or CIDR listed
// — a hostname alone never authorises an address it resolves to (this is what
// keeps the DNS-rebind protection intact for an allowlisted name).
type SourceSecurityConfig struct {
	AllowInternalHosts []string `yaml:"allow_internal_hosts,omitempty"`
}

// Allowlist parses the configured entries into the guard's runtime allowlist.
func (c SourceSecurityConfig) Allowlist() (*gitops.Allowlist, error) {
	return gitops.ParseAllowlist(c.AllowInternalHosts)
}

// ContainerSecurityConfig configures the security floor enforced on
// docker/podman task host configuration (issue #380). The zero value —
// no block in dicode.yaml — denies everything dangerous.
//
// Example:
//
//	container_security:
//	  allow_host_network: true                 # permit network_mode: host / container:<id>
//	  allow_insecure_security_opt: false       # seccomp/apparmor/label/unmask weakening
//	  allowed_cap_add: [SYS_PTRACE]            # caps tasks may add despite the deny list ("ALL" = any)
//	  allowed_volume_roots: [/srv/dicode-data] # strict allowlist for bind-mount sources
//
// When allowed_volume_roots is non-empty, EVERY bind-mount source must
// resolve (after symlink/".." cleaning) inside one of the listed roots;
// an explicitly listed root also overrides the built-in sensitive-path
// denylist (/, /proc, /sys, /etc, /dev, /run, the docker/podman sockets, …).
type ContainerSecurityConfig struct {
	AllowHostNetwork         bool     `yaml:"allow_host_network,omitempty"`
	AllowInsecureSecurityOpt bool     `yaml:"allow_insecure_security_opt,omitempty"`
	AllowedCapAdd            []string `yaml:"allowed_cap_add,omitempty"`
	AllowedVolumeRoots       []string `yaml:"allowed_volume_roots,omitempty"`
}

// Policy converts the operator config into the runtime-enforced policy.
func (c ContainerSecurityConfig) Policy() containersec.Policy {
	return containersec.Policy{
		AllowHostNetwork:         c.AllowHostNetwork,
		AllowInsecureSecurityOpt: c.AllowInsecureSecurityOpt,
		AllowedCapAdd:            c.AllowedCapAdd,
		AllowedVolumeRoots:       c.AllowedVolumeRoots,
	}
}

// DatabaseConfig selects the storage backend.
// SQLite is the default (free). Postgres/MySQL are for paid/enterprise use.
type DatabaseConfig struct {
	Type   string `yaml:"type"`    // "sqlite" (default) | "postgres" | "mysql"
	Path   string `yaml:"path"`    // sqlite: path to .db file
	URLEnv string `yaml:"url_env"` // postgres/mysql: env var holding DSN
}

type SecretsConfig struct {
	Providers []SecretProviderConfig `yaml:"providers"`
}

type SecretProviderConfig struct {
	Type     string `yaml:"type"`      // "local" | "env" | "vault" | ...
	Address  string `yaml:"address"`   // vault address
	TokenEnv string `yaml:"token_env"` // env var holding token
}

type ServerConfig struct {
	Port           int      `yaml:"port"`
	Secret         string   `yaml:"secret" json:"-"`           // optional passphrase; excluded from JSON API
	Auth           bool     `yaml:"auth"`                      // enable global auth wall (default: false)
	AllowedOrigins []string `yaml:"allowed_origins,omitempty"` // CORS allowlist; empty = same-origin only
	TrustProxy     bool     `yaml:"trust_proxy,omitempty"`     // trust X-Forwarded-For from upstream proxy
	MCP            *bool    `yaml:"mcp,omitempty"`             // expose MCP endpoint at /mcp; nil → default true, explicit false opts out
	TLSCertFile    string   `yaml:"tls_cert,omitempty"`        // path to TLS certificate (PEM); enables HTTPS when set with tls_key
	TLSKeyFile     string   `yaml:"tls_key,omitempty"`         // path to TLS private key (PEM)
	// BcryptCost is the work factor used when hashing the stored auth
	// passphrase. Valid range 4–14. 0 means "unset" → defaults to 12 in
	// applyDefaults. Higher = slower login but stronger against offline attacks
	// if the SQLite DB ever leaks; lower can be useful on very small devices
	// (e.g. Raspberry Pi Zero) where the default ~300ms login is too slow.
	// Values outside 4–14 are rejected by validate.
	BcryptCost int `yaml:"bcrypt_cost,omitempty"`
	// DeviceBinding controls how strictly a trusted-device cookie is bound to
	// the IP subnet and User-Agent family it was issued for. One of:
	//   "off"    — record IP/UA but never verify them (default, backward compatible)
	//   "warn"   — allow renewal on drift, but flag it on /security
	//   "strict" — reject renewal when the IP subnet or UA family drifts
	// Empty resolves to "off" in applyDefaults; validate rejects other values.
	DeviceBinding string `yaml:"device_binding,omitempty"`
}

// Load reads and parses the config file at path, then applies defaults.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("open config: %w", err)
	}

	// Probe for removed top-level keys before decoding into Config. Fail
	// fast with a clear migration error instead of silently dropping the
	// block — yaml.v3 does not enforce KnownFields by default, so an
	// unrecognised key would otherwise vanish without comment.
	var probe struct {
		Sources       any `yaml:"sources"`
		Notifications any `yaml:"notifications"`
	}
	if err := yaml.Unmarshal(data, &probe); err == nil {
		if probe.Sources != nil {
			return nil, ErrLegacySourcesFormat
		}
		if probe.Notifications != nil {
			return nil, ErrLegacyNotificationsBlock
		}
	}

	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}

	configDir, _ := filepath.Abs(filepath.Dir(path))
	applyDefaults(&cfg, configDir)

	if err := cfg.validate(); err != nil {
		return nil, fmt.Errorf("invalid config: %w", err)
	}

	return &cfg, nil
}

// expandHome replaces a leading ~/ with the actual home directory.
func expandHome(path string) string {
	if len(path) >= 2 && path[:2] == "~/" {
		home, err := os.UserHomeDir()
		if err == nil {
			return home + path[1:]
		}
	}
	return path
}

// expandVars replaces ${VAR} placeholders in path strings.
// Supported variables:
//   - ${HOME}      — user home directory
//   - ${CONFIGDIR} — directory containing dicode.yaml
//   - ${DATADIR}   — resolved data_dir value
func expandVars(path string, vars map[string]string) string {
	for k, v := range vars {
		path = strings.ReplaceAll(path, "${"+k+"}", v)
	}
	return path
}

func applyDefaults(cfg *Config, configDir string) {
	// Build template variables for path expansion.
	home, _ := os.UserHomeDir()
	vars := map[string]string{
		"HOME":      home,
		"CONFIGDIR": configDir,
	}

	// Expand ~ and ${VAR} in all path fields before anything else.
	expand := func(path string) string {
		return expandVars(expandHome(path), vars)
	}
	cfg.DataDir = expand(cfg.DataDir)

	// DataDir must be resolved first so ${DATADIR} is available for other paths.
	if cfg.DataDir == "" {
		cfg.DataDir = home + "/.dicode"
	}
	vars["DATADIR"] = cfg.DataDir

	cfg.Database.Path = expand(cfg.Database.Path)

	// Synthesise the virtual "ai-scratch" local source if the operator
	// hasn't already defined one AND the ai-tasks directory already exists.
	// The directory is created lazily by the first AI authoring session
	// (`dicode task create --ai`); synthesising a source for a non-existent
	// path causes the taskset resolver to log spurious WARN on every
	// fsnotify tick and can race with other sources that share the same
	// watchRoot (both resolve to filepath.Dir of their ref path).
	//
	// Synthesis runs before the entry-expansion loop below so the
	// synthesised ref goes through the same path-expansion + watch-default
	// + IsGit-branch normalisation every other entry gets.
	if cfg.Spec.Entries == nil {
		cfg.Spec.Entries = map[string]*taskset.Entry{}
	}
	aiScratchPath := filepath.Join(cfg.DataDir, "ai-tasks")
	if _, exists := cfg.Spec.Entries["ai-scratch"]; !exists {
		if fi, err := os.Stat(aiScratchPath); err == nil && fi.IsDir() {
			cfg.Spec.Entries["ai-scratch"] = &taskset.Entry{
				Ref: &taskset.Ref{Path: aiScratchPath},
			}
		}
	}

	// Expand ${VAR} in ref paths for spec.entries.
	for _, entry := range cfg.Spec.Entries {
		if entry == nil || entry.Ref == nil {
			continue
		}
		entry.Ref.Path = expand(entry.Ref.Path)
		// Apply defaults for git refs.
		if entry.Ref.IsGit() {
			if entry.Ref.Branch == "" {
				entry.Ref.Branch = "main"
			}
			if entry.Ref.PollInterval == 0 {
				entry.Ref.PollInterval = 30 * time.Second
			}
		}
		// Watch defaults to true for local refs.
		if !entry.Ref.IsGit() && entry.Ref.Watch == nil {
			t := true
			entry.Ref.Watch = &t
		}
	}
	if cfg.Server.Port == 0 {
		cfg.Server.Port = 8080
	}
	// MCP defaults to enabled. Pointer lets us distinguish unset (nil → true)
	// from explicit false (opt-out).
	if cfg.Server.MCP == nil {
		t := true
		cfg.Server.MCP = &t
	}
	// BcryptCost defaults to 12 — ~300ms per hash on a 2024 server CPU.
	// Operators can override via server.bcrypt_cost in dicode.yaml; validate()
	// enforces the 4–14 range. We keep the unset → default mapping in
	// applyDefaults rather than at the call site so every consumer sees the
	// resolved value.
	if cfg.Server.BcryptCost == 0 {
		cfg.Server.BcryptCost = 12
	}
	// Device binding defaults to off so existing deployments keep their
	// current trusted-device behaviour until an operator opts in.
	if cfg.Server.DeviceBinding == "" {
		cfg.Server.DeviceBinding = "off"
	}
	// Default secret providers if none configured
	if len(cfg.Secrets.Providers) == 0 {
		cfg.Secrets.Providers = []SecretProviderConfig{
			{Type: "local"},
			{Type: "env"},
		}
	}
	// Default database to sqlite
	if cfg.Database.Type == "" {
		cfg.Database.Type = "sqlite"
	}
	if cfg.Database.Type == "sqlite" && cfg.Database.Path == "" {
		cfg.Database.Path = cfg.DataDir + "/data.db"
	}
	if cfg.LogLevel == "" {
		cfg.LogLevel = "info"
	}
	// AI.Task defaults to the buildin/dicodai preset so out-of-the-box
	// WebUI + CLI AI flows work without configuration. Empty-string and
	// absent both resolve to the default — there is no "explicitly
	// disabled" state today. If a future version needs one, switch to
	// *string and test for nil instead of empty.
	if cfg.AI.Task == "" {
		cfg.AI.Task = "buildin/dicodai"
	}
	// AI.CreateTask defaults to the buildin/task-create preset so AI-first
	// authoring (`dicode task create --ai`, `dicode task edit`, the
	// dc-task-create webui component) works out of the box. Same
	// empty-or-default semantics as AI.Task — keep them in lockstep so
	// operators reason about one rule.
	if cfg.AI.CreateTask == "" {
		cfg.AI.CreateTask = "buildin/task-create"
	}
	// AI.CreateSessionTTL caps idle authoring-session lifetime so a stale
	// open session can't hold a source's dev-mode lock indefinitely. 24h
	// is long enough for "I'll come back to it tomorrow" and short enough
	// to free shared boxes overnight.
	if cfg.AI.CreateSessionTTL == 0 {
		cfg.AI.CreateSessionTTL = 24 * time.Hour
	}
	// RunInputs defaults: 30-day retention, local-storage backend.
	if cfg.Defaults.RunInputs.Retention == 0 {
		cfg.Defaults.RunInputs.Retention = 30 * 24 * time.Hour
	}
	if cfg.Defaults.RunInputs.StorageTask == "" {
		cfg.Defaults.RunInputs.StorageTask = "buildin/local-storage"
	}
	// DataDir default is set earlier during variable expansion.
}

func (cfg *Config) validate() error {
	for name, entry := range cfg.Spec.Entries {
		if entry == nil {
			return fmt.Errorf("spec.entries[%q]: entry must not be null", name)
		}
		if entry.Ref == nil && entry.Inline == nil {
			return fmt.Errorf("spec.entries[%q]: either ref or inline must be set", name)
		}
		if entry.Ref != nil {
			if entry.Ref.IsGit() {
				// URL is present (checked by IsGit); validate its scheme.
				if err := taskset.ValidateRefURL("dicode.yaml", name, entry.Ref.URL); err != nil {
					return err
				}
			} else if entry.Ref.Path == "" {
				return fmt.Errorf("spec.entries[%q]: ref.path is required for local entries", name)
			}
		}
	}
	// Lift the top-level `enabled` shortcut into overrides.enabled so that all
	// downstream code (resolver, override merging) sees one canonical path.
	// The same lift is applied by validateTaskSet for TaskSet files — both call
	// the shared helper to stay DRY.
	if err := taskset.LiftEntryEnabled(cfg.Spec.Entries); err != nil {
		return fmt.Errorf("spec.entries: %w", err)
	}
	if cfg.Relay.BrokerURL != "" {
		u, err := url.Parse(cfg.Relay.BrokerURL)
		if err != nil {
			return fmt.Errorf("relay.broker_url: %w", err)
		}
		if u.Scheme != "http" && u.Scheme != "https" {
			return fmt.Errorf("relay.broker_url: must use http:// or https://, got scheme %q", u.Scheme)
		}
		if u.Host == "" {
			return fmt.Errorf("relay.broker_url: missing host in %q", cfg.Relay.BrokerURL)
		}
	}
	// bcrypt cost: x/crypto/bcrypt's MinCost = 4, MaxCost = 31, but anything
	// above ~14 is multi-second per login on commodity hardware and serves no
	// practical purpose for a single-user passphrase. Cap at 14 to prevent
	// operators from accidentally locking themselves out (or causing a
	// mid-attempt timeout) by setting 20 "to be safe".
	if cfg.Server.BcryptCost != 0 && (cfg.Server.BcryptCost < 4 || cfg.Server.BcryptCost > 14) {
		return fmt.Errorf("server.bcrypt_cost: must be between 4 and 14, got %d", cfg.Server.BcryptCost)
	}
	switch cfg.Server.DeviceBinding {
	case "", "off", "warn", "strict":
	default:
		return fmt.Errorf("server.device_binding: must be one of off, warn, strict, got %q", cfg.Server.DeviceBinding)
	}
	if cfg.AuditLog.RetentionDays != nil && *cfg.AuditLog.RetentionDays < 0 {
		return fmt.Errorf("audit_log.retention_days: must be >= 0 (0 disables pruning), got %d", *cfg.AuditLog.RetentionDays)
	}
	if err := cfg.Defaults.OnFailureChain.ValidateAtDefaults(); err != nil {
		return fmt.Errorf("defaults.on_failure_chain: %w", err)
	}
	for name, p := range cfg.Approval.Sources {
		if p.Trust != "" && p.Trust != "always" {
			return fmt.Errorf("approval.sources[%q].trust: must be \"always\" or unset, got %q", name, p.Trust)
		}
	}
	for id, p := range cfg.Approval.Tasks {
		if p.Trust != "" && p.Trust != "always" {
			return fmt.Errorf("approval.tasks[%q].trust: must be \"always\" or unset, got %q", id, p.Trust)
		}
	}
	// container_security (issue #380): roots must be absolute so the
	// runtime's resolve-then-prefix check is well-defined; caps must be
	// non-empty names.
	for _, root := range cfg.ContainerSecurity.AllowedVolumeRoots {
		if !filepath.IsAbs(root) {
			return fmt.Errorf("container_security.allowed_volume_roots: %q must be an absolute path", root)
		}
	}
	for _, c := range cfg.ContainerSecurity.AllowedCapAdd {
		if strings.TrimSpace(c) == "" {
			return fmt.Errorf("container_security.allowed_cap_add: entries must be non-empty capability names")
		}
	}
	// source_security (issue #537): reject malformed allowlist entries at load
	// so an operator learns of a typo'd CIDR/host at startup, not on the first
	// blocked clone.
	if _, err := cfg.SourceSecurity.Allowlist(); err != nil {
		return err
	}
	return nil
}
