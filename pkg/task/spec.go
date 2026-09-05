package task

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Runtime identifies the scripting engine used to execute a task.
type Runtime string

const (
	RuntimeDeno   Runtime = "deno"
	RuntimeDocker Runtime = "docker"
	RuntimePodman Runtime = "podman"
)

// DockerBuild configures a local Dockerfile build instead of pulling a pre-built image.
// The built image is tagged dicode-<taskID>:<hash> and cached; rebuild only happens when
// the Dockerfile content changes. Old dicode-<taskID>:* images (task removed, or
// Dockerfile changed) are reclaimed best-effort by the daemon's periodic image GC —
// see pkg/runtime/imagegc and the ReclaimOrphanedImages functions in the docker and
// podman runtimes.
type DockerBuild struct {
	Dockerfile string `yaml:"dockerfile,omitempty"` // path relative to task dir; default "Dockerfile"
	Context    string `yaml:"context,omitempty"`    // path relative to task dir; default task dir
}

// ResolvePaths returns the absolute Dockerfile path and build context directory
// for this build config, resolving relative paths against taskDir.
func (b *DockerBuild) ResolvePaths(taskDir string) (dockerfilePath, contextDir string) {
	dockerfilePath = b.Dockerfile
	if dockerfilePath == "" {
		dockerfilePath = "Dockerfile"
	}
	if !filepath.IsAbs(dockerfilePath) {
		dockerfilePath = filepath.Join(taskDir, dockerfilePath)
	}
	contextDir = taskDir
	if b.Context != "" {
		if filepath.IsAbs(b.Context) {
			contextDir = b.Context
		} else {
			contextDir = filepath.Join(taskDir, b.Context)
		}
	}
	return
}

// DockerConfig holds Docker/Podman-specific task configuration.
type DockerConfig struct {
	Image      string            `yaml:"image,omitempty"`       // e.g. "nginx:alpine"
	Build      *DockerBuild      `yaml:"build,omitempty"`       // build from local Dockerfile instead of pulling
	Command    []string          `yaml:"command,omitempty"`     // overrides image CMD
	Entrypoint []string          `yaml:"entrypoint,omitempty"`  // overrides image ENTRYPOINT
	Volumes    []string          `yaml:"volumes,omitempty"`     // "host:container[:ro]"
	Ports      []string          `yaml:"ports,omitempty"`       // "hostPort:containerPort[/proto]"
	WorkingDir string            `yaml:"working_dir,omitempty"` // container working dir
	EnvVars    map[string]string `yaml:"env_vars,omitempty"`    // extra env vars (literal)
	PullPolicy string            `yaml:"pull_policy,omitempty"` // "always" | "missing" (default) | "never"

	// Network / isolation
	NetworkMode string   `yaml:"network_mode,omitempty"` // "host" | "bridge" | "none" | "<custom-network>"
	ExtraHosts  []string `yaml:"extra_hosts,omitempty"`  // "<name>:<ip-or-host-gateway>"

	// Hardening
	CapDrop     []string `yaml:"cap_drop,omitempty"`     // e.g. ["ALL"]
	CapAdd      []string `yaml:"cap_add,omitempty"`      // e.g. ["NET_BIND_SERVICE"] — re-add after CapDrop
	SecurityOpt []string `yaml:"security_opt,omitempty"` // e.g. ["no-new-privileges:true"]
	ReadOnly    bool     `yaml:"read_only,omitempty"`    // mount container rootfs read-only
	User        string   `yaml:"user,omitempty"`         // "<uid>[:<gid>]" or "<name>[:<group>]"
}

// ChainTrigger fires a task when another task completes.
//
// Params are user-supplied values forwarded into the downstream task's input
// map alongside engine-reserved keys (taskID, runID, status, output,
// _chain_depth). When Params is empty, the engine passes the upstream task's
// raw output through as input — preserving the historical contract where
// downstream tasks consume `input` as the upstream's return value directly.
// When Params is non-empty, input becomes a map[string]any merging user
// params with the reserved engine keys; reserved keys are validated to not
// appear in Params at config-load (see Spec.validate), so collisions are
// impossible at firing time.
//
// Overrides, when set, are a per-firing patch applied to the downstream's
// own spec right before dispatch via this chain edge. Semantically the
// downstream is declaring the variant of itself it wants to run when fired
// via this chain — manual fires of the same downstream are unaffected.
// The merge reuses the same taskset.ApplyOverrides logic that powers the
// global `dicode tasks override <id>` path. The override is held as a
// *Overrides pointer so the (already large) override surface stays in one
// place; a nil pointer means "no per-edge override" (the existing
// behaviour).
//
// Mirror of OnFailureChainSpec.Params (failure-chain side); shares the same
// reservedChainParamKeys set.
type ChainTrigger struct {
	From      string         `yaml:"from"`                // task ID to listen for
	On        string         `yaml:"on,omitempty"`        // "success" (default) | "failure" | "always"
	Params    map[string]any `yaml:"params,omitempty"`    // forwarded into downstream task input
	Overrides *Overrides     `yaml:"overrides,omitempty"` // per-firing override applied to the downstream spec
}

// WebhookAuthMode is the auth posture of a webhook trigger. In YAML it accepts a
// bool (true = session, false/absent = public) or a string ("session", "any").
//   - session: a valid dicode session is required for GET (UI) and POST (run);
//     a webhook_secret, if also set, still ANDs on top.
//   - any: session OR a valid HMAC signature. HMAC is the only auth that
//     traverses the relay, so this is the machine-caller-over-relay path.
type WebhookAuthMode string

const (
	WebhookAuthNone    WebhookAuthMode = ""
	WebhookAuthSession WebhookAuthMode = "session"
	WebhookAuthAny     WebhookAuthMode = "any"
)

// Enabled reports whether the webhook is auth-gated at all.
func (m WebhookAuthMode) Enabled() bool { return m != WebhookAuthNone }

// RequiresSession reports whether a session is the sole accepted credential
// (session mode). Distinguished from "any", where HMAC is an alternative.
func (m WebhookAuthMode) RequiresSession() bool { return m == WebhookAuthSession }

// MarshalJSON preserves the pre-tri-value wire format so approval content
// hashes stay stable across the bool→WebhookAuthMode change: none→false,
// session→true (byte-identical to the old bool encoding, so an existing
// auth: true task does not spuriously re-pend), any→"any" (distinct, so
// switching a task to "any" re-pends it — it opens a relay-reachable HMAC path).
func (m WebhookAuthMode) MarshalJSON() ([]byte, error) {
	switch m {
	case WebhookAuthAny:
		return []byte(`"any"`), nil
	case WebhookAuthSession:
		return []byte(`true`), nil
	default:
		return []byte(`false`), nil
	}
}

// UnmarshalYAML accepts a bool (back-compat: true→session, false→none) or a
// string ("session"/"any"). Any other scalar or node kind is an error.
func (m *WebhookAuthMode) UnmarshalYAML(value *yaml.Node) error {
	if value.Kind != yaml.ScalarNode {
		return fmt.Errorf("trigger.auth must be a bool or string, got %s", value.Tag)
	}
	switch value.Tag {
	case "!!bool":
		var b bool
		if err := value.Decode(&b); err != nil {
			return err
		}
		if b {
			*m = WebhookAuthSession
		} else {
			*m = WebhookAuthNone
		}
		return nil
	case "!!str":
		switch WebhookAuthMode(value.Value) {
		case WebhookAuthNone, WebhookAuthSession, WebhookAuthAny:
			*m = WebhookAuthMode(value.Value)
			return nil
		}
		return fmt.Errorf("invalid trigger.auth %q (want true, false, \"session\", or \"any\")", value.Value)
	default:
		return fmt.Errorf("trigger.auth must be a bool or string, got %s", value.Tag)
	}
}

// WebhookSecretResolved reports whether s is a usable HMAC secret rather than an
// empty string or an unresolved ${VAR} placeholder (the referenced env var was
// not set at load time). The ${ check is a heuristic — a real secret is opaque
// random bytes and won't contain "${" — and it fails safe: a false "unresolved"
// only downgrades auth: any to session.
func WebhookSecretResolved(s string) bool {
	return s != "" && !strings.Contains(s, "${")
}

// normalizeWebhookAuthFields downgrades an auth: any webhook that has no resolved
// webhook_secret to plain session auth. auth: any authenticates a machine caller
// by HMAC over the untrusted relay, so it MUST verify against a real secret;
// serving it against an unresolved ${VAR} placeholder would let anyone who can
// read the (often committed) task.yaml sign a valid request. Downgrading to
// session — and clearing the placeholder so session mode doesn't demand an
// impossible browser signature — keeps the webhook working (browser/session
// auth) with the relay HMAC path simply not offered. Must run after template
// expansion, the only point where a real secret is distinguishable from a
// placeholder. Shared by kind: Task and kind: PipelineTask.
func normalizeWebhookAuthFields(mode *WebhookAuthMode, secret *string, replayProtection, requireTimestamp **bool, warnings *[]string) {
	if *mode != WebhookAuthAny || WebhookSecretResolved(*secret) {
		return
	}
	*mode = WebhookAuthSession
	*secret = ""
	// replay_protection / require_timestamp only act on the HMAC path, which
	// needs a secret — so once we've cleared it they are dead options. Clear
	// them too, or a re-validation pass (the taskset resolver re-runs Validate,
	// which rebuilds Warnings from scratch) would drop the downgrade note and
	// surface a misleading "webhook_secret is empty — the webhook is
	// unauthenticated" warning on a webhook that is in fact session-gated.
	*replayProtection = nil
	*requireTimestamp = nil
	*warnings = append(*warnings,
		`trigger.auth: "any" needs a resolved webhook_secret but none is set — serving session auth only (the HMAC/relay path is disabled)`)
}

// normalizeWebhookAuth applies normalizeWebhookAuthFields to a kind: Task spec.
func normalizeWebhookAuth(s *Spec) {
	normalizeWebhookAuthFields(&s.Trigger.WebhookAuth, &s.Trigger.WebhookSecret,
		&s.Trigger.ReplayProtection, &s.Trigger.RequireTimestamp, &s.Warnings)
}

// TriggerConfig defines how a task is triggered.
// Exactly one of Cron, Webhook, Manual, Chain, or Daemon should be set.
type TriggerConfig struct {
	Cron             string          `yaml:"cron,omitempty"`              // cron expression e.g. "0 9 * * *"
	Webhook          string          `yaml:"webhook,omitempty"`           // HTTP path e.g. "/hooks/my-task"
	WebhookSecret    string          `yaml:"webhook_secret,omitempty"`    // HMAC-SHA256 secret for webhook auth
	WebhookAuth      WebhookAuthMode `yaml:"auth,omitempty"`              // session-required (true/"session") or session-OR-HMAC ("any")
	ReplayProtection *bool           `yaml:"replay_protection,omitempty"` // nonce-cache replay guard; default true when webhook_secret is set
	RequireTimestamp *bool           `yaml:"require_timestamp,omitempty"` // reject requests missing X-Dicode-Timestamp; default false (GitHub-style signers send no timestamp)
	Manual           bool            `yaml:"manual,omitempty"`            // only via explicit trigger
	Chain            *ChainTrigger   `yaml:"chain,omitempty"`             // fire when another task completes
	Daemon           bool            `yaml:"daemon,omitempty"`            // start on app start, restart on exit
	Restart          string          `yaml:"restart,omitempty"`           // daemon only: "always"(default)|"on-failure"|"never"
}

// Param defines a user-configurable input for a task.
type Param struct {
	Name        string `yaml:"name"        json:"name"`
	Type        string `yaml:"type"        json:"type,omitempty"` // "string" | "number" | "boolean" | "cron"
	Default     string `yaml:"default"     json:"default,omitempty"`
	Description string `yaml:"description" json:"description,omitempty"`
	Required    bool   `yaml:"required"    json:"required,omitempty"`
}

// Params is a list of Param values that can be written in two equivalent YAML forms:
//
//	# concise map (name → default or full spec):
//	params:
//	  repo: "deno/deno"
//	  limit:
//	    description: Max results
//	    default: "10"
//	    type: number
//
//	# explicit list:
//	params:
//	  - name: repo
//	    default: "deno/deno"
//	    description: GitHub repo in owner/name format
type Params []Param

func (p *Params) UnmarshalYAML(value *yaml.Node) error {
	switch value.Kind {
	case yaml.MappingNode:
		if len(value.Content)%2 != 0 {
			return fmt.Errorf("params mapping has odd number of nodes")
		}
		*p = make(Params, 0, len(value.Content)/2)
		for i := 0; i < len(value.Content); i += 2 {
			name := value.Content[i].Value
			val := value.Content[i+1]
			param := Param{Name: name}
			if val.Kind == yaml.ScalarNode {
				param.Default = val.Value
			} else {
				type paramBody struct {
					Type        string `yaml:"type"`
					Default     string `yaml:"default"`
					Description string `yaml:"description"`
					Required    bool   `yaml:"required"`
				}
				var body paramBody
				if err := val.Decode(&body); err != nil {
					return fmt.Errorf("param %q: %w", name, err)
				}
				param.Type = body.Type
				param.Default = body.Default
				param.Description = body.Description
				param.Required = body.Required
			}
			*p = append(*p, param)
		}
		return nil
	default:
		return fmt.Errorf("params must be a sequence or mapping, got %v", value.Tag)
	}
}

// FSEntry declares a path a task is allowed to access.
type FSEntry struct {
	Path       string `yaml:"path"`
	Permission string `yaml:"permission"` // "r" | "w" | "rw"
}

// IfMissing declares a prereq task to run when a secret-backed env entry is
// not present in the secrets store at dispatch time. The trigger engine fires
// the prereq synchronously in chain mode before invoking the runtime; if it
// succeeds, env resolution retries. If the prereq also fails (e.g. an OAuth
// flow needs interactive user authorization), its error — typically carrying
// an authorize URL — surfaces as the original task's failure, which the UI
// can render as a "setup required" call to action.
type IfMissing struct {
	Task   string            `yaml:"task"              json:"task"`             // fully-qualified task id, e.g. "auth/openrouter-oauth"
	Params map[string]string `yaml:"params,omitempty"  json:"params,omitempty"` // params forwarded to the prereq task (optional)
}

// EnvEntry declares one environment variable the task is allowed to access.
// Supports five forms in YAML:
//
//   - HOME                          # bare name: allowlist $HOME from host env, same name
//   - name: API_KEY                 # rename from host env: read $GH_TOKEN, expose as API_KEY
//     from: GH_TOKEN
//   - name: TOKEN                   # explicit env prefix (equivalent to bare)
//     from: env:GH_TOKEN
//   - name: PG_URL                  # provider-task lookup: spawn task "doppler" to resolve PG_URL
//     from: task:doppler
//   - name: DB_PASS                 # secret injection: resolve "db_password" from secrets store
//     secret: db_password
//   - name: LOG_LEVEL               # literal value (used by taskset overrides)
//     value: "info"
//
// Lookup rules:
//   - secret:        → secrets store only; run fails if key not found
//   - from: env:NAME → host OS environment only (os.Getenv); injected as entry.Name
//   - from: task:ID  → provider task ID; resolver spawns ID once per consumer
//     launch (batched across all task: entries with the same ID)
//   - from: bare     → identical to from: env:bare (backwards compat)
//   - bare entry     → allowlisted in --allow-env; script reads it from host env at runtime
//
// The optional `if_missing:` directive (only meaningful alongside `secret:`)
// runs a prereq task when the secret is absent. See the IfMissing type.
//
// The optional `default:` literal (only meaningful alongside `secret:`) is
// injected when the secret is not found, letting an operator stand a task up
// with a documented fallback (e.g. a dev password) before configuring the
// store. It takes precedence over `optional:`'s empty-string degrade.
type EnvEntry struct {
	Name      string     `yaml:"name"                  json:"name"`
	From      string     `yaml:"from,omitempty"        json:"from,omitempty"`       // host OS env var name to read and inject as Name
	Secret    string     `yaml:"secret,omitempty"      json:"secret,omitempty"`     // secrets store key to resolve and inject as Name
	Value     string     `yaml:"value,omitempty"       json:"value,omitempty"`      // literal value injection (taskset overrides)
	Default   string     `yaml:"default,omitempty"     json:"default,omitempty"`    // literal fallback injected when Secret is not found
	Optional  bool       `yaml:"optional,omitempty"    json:"optional,omitempty"`   // if true, missing secret → empty string instead of failure
	IfMissing *IfMissing `yaml:"if_missing,omitempty"  json:"if_missing,omitempty"` // prereq task to run when Secret is absent
}

// UnmarshalYAML allows EnvEntry to decode from a plain string, "KEY=VALUE" string, or a mapping.
func (e *EnvEntry) UnmarshalYAML(value *yaml.Node) error {
	if value.Kind == yaml.ScalarNode {
		s := value.Value
		if i := strings.IndexByte(s, '='); i >= 0 {
			e.Name = s[:i]
			e.Value = s[i+1:]
		} else {
			e.Name = s
		}
		return nil
	}
	type alias EnvEntry
	var a alias
	if err := value.Decode(&a); err != nil {
		return err
	}
	*e = EnvEntry(a)
	return nil
}

// Permissions declares what the task is explicitly allowed to access.
// Nothing is passed implicitly — every env var, filesystem path,
// subprocess executable, network host, and dicode API must be listed here.
type Permissions struct {
	// Env lists env vars the task script may read or that are injected into it.
	Env []EnvEntry `yaml:"env,omitempty" json:"env,omitempty"`
	// EnvReadExposed grants unrestricted env-var reads. For Deno it passes bare
	// --allow-env to the sandbox. For Python it disables the os.environ filter
	// added in #418 (by default Python tasks can only read the names listed in
	// Env plus a runtime-essential set). The exposed set is still bounded by
	// runtime.SubprocessEnv (an allowlist — PATH/HOME/cache/proxy/TLS vars,
	// DICODE_SOCKET/DICODE_TOKEN, and the task's own resolved vars; the daemon
	// master/admin keys are denylisted), so this never reaches anything the task
	// does not already hold. It exists for node-compat / npm tasks whose
	// transitive deps enumerate process.env at import time.
	EnvReadExposed bool `yaml:"env_read_exposed,omitempty" json:"env_read_exposed,omitempty"`
	// FS lists filesystem paths and their access modes ("r", "w", "rw").
	// Deno enforces reads and writes. Python enforces writes only: an
	// in-interpreter read allowlist would break normal execution (the
	// interpreter and site-packages read files constantly), so "r" entries
	// are ignored there and reads stay unrestricted.
	FS []FSEntry `yaml:"fs,omitempty" json:"fs,omitempty"`
	// Run lists executables the task may spawn (Deno and Python).
	// Use ["*"] for all; omit to deny all subprocess execution.
	Run []string `yaml:"run,omitempty" json:"run,omitempty"`
	// Net controls outbound network access (Deno, Python, Docker, and Podman).
	// Omit or use [] (empty list) to deny all network access (default).
	// Use ["*"] for unrestricted access.
	// List specific hosts to restrict: ["api.github.com", "hooks.slack.com"].
	//
	// Enforcement per runtime:
	//   Deno   — --allow-net=HOST list; exact per-host enforcement.
	//   Python — urllib/requests intercepted at stdlib level; exact per-host enforcement.
	//   Docker/Podman — when empty (and no ports published): container network mode
	//                   defaults to "none", denying all outbound connectivity.
	//                   When non-empty without "*": container gets unrestricted network
	//                   (per-host filtering is not yet implemented; a warning is logged).
	//                   When ["*"]: unrestricted. When docker.network_mode is set
	//                   explicitly, that value takes precedence over permissions.net.
	Net []string `yaml:"net,omitempty" json:"net,omitempty"`
	// Sys lists Deno system-info APIs the task may call (Deno only; the
	// names have no usable Python equivalent, so Python ignores this field).
	// Use ["*"] for all, or list specific names: ["hostname", "osRelease", "networkInterfaces"].
	// Omit to deny all sys access (default).
	Sys []string `yaml:"sys,omitempty" json:"sys,omitempty"`
	// Dicode controls which dicode runtime APIs (dicode.*, mcp.*) the task may call.
	// All dicode APIs are denied by default; each must be explicitly enabled.
	Dicode *DicodePermissions `yaml:"dicode,omitempty" json:"dicode,omitempty"`
}

// DicodePermissions declares which dicode runtime APIs the task may call.
// All dicode.* and mcp.* globals are denied by default; each must be explicitly enabled here.
type DicodePermissions struct {
	// Tasks enables dicode.run_task() and lists the target task IDs allowed.
	// Use ["*"] to allow all tasks. Omit (nil) to deny dicode.run_task() entirely.
	Tasks []string `yaml:"tasks,omitempty" json:"tasks,omitempty"`
	// MCP enables mcp.list_tools() and mcp.call() for the listed MCP daemon task IDs.
	// Use ["*"] to allow all MCP daemons. Omit (nil) to deny all MCP access.
	MCP []string `yaml:"mcp,omitempty" json:"mcp,omitempty"`
	// ListTasks enables dicode.list_tasks().
	ListTasks bool `yaml:"list_tasks,omitempty" json:"list_tasks,omitempty"`
	// GetRuns enables dicode.get_runs().
	GetRuns bool `yaml:"get_runs,omitempty" json:"get_runs,omitempty"`
	// SecretsWrite enables dicode.secrets_set() and dicode.secrets_delete().
	// Tasks may write or overwrite secrets but never read them back.
	SecretsWrite bool `yaml:"secrets_write,omitempty" json:"secrets_write,omitempty"`
	// SecretsHas enables dicode.secrets.has(key) — a boolean presence check.
	// Returns true/false only; never returns the secret value. Distinct from
	// SecretsWrite so tasks can check presence without write rights.
	SecretsHas bool `yaml:"secrets_has,omitempty" json:"secrets_has,omitempty"`
	// RunsListExpired enables dicode.runs.list_expired().
	RunsListExpired bool `yaml:"runs_list_expired,omitempty" json:"runs_list_expired,omitempty"`
	// RunsDeleteInput enables dicode.runs.delete_input().
	RunsDeleteInput bool `yaml:"runs_delete_input,omitempty" json:"runs_delete_input,omitempty"`
	// RunsPinInput enables dicode.runs.pin_input().
	RunsPinInput bool `yaml:"runs_pin_input,omitempty" json:"runs_pin_input,omitempty"`
	// RunsUnpinInput enables dicode.runs.unpin_input().
	RunsUnpinInput bool `yaml:"runs_unpin_input,omitempty" json:"runs_unpin_input,omitempty"`
	// RunsReplay enables dicode.runs.replay() — re-fires a previously
	// persisted run with its stored input.
	RunsReplay bool `yaml:"runs_replay,omitempty" json:"runs_replay,omitempty"`
	// RunsGetInput enables dicode.runs.get_input() — read another task's
	// persisted run input. Sensitive: grants cross-task input access.
	// Persisted inputs are redacted at write time (see #233's deny-list),
	// so the surface is bounded but still grants visibility into anything
	// not on the deny-list. Grant only to tasks that legitimately need to
	// inspect failed runs (auto-fix, audit, replay-tooling).
	RunsGetInput bool `yaml:"runs_get_input,omitempty" json:"runs_get_input,omitempty"`
	// TasksTest enables dicode.tasks.test() — runs a task's sibling test file
	// via pkg/tasktest.
	TasksTest bool `yaml:"tasks_test,omitempty" json:"tasks_test,omitempty"`
	// SourcesList enables dicode.sources.list() — names, types, and dev-mode
	// state of the configured taskset sources. Host paths are withheld from
	// the listing, so this grants no filesystem visibility on its own.
	SourcesList bool `yaml:"sources_list,omitempty" json:"sources_list,omitempty"`
	// SourcesSetDevMode enables dicode.sources.set_dev_mode() — toggles dev
	// mode (incl. clone-mode) on a configured taskset source.
	SourcesSetDevMode bool `yaml:"sources_set_dev_mode,omitempty" json:"sources_set_dev_mode,omitempty"`
	// GitCommitPush enables dicode.git.commit_push() — wraps
	// pkg/source/git.CommitPush for add/commit/push from an IPC task. (#234)
	GitCommitPush bool `yaml:"git_commit_push,omitempty" json:"git_commit_push,omitempty"`

	// Crypto enables dicode.crypto.encrypt() and dicode.crypto.decrypt() and
	// lists the context strings the task is allowed to use. The context is
	// bound into the AEAD's AAD, so a task with crypto: ["foo"] cannot decrypt
	// a blob produced under context "bar" — domain separation is enforced
	// cryptographically, not just at the access-control layer.
	//
	// Use ["*"] to allow all contexts (intended for admin/utility tasks only;
	// every named-context task should list its contexts explicitly).
	Crypto []string `yaml:"crypto,omitempty" json:"crypto,omitempty"`

	// AuditQuery enables dicode.audit.query() — read access to the security
	// audit trail (#415). Sensitive: the log lists every actor, target, and
	// denial across the system, so it is never granted ambiently. Grant only
	// to tasks that legitimately ship or inspect the audit log (e.g. the
	// buildin log exporters).
	AuditQuery bool `yaml:"audit_query,omitempty" json:"audit_query,omitempty"`
}

// WebuiNav declares a first-class navigation entry a task contributes to the
// main webui header, linking to this task's own webhook-served page (see
// WebuiConfig).
type WebuiNav struct {
	// Label is the link text shown in the nav. Required.
	Label string `yaml:"label" json:"label"`
	// Order controls left-to-right position among contributed nav entries
	// (ascending; default 0). Entries with equal Order are sorted by task ID.
	Order int `yaml:"order,omitempty" json:"order,omitempty"`
	// Icon is an optional icon identifier for the nav entry (renderer-defined).
	Icon string `yaml:"icon,omitempty" json:"icon,omitempty"`
}

// WebuiConfig lets a task contribute a first-class navigation entry to the
// main webui header, linking to this task's own webhook-served page (see
// dicode-buildin's auth-providers for a self-contained SPA task this targets).
type WebuiConfig struct {
	Nav *WebuiNav `yaml:"nav,omitempty" json:"nav,omitempty"`
}

// ProviderConfig declares secret-provider settings on a task that
// implements the issue #119 provider contract (calls dicode.output(map,
// { secret: true }) with a flat Record<string,string>).
//
// CacheTTL controls how long resolved values are cached. Zero (the
// default) disables caching. Cache key is (provider-task-id,
// secret-name); entries are busted when the task content hash changes.
type ProviderConfig struct {
	CacheTTL time.Duration `yaml:"cache_ttl,omitempty" json:"cache_ttl,omitempty"`
}

// Spec is parsed from task.yaml.
type Spec struct {
	Name        string        `yaml:"name"        json:"name"`
	Description string        `yaml:"description" json:"description"`
	Version     string        `yaml:"version"     json:"version"`
	Author      string        `yaml:"author,omitempty" json:"author,omitempty"`
	Runtime     Runtime       `yaml:"runtime"     json:"runtime"`
	Docker      *DockerConfig `yaml:"docker,omitempty" json:"docker,omitempty"`
	Trigger     TriggerConfig `yaml:"trigger"     json:"trigger"`
	Params      Params        `yaml:"params,omitempty"      json:"params,omitempty"`
	Permissions Permissions   `yaml:"permissions,omitempty" json:"permissions,omitempty"`
	Timeout     time.Duration `yaml:"timeout"             json:"timeout"`
	// MCPExposed, when true, marks this task as visible and callable via the
	// MCP endpoint (/mcp). Tasks default to NOT exposed — users must opt in
	// per task. The builtin MCP task filters dicode.list_tasks and gates
	// dicode.run_task using this flag so that connecting Claude Desktop or
	// Cursor to /mcp does not inadvertently surface internal tasks.
	MCPExposed bool `yaml:"mcp_exposed,omitempty" json:"mcp_exposed,omitempty"`
	// MCPPort declares that this daemon task exposes an MCP server on the given port.
	MCPPort int `yaml:"mcp_port,omitempty" json:"mcp_port,omitempty"`
	// OnFailureChain overrides the global defaults.on_failure_chain for this task.
	// Set to {task: ""} to disable the global default for this task only.
	OnFailureChain *OnFailureChainSpec `yaml:"on_failure_chain,omitempty" json:"on_failure_chain,omitempty"`

	// Silent, when true, detaches the task's stdout and stderr from the run-log
	// capture pipes (sends them to io.Discard). Use for tasks that handle
	// plaintext credentials so a careless console.log cannot leak them into
	// the run log. Combine with permissions.{net,fs,env}: [] to remove every
	// exfiltration channel.
	Silent bool `yaml:"silent,omitempty" json:"silent,omitempty"`

	// Provider declares this task as a secret provider implementing the
	// issue #119 contract. nil = not a provider; non-nil = provider with
	// the given config. The reconciler uses this to gate cache_ttl
	// validation; the resolver uses it to look up the TTL.
	Provider *ProviderConfig `yaml:"provider,omitempty" json:"provider,omitempty"`

	// Webui lets a task contribute a first-class navigation entry to the
	// main webui header, linking to this task's own webhook-served page.
	// nil = no nav contribution (default).
	Webui *WebuiConfig `yaml:"webui,omitempty" json:"webui,omitempty"`

	// RunInputs configures per-task input persistence. Overrides the global
	// defaults.run_inputs from dicode.yaml for this task only.
	RunInputs *RunInputsTaskOverride `yaml:"run_inputs,omitempty" json:"run_inputs,omitempty"`

	// RunResult configures per-task return-value persistence. When
	// `enabled: false` the JSON-marshalled return value is NOT written to
	// `runs.return_value` in the database. The value still flows in-memory:
	// synchronous callers of `dicode.run_task` receive it through WaitRun,
	// and chain triggers receive it via `input.output`. Only the persisted
	// (replayable / WebUI-visible) copy is suppressed.
	//
	// Use this for tasks that legitimately return secret material
	// (e.g. a template renderer that emits a rendered config with embedded
	// tokens). Structured `dicode.output(content, contentType)` data, stdout,
	// stderr, and `fail_reason` are unaffected — those persist as usual.
	RunResult *RunResultConfig `yaml:"run_result,omitempty" json:"run_result,omitempty"`

	// AutoFix configures how the auto-fix loop sees this task's input.
	// Independent of RunInputs (which controls persistence). Used by #238.
	AutoFix *AutoFixConfig `yaml:"auto_fix,omitempty" json:"auto_fix,omitempty"`

	// HashInclude names additional files or directories, each resolved
	// relative to TaskDir, whose content is folded into this task's content
	// hash (task.Hash) alongside its own dir. A task can import a shared
	// module living outside its own dir (e.g. a sibling buildin task's
	// helper library); without this, editing that module never perturbs the
	// importer's hash, so it never re-trips the reconciler reload or the
	// #392 approval-gate re-pend for the importer (#585). Most tasks don't
	// need this — the Deno/Python sandbox already only reads within TaskDir,
	// so only a task that explicitly imports a path outside it should set
	// this to the same path(s) it imports.
	HashInclude []string `yaml:"hash_include,omitempty" json:"hash_include,omitempty"`

	// Enabled is false when this task has been disabled via an override or
	// entry-level `enabled: false`. Disabled tasks remain in the registry and
	// appear in the API (with enabled:false) but are not scheduled, spawned,
	// or registered as webhooks by the trigger engine.
	// Default is true; the resolver sets it to true on load and flips it
	// when an override says false.
	Enabled bool `yaml:"-" json:"enabled"`

	// Template is an optional, namespaced identifier declaring that this
	// task is an instance of a generic template. Set in the BASE task.yaml;
	// the resolver propagates it to every entry that inherits via `ref.path`
	// because the field is part of the merged spec like any other.
	// Format: reverse-DNS prefix + slash + slug, e.g. "dicode.io/oauth-app".
	// Used today by `buildin/auth-providers` to surface BYO OAuth tasks in
	// the dashboard without hardcoding a per-task allowlist.
	Template string `yaml:"template,omitempty" json:"template,omitempty"`

	// TaskDir is the directory path of the task in the repo (not stored in YAML).
	TaskDir string `yaml:"-" json:"-"`
	// ManifestFile is the basename of the manifest actually loaded from
	// TaskDir — "task.yaml" or "task.yml" (not stored in YAML). Set by
	// loadDirWithVars so a caller that later needs to read-modify-write the
	// on-disk manifest (e.g. pkg/webui's trigger-editing endpoint) targets
	// the exact file this spec came from, rather than re-probing TaskDir and
	// risking a same-directory sibling manifest with different content
	// (#765). Empty for a Spec constructed without going through the
	// loader (e.g. directly in a test); callers should fall back to
	// ReadManifest's probing in that case.
	ManifestFile string `yaml:"-" json:"-"`
	// ID is derived from the directory name (not stored in YAML).
	ID string `yaml:"-" json:"id"`
	// Warnings holds non-fatal config-load warnings emitted during validate().
	// Callers (reconciler, taskset resolver) should log these via their zap
	// logger after LoadDir / LoadDirWithVars returns.
	Warnings []string `yaml:"-" json:"-"`
}

// RunInputsTaskOverride is the per-task override for run-input persistence.
type RunInputsTaskOverride struct {
	Enabled         *bool         `yaml:"enabled,omitempty"          json:"enabled,omitempty"`
	Retention       time.Duration `yaml:"retention,omitempty"        json:"retention,omitempty"`
	BodyFullTextual *bool         `yaml:"body_full_textual,omitempty" json:"body_full_textual,omitempty"`
}

// RunResultConfig configures per-task return-value persistence. See the
// Spec.RunResult doc comment for the full contract; the short version is
// that `enabled: false` suppresses the JSON-marshalled return value from
// being written to `runs.return_value`, while still letting it flow to
// in-memory consumers (synchronous `dicode.run_task` callers via WaitRun,
// chain triggers via `input.output`). Structured `dicode.output()` data,
// stdout, and stderr are unaffected.
type RunResultConfig struct {
	// Enabled (default true) controls return-value persistence. When set
	// to false, the engine skips the `SetRunResult` write for the
	// return_value column. A nil pointer is treated as the default (true).
	Enabled *bool `yaml:"enabled,omitempty" json:"enabled,omitempty"`
}

// PersistReturnValue reports whether this RunResultConfig allows the
// return value to be persisted to `runs.return_value`. Treats a nil
// receiver or a nil Enabled pointer as the default (true).
func (c *RunResultConfig) PersistReturnValue() bool {
	if c == nil || c.Enabled == nil {
		return true
	}
	return *c.Enabled
}

// AutoFixConfig configures how the auto-fix loop sees this task's input.
// Independent of run_inputs (which controls persistence). Used by #238.
type AutoFixConfig struct {
	// IncludeInput controls whether the auto-fix agent sees the task's
	// persisted input in its prompt. Default true. Set to false for tasks
	// whose inputs contain sensitive data the agent should not see.
	IncludeInput *bool `yaml:"include_input,omitempty" json:"include_input,omitempty"`

	// ShowRedactedFieldNames controls whether the agent sees a list of
	// redacted field names (e.g. "Authorization", "headers.x-api-key").
	// Default true. Set to false for tasks where field-name topology is
	// itself sensitive.
	ShowRedactedFieldNames *bool `yaml:"show_redacted_field_names,omitempty" json:"show_redacted_field_names,omitempty"`
}

// LoadDir reads a task from its directory (expects task.yaml and task.<ext>).
// Equivalent to LoadDirWithVars(dir, nil). Use LoadDirWithVars from source
// loaders that know about per-source context (TASK_SET_DIR, …).
func LoadDir(dir string) (*Spec, error) {
	return LoadDirWithVars(dir, nil)
}

// ReadManifest returns the path and raw content of dir's task manifest file
// — task.yaml, falling back to task.yml (see openTaskSpecFile) — as a
// single open+read, not a path lookup a caller then reopens (which would
// double the syscalls and open a TOCTOU window against a concurrent source
// sync). Callers that need to read-modify-write a task's on-disk manifest
// directly (e.g. pkg/webui's trigger-editing endpoint) use this instead of
// hardcoding task.yaml, so a task.yml-only task (#765) can still be edited
// rather than hitting a spurious "no such file" error.
func ReadManifest(dir string) (path string, data []byte, err error) {
	return readManifest(dir, "")
}

// ReadManifestFile is ReadManifest for a caller that already knows the exact
// manifest filename to read (e.g. spec.ManifestFile, as recorded by the
// loader that produced spec) — it reads dir/filename directly, with no
// task.yaml/task.yml probing, so a read-modify-write cycle always targets
// the same file the spec was actually loaded from, never a same-directory
// sibling with different content (#765). filename must be non-empty.
func ReadManifestFile(dir, filename string) (path string, data []byte, err error) {
	if filename == "" {
		return "", nil, fmt.Errorf("ReadManifestFile: filename must be non-empty for %s (use ReadManifest for probing)", dir)
	}
	return readManifest(dir, filename)
}

func readManifest(dir, filename string) (path string, data []byte, err error) {
	f, specPath, err := openTaskSpecFile(dir, filename)
	if err != nil {
		return "", nil, fmt.Errorf("open %s: %w", specPath, err)
	}
	defer f.Close()
	data, err = io.ReadAll(f)
	if err != nil {
		return "", nil, fmt.Errorf("read %s: %w", specPath, err)
	}
	return specPath, data, nil
}

// openTaskSpecFile opens a task manifest in dir. When filename is non-empty,
// it opens exactly dir/filename — no probing — for callers (the taskset
// resolver) that already know which manifest name a prior kind-detection
// pass vetted, so the file actually loaded is always the same file that was
// checked, never a different sibling. When filename is empty, it probes
// task.yaml then task.yml, accepting them interchangeably — the same pair
// pkg/taskset/resolver.go's isTaskFileName and resolveYAMLPath recognize —
// for callers (LoadDir, ScanDir-discovered sources) that only have a bare
// directory. Without this fallback, a ref that resolves onto a bare
// task.yml (#765) passed resolver kind-detection but then failed to load
// here with a confusing "open task.yaml ...: no such file or directory",
// since the ref's resolved file was discarded in favor of re-deriving a
// hardcoded task.yaml path from its parent directory. In probe mode,
// task.yaml is tried first and preferred on ties (a directory with both is
// not expected, but favors the conventional name); if task.yml exists but
// fails to open for a reason other than not-existing (e.g. a permission
// error), that error is returned rather than silently falling back to the
// task.yaml-not-found error.
//
// filename (when non-empty) is expected to already be a bare basename — the
// resolver passes filepath.Base(...) of an already ref-resolution-validated
// path — but this backs an exported entry point (LoadDirWithVarsFile/
// LoadPipelineDirFile), so containment is enforced explicitly via os.Root
// (Go 1.24+) — the standard library's own sandboxed file-access API, which
// rejects a name that would resolve outside dir (including through a
// symlink) at the OS level — rather than trusting every future caller to
// pre-sanitize.
//
// This intentionally does not go through internal/pathguard — dicode's
// usual single audited containment check — despite the duplication: a
// static-analysis pass over this exact code flagged the equivalent
// pathguard.Within call (and, after that, an inlined filepath.Rel-based
// check) as an unrecognized barrier and kept alerting on the os.Open sink,
// where os.Root's stdlib-native sandboxing is what it accepted. One Root is
// opened per call and shared across both probe candidates (rather than one
// os.OpenRoot per candidate) to avoid a redundant directory-open syscall on
// the common task.yml-only path.
func openTaskSpecFile(dir, filename string) (*os.File, string, error) {
	hint := filename
	if hint == "" {
		hint = "task.yaml"
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, filepath.Join(dir, hint), err
	}
	defer root.Close()

	if filename != "" {
		p := filepath.Join(dir, filename)
		f, err := root.Open(filename)
		return f, p, err
	}

	specPath := filepath.Join(dir, "task.yaml")
	f, err := root.Open("task.yaml")
	if err == nil {
		return f, specPath, nil
	}
	// Try task.yml regardless of why task.yaml failed (not found, or some
	// other error like permission-denied) — a readable task.yml should
	// still load even if a stray, unreadable task.yaml sits alongside it.
	ymlPath := filepath.Join(dir, "task.yml")
	ymlFile, ymlErr := root.Open("task.yml")
	if ymlErr == nil {
		return ymlFile, ymlPath, nil
	}
	// Both failed: prefer surfacing whichever error isn't a plain "not
	// found" (more actionable — e.g. a permission error) over the other's
	// not-found, defaulting to task.yaml's error when both are equally
	// uninformative not-found errors.
	if !os.IsNotExist(err) {
		return nil, specPath, err
	}
	if !os.IsNotExist(ymlErr) {
		return nil, ymlPath, ymlErr
	}
	return nil, specPath, err
}

// LoadDirWithVars reads a task from its directory, expanding ${VAR} references
// in the spec using built-in variables merged with the caller-supplied extras.
// Pass nil for extras when loading a task outside of a source context.
// Probes for task.yaml then task.yml (see openTaskSpecFile) — use
// LoadDirWithVarsFile instead when the caller already knows the exact
// manifest filename a prior kind-detection pass vetted.
//
// Typical extras:
//   - TASK_SET_DIR: directory of the root taskset.yaml for taskset sources,
//     or the source root for raw local/git sources. Injected automatically
//     by pkg/taskset/resolver.Resolve and by pkg/source/{local,git}.
//
// See pkg/task/template.go and docs/task-template-vars.md for the full
// variable set and resolution rules.
func LoadDirWithVars(dir string, extras map[string]string) (*Spec, error) {
	return loadDirWithVars(dir, "", extras)
}

// LoadDirWithVarsFile is LoadDirWithVars for a caller that already knows the
// exact manifest filename to load (e.g. pkg/taskset/resolver.go, after its
// own DetectKind/mustBeTask check already read that specific file) — it
// opens dir/filename directly, with no task.yaml/task.yml probing, so the
// file actually loaded is always the same file the kind-detection/security
// check already vetted, never a same-directory sibling with different
// content. filename must be non-empty — pass it through LoadDirWithVars
// instead of "" to get the probing behavior; that distinction is enforced,
// not silently interchangeable, so an accidental empty filename can't
// reintroduce the sibling-manifest mismatch this function exists to close.
func LoadDirWithVarsFile(dir, filename string, extras map[string]string) (*Spec, error) {
	if filename == "" {
		return nil, fmt.Errorf("LoadDirWithVarsFile: filename must be non-empty for %s (use LoadDirWithVars for probing)", dir)
	}
	return loadDirWithVars(dir, filename, extras)
}

func loadDirWithVars(dir, filename string, extras map[string]string) (*Spec, error) {
	f, specPath, err := openTaskSpecFile(dir, filename)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", specPath, err)
	}
	defer f.Close()
	data, err := io.ReadAll(f)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", specPath, err)
	}

	// Probe for the removed `notify:` block before decoding. yaml.v3's
	// tolerant decode would otherwise drop it silently and the task author
	// would lose alerts they think are still configured. Notifications are
	// now task-based via on_failure_chain (#279).
	var probe struct {
		Notify any `yaml:"notify"`
	}
	if err := yaml.Unmarshal(data, &probe); err == nil && probe.Notify != nil {
		return nil, fmt.Errorf("%s: legacy `notify` block detected. "+
			"The per-task notify field was removed (#279). Use `on_failure_chain` "+
			"to fire a notification task on failure — see docs", specPath)
	}

	var spec Spec
	if err := yaml.Unmarshal(data, &spec); err != nil {
		return nil, fmt.Errorf("parse %s: %w", specPath, err)
	}

	if err := spec.validate(); err != nil {
		return nil, fmt.Errorf("invalid task in %s: %w", dir, err)
	}

	spec.TaskDir = dir
	spec.ManifestFile = filepath.Base(specPath)
	spec.ID = filepath.Base(dir)
	// Default Enabled to true; the taskset resolver may flip it to false if
	// an override or entry-level `enabled: false` is in effect.
	spec.Enabled = true

	// Expand ${VAR} template references in paths, secrets, and env indirection
	// keys. Kept narrow — see expandSpec for the allowlist and
	// pkg/task/template.go for the resolution rules.
	expandSpec(&spec, builtinVars(dir, extras))

	// Runs AFTER expansion: it can only tell a real secret from an unresolved
	// ${VAR} placeholder once expansion has been attempted.
	normalizeWebhookAuth(&spec)

	if spec.Runtime == "" || spec.Runtime == "js" {
		spec.Runtime = RuntimeDeno
	}
	return &spec, nil
}

// ScriptPath returns the path to the task script file.
// Returns empty string for runtimes that don't use a script file (e.g. Docker).
// For the deno runtime, task.ts is preferred over task.js.
// For other runtimes, the first existing task.<ext> candidate is returned;
// callers that know the exact extension should construct the path themselves.
func (s *Spec) ScriptPath() string {
	switch s.Runtime {
	case RuntimeDeno:
		ts := filepath.Join(s.TaskDir, "task.ts")
		if fi, err := os.Lstat(ts); err == nil && fi.Mode()&os.ModeSymlink == 0 {
			return ts
		}
		p := filepath.Join(s.TaskDir, "task.js")
		if fi, err := os.Lstat(p); err == nil && fi.Mode()&os.ModeSymlink == 0 {
			return p
		}
		return ""
	case RuntimeDocker, RuntimePodman:
		return ""
	default:
		// For subprocess runtimes, look for any task.* file in the task dir.
		// Symlinks are rejected to prevent reading files outside the task directory.
		for _, ext := range []string{".py", ".jl", ".rb", ".sh", ".ts", ".js", ".mjs"} {
			p := filepath.Join(s.TaskDir, "task"+ext)
			if fi, err := os.Lstat(p); err == nil && fi.Mode()&os.ModeSymlink == 0 {
				return p
			}
		}
		return ""
	}
}

// Script reads and returns the task script source.
func (s *Spec) Script() (string, error) {
	p := s.ScriptPath()
	if p == "" {
		return "", fmt.Errorf("no script file found for task %s (file missing or is a symlink)", s.Name)
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return "", fmt.Errorf("read script %s: %w", p, err)
	}
	return string(b), nil
}

// Validate runs the per-spec consistency checks (shape of trigger config,
// docker section, etc.) and returns the first violation. Exposed publicly
// so callers that mutate a Spec after LoadDir (e.g. per-edge override
// dispatch in pkg/trigger) can re-validate the resulting variant before
// dispatching it.
func (s *Spec) Validate() error { return s.validate() }

func (s *Spec) validate() error {
	// Clear stale warnings so re-validation (e.g. on a reloaded spec) starts
	// from a clean slate; otherwise warnings accumulate across validate
	// calls.
	s.Warnings = nil
	if s.Name == "" {
		return fmt.Errorf("name is required")
	}
	for _, e := range s.Permissions.Env {
		if e.Name == "*" {
			return fmt.Errorf(`permissions.env: a name-only "*" entry is no longer accepted; set "env_read_exposed: true" to grant the Deno sandbox bare --allow-env`)
		}
	}
	if s.Webui != nil && s.Webui.Nav != nil && s.Webui.Nav.Label == "" {
		return fmt.Errorf("webui.nav.label is required")
	}
	for _, inc := range s.HashInclude {
		if inc == "" {
			return fmt.Errorf("hash_include: entries must not be empty")
		}
		if filepath.IsAbs(inc) {
			return fmt.Errorf("hash_include: %q must be relative to the task directory, not absolute", inc)
		}
		if !hashIncludeLexicallyInBounds(inc) {
			return fmt.Errorf("hash_include: %q must resolve within the task's parent directory — at most one \"..\" hop up (e.g. \"../sibling-task/file.ts\"), never further up the filesystem or to the parent directory itself", inc)
		}
	}
	triggers := 0
	if s.Trigger.Cron != "" {
		triggers++
	}
	if s.Trigger.Webhook != "" {
		triggers++
	}
	// "any" mode has a dead HMAC path (and misleadingly implies relay-reachability)
	// without a secret to verify against.
	if s.Trigger.WebhookAuth == WebhookAuthAny && s.Trigger.WebhookSecret == "" {
		return fmt.Errorf(`trigger.auth: "any" requires webhook_secret (the HMAC path has nothing to verify without it)`)
	}
	if s.Trigger.Manual {
		triggers++
	}
	if s.Trigger.Daemon {
		triggers++
		switch s.Trigger.Restart {
		case "", "always", "on-failure", "never":
			// ok
		default:
			return fmt.Errorf("trigger.restart must be always, on-failure, or never")
		}
	}
	if s.Trigger.Chain != nil {
		triggers++
		if s.Trigger.Chain.From == "" {
			return fmt.Errorf("trigger.chain.from is required")
		}
		switch s.Trigger.Chain.On {
		case "", "success", "failure", "always":
			// ok
		default:
			return fmt.Errorf("trigger.chain.on must be success, failure, or always")
		}
		// Reject reserved keys at config load so the engine never has to
		// disambiguate user vs. engine values at firing time. Mirrors the
		// check on OnFailureChainSpec.Params (see onfailurechain.go).
		for k, v := range s.Trigger.Chain.Params {
			if _, reserved := reservedChainParamKeys[k]; reserved {
				return fmt.Errorf("trigger.chain.params: %q is a reserved key (used by the engine)", k)
			}
			// Static validation of `${input.…}` references: catches
			// malformed shapes (${input.foo}, ${input.output.a.b}, …)
			// at config load. The resolver type-asserts per token at
			// dispatch time; this surfaces typos before the first
			// chain fire.
			if sv, ok := v.(string); ok {
				if err := ValidateInputRefs(fmt.Sprintf("trigger.chain.params.%s", k), sv); err != nil {
					return err
				}
			}
		}
		if s.Trigger.Chain.Overrides != nil {
			if err := validatePerEdgeOverrides("trigger.chain.overrides", s.Trigger.Chain.Overrides); err != nil {
				return err
			}
		}
	}
	if triggers == 0 {
		return fmt.Errorf("at least one trigger must be configured (cron, webhook, manual, or chain)")
	}
	if triggers > 1 {
		return fmt.Errorf("only one trigger type is allowed per task")
	}
	s.Warnings = append(s.Warnings, webhookSecretGatedFieldWarnings(s.Trigger.WebhookSecret, s.Trigger.ReplayProtection, s.Trigger.RequireTimestamp)...)
	if s.OnFailureChain != nil {
		warns, err := s.OnFailureChain.Validate()
		if err != nil {
			return fmt.Errorf("on_failure_chain: %w", err)
		}
		s.Warnings = append(s.Warnings, warns...)
	}
	switch s.Runtime {
	case RuntimeDeno, "js", "":
		// ok — "js" is a legacy alias for "deno"
	case RuntimeDocker, RuntimePodman:
		if s.Docker == nil {
			return fmt.Errorf("runtime %s requires a docker: section in task.yaml", s.Runtime)
		}
		if s.Docker.Image == "" && s.Docker.Build == nil {
			return fmt.Errorf("docker: requires either image or build")
		}
		switch s.Docker.PullPolicy {
		case "", "missing", "always", "never":
			// ok
		default:
			return fmt.Errorf("docker.pull_policy must be always, missing, or never")
		}
		s.Warnings = append(s.Warnings, dockerHardeningWarnings(s.Docker)...)
	default:
		// Any other non-empty runtime is accepted; executor presence is checked at run time.
	}
	return nil
}

// hashIncludeLexicallyInBounds reports whether inc, as a hash_include entry,
// stays within the strict-descendant boundary task.Hash's resolveInclude
// enforces at hash time — dir's parent directory, i.e. at most one ".." hop
// up from the task's own directory. Checked here too, at config-load time,
// rather than leaving it to fail only inside task.Hash later: a lexically
// out-of-bounds entry is a config typo an author can fix on the spot, so it
// should surface immediately as a load error instead of waiting for the
// reconciler's next poll. This check can't catch every escape, though — a
// symlink partway down an in-bounds-looking path can still redirect outside
// the boundary, and that's only visible once task.Hash actually resolves it
// at hash time (see resolveInclude, and pkg/taskset/source.go's snapHash for
// how that later failure is handled — #682).
//
// Purely lexical, no filesystem access (symlink-aware containment is
// task.Hash's job, once an absolute dir is known — see resolveInclude).
// filepath.Join(dir, inc) always cleans inc as a standalone relative path
// first, so counting inc's own leading ".." segments (via filepath.Clean
// from a virtual "." origin) determines how many levels above dir the
// resolved path would land, independent of dir's actual value — true for
// any clean, non-".."-containing absolute dir, i.e. any real filesystem
// path.
func hashIncludeLexicallyInBounds(inc string) bool {
	cleaned := filepath.Clean(filepath.FromSlash(inc))
	if cleaned == ".." {
		return false // resolves to exactly the boundary itself
	}
	depth := 0
	for _, seg := range strings.Split(cleaned, string(filepath.Separator)) {
		if seg != ".." {
			break
		}
		depth++
	}
	return depth <= 1
}

// webhookSecretGatedFieldWarnings flags trigger.replay_protection /
// trigger.require_timestamp set without a trigger.webhook_secret. Both
// fields only take effect inside the HMAC verification path
// (verifyWebhookSignatureSecret / checkWebhookReplay in pkg/trigger), which
// returns immediately, unauthenticated, whenever the secret is empty — so
// without a secret they are silent no-ops rather than the hardening the
// operator likely intended.
func webhookSecretGatedFieldWarnings(webhookSecret string, replayProtection, requireTimestamp *bool) []string {
	if webhookSecret != "" {
		return nil
	}
	var warnings []string
	if replayProtection != nil && *replayProtection {
		warnings = append(warnings, "trigger.replay_protection is set but trigger.webhook_secret is empty — the webhook is unauthenticated and replay_protection has no effect")
	}
	if requireTimestamp != nil && *requireTimestamp {
		warnings = append(warnings, "trigger.require_timestamp is set but trigger.webhook_secret is empty — the webhook is unauthenticated and require_timestamp has no effect")
	}
	return warnings
}

// dockerHardeningWarnings flags container settings that visibly weaken
// isolation so they surface in the UI at config-load time. Warnings are
// advisory only; the hard security floor is enforced at dispatch by
// pkg/runtime/containersec.Validate (issue #380), which rejects these
// settings — plus sensitive bind mounts — unless the operator opted in via
// the container_security block in dicode.yaml.
func dockerHardeningWarnings(d *DockerConfig) []string {
	var warns []string
	if d.NetworkMode == "host" {
		warns = append(warns, "docker.network_mode: host shares the host network namespace; the container can reach every loopback service")
	}
	for _, c := range d.CapAdd {
		switch strings.ToUpper(c) {
		case "SYS_ADMIN", "SYS_PTRACE", "SYS_MODULE", "ALL":
			warns = append(warns, fmt.Sprintf("docker.cap_add includes %s — this can enable container escape", c))
		}
	}
	for _, o := range d.SecurityOpt {
		lower := strings.ToLower(o)
		switch {
		case strings.HasPrefix(lower, "seccomp=unconfined"),
			strings.HasPrefix(lower, "apparmor=unconfined"),
			lower == "label=disable":
			warns = append(warns, fmt.Sprintf("docker.security_opt %q disables a kernel sandbox layer", o))
		}
	}
	return warns
}

// ChainOn returns the normalized "on" condition for a chain trigger.
// Defaults to "success" if unset.
func (c *ChainTrigger) ChainOn() string {
	if c.On == "" {
		return "success"
	}
	return c.On
}
