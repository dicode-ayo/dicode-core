package task

import (
	"fmt"
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
// the Dockerfile content changes.
//
// TODO: clean up old dicode-<taskID>:* images when a task is removed or the Dockerfile changes.
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
}

// ChainTrigger fires a task when another task completes.
type ChainTrigger struct {
	From string `yaml:"from"`         // task ID to listen for
	On   string `yaml:"on,omitempty"` // "success" (default) | "failure" | "always"
}

// TriggerConfig defines how a task is triggered.
// Exactly one of Cron, Webhook, Manual, Chain, or Daemon should be set.
type TriggerConfig struct {
	Cron          string        `yaml:"cron,omitempty"`           // cron expression e.g. "0 9 * * *"
	Webhook       string        `yaml:"webhook,omitempty"`        // HTTP path e.g. "/hooks/my-task"
	WebhookSecret string        `yaml:"webhook_secret,omitempty"` // HMAC-SHA256 secret for webhook auth
	WebhookAuth   bool          `yaml:"auth,omitempty"`           // require dicode session for GET (UI) and POST (run)
	Manual        bool          `yaml:"manual,omitempty"`         // only via explicit trigger
	Chain         *ChainTrigger `yaml:"chain,omitempty"`          // fire when another task completes
	Daemon        bool          `yaml:"daemon,omitempty"`         // start on app start, restart on exit
	Restart       string        `yaml:"restart,omitempty"`        // daemon only: "always"(default)|"on-failure"|"never"
}

// Param defines a user-configurable input for a task.
type Param struct {
	Name        string `yaml:"name"`
	Type        string `yaml:"type"` // "string" | "number" | "boolean" | "cron"
	Default     string `yaml:"default"`
	Description string `yaml:"description"`
	Required    bool   `yaml:"required"`
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
// Supports four forms in YAML:
//
//   - HOME                          # bare name: allowlist $HOME from host env, same name
//   - name: API_KEY                 # rename from host env: read $GH_TOKEN, expose as API_KEY
//     from: GH_TOKEN
//   - name: DB_PASS                 # secret injection: resolve "db_password" from secrets store
//     secret: db_password
//   - name: LOG_LEVEL               # literal value (used by taskset overrides)
//     value: "info"
//
// Lookup rules:
//   - secret: → secrets store only; run fails if key not found
//   - from:   → host OS environment only (os.Getenv); injected as entry.Name
//   - bare name (no secret/from/value) → allowlisted in --allow-env; script reads it from host env at runtime
//
// The optional `if_missing:` directive (only meaningful alongside `secret:`)
// runs a prereq task when the secret is absent. See the IfMissing type.
type EnvEntry struct {
	Name      string     `yaml:"name"                  json:"name"`
	From      string     `yaml:"from,omitempty"        json:"from,omitempty"`       // host OS env var name to read and inject as Name
	Secret    string     `yaml:"secret,omitempty"      json:"secret,omitempty"`     // secrets store key to resolve and inject as Name
	Value     string     `yaml:"value,omitempty"       json:"value,omitempty"`      // literal value injection (taskset overrides)
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
	// FS lists filesystem paths and their access modes ("r", "w", "rw"). Deno only.
	FS []FSEntry `yaml:"fs,omitempty" json:"fs,omitempty"`
	// Run lists executables the task may spawn via Deno.Command. Use ["*"] for all. Deno only.
	Run []string `yaml:"run,omitempty" json:"run,omitempty"`
	// Net controls outbound network access (Deno only).
	// Omit or use ["*"] for unrestricted access (--allow-net).
	// List specific hosts to restrict: ["api.github.com", "hooks.slack.com"].
	// Use [] (empty list) to deny all network access.
	Net []string `yaml:"net,omitempty" json:"net,omitempty"`
	// Sys lists Deno system-info APIs the task may call (Deno only).
	// Use ["*"] for all, or list specific names: ["hostname", "osRelease", "networkInterfaces"].
	// Omit to deny all sys access (default).
	Sys []string `yaml:"sys,omitempty" json:"sys,omitempty"`
	// Dicode controls which dicode runtime APIs (dicode.*, mcp.*) the task may call.
	// All dicode APIs are denied by default; each must be explicitly enabled.
	Dicode *DicodePermissions `yaml:"dicode,omitempty" json:"dicode,omitempty"`
}

// NotifyConfig controls when dicode sends push notifications for a task.
// Nil fields inherit from the parent TaskSet defaults or the global config.
type NotifyConfig struct {
	OnSuccess *bool `yaml:"on_success,omitempty" json:"on_success,omitempty"`
	OnFailure *bool `yaml:"on_failure,omitempty" json:"on_failure,omitempty"`
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
	// GetConfig enables dicode.get_config().
	GetConfig bool `yaml:"get_config,omitempty" json:"get_config,omitempty"`
	// SecretsWrite enables dicode.secrets_set() and dicode.secrets_delete().
	// Tasks may write or overwrite secrets but never read them back.
	SecretsWrite bool `yaml:"secrets_write,omitempty" json:"secrets_write,omitempty"`
	// OAuthInit enables dicode.oauth.build_auth_url(). Reserved for the
	// auth-start built-in task. Granting this lets the task construct a
	// signed /auth/:provider URL using the daemon's relay identity; the
	// payload layout is hardcoded in Go so this primitive cannot be coaxed
	// into producing signatures for other message types (e.g. WSS
	// handshakes).
	OAuthInit bool `yaml:"oauth_init,omitempty" json:"oauth_init,omitempty"`
	// OAuthStore enables dicode.oauth.store_token(). Reserved for the
	// auth-relay built-in task. The primitive decrypts an incoming token
	// envelope and writes the resulting credentials directly into the
	// secrets store — plaintext never reaches task code, so a careless
	// console.log cannot leak a token.
	OAuthStore bool `yaml:"oauth_store,omitempty" json:"oauth_store,omitempty"`
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
	Notify      *NotifyConfig `yaml:"notify,omitempty" json:"notify,omitempty"`
	// MCPPort declares that this daemon task exposes an MCP server on the given port.
	MCPPort int `yaml:"mcp_port,omitempty" json:"mcp_port,omitempty"`
	// OnFailureChain overrides the global defaults.on_failure_chain for this task.
	// nil = inherit global default, "" = disable, "task-id" = custom target.
	OnFailureChain *string `yaml:"on_failure_chain,omitempty" json:"on_failure_chain,omitempty"`

	// TaskDir is the directory path of the task in the repo (not stored in YAML).
	TaskDir string `yaml:"-" json:"-"`
	// ID is derived from the directory name (not stored in YAML).
	ID string `yaml:"-" json:"id"`
}

// LoadDir reads a task from its directory (expects task.yaml and task.<ext>).
// Equivalent to LoadDirWithVars(dir, nil). Use LoadDirWithVars from source
// loaders that know about per-source context (TASK_SET_DIR, …).
func LoadDir(dir string) (*Spec, error) {
	return LoadDirWithVars(dir, nil)
}

// LoadDirWithVars reads a task from its directory, expanding ${VAR} references
// in the spec using built-in variables merged with the caller-supplied extras.
// Pass nil for extras when loading a task outside of a source context.
//
// Typical extras:
//   - TASK_SET_DIR: directory of the root taskset.yaml for taskset sources,
//     or the source root for raw local/git sources. Injected automatically
//     by pkg/taskset/resolver.Resolve and by pkg/source/{local,git}.
//
// See pkg/task/template.go and docs/task-template-vars.md for the full
// variable set and resolution rules.
func LoadDirWithVars(dir string, extras map[string]string) (*Spec, error) {
	specPath := filepath.Join(dir, "task.yaml")
	f, err := os.Open(specPath)
	if err != nil {
		return nil, fmt.Errorf("open task.yaml in %s: %w", dir, err)
	}
	defer f.Close()

	var spec Spec
	if err := yaml.NewDecoder(f).Decode(&spec); err != nil {
		return nil, fmt.Errorf("parse task.yaml in %s: %w", dir, err)
	}

	if err := spec.validate(); err != nil {
		return nil, fmt.Errorf("invalid task in %s: %w", dir, err)
	}

	spec.TaskDir = dir
	spec.ID = filepath.Base(dir)

	// Expand ${VAR} template references in paths, secrets, and env indirection
	// keys. Kept intentionally narrow — see expandSpec for the allowlist and
	// pkg/task/template.go for the resolution rules.
	expandSpec(&spec, builtinVars(dir, extras))

	if spec.Runtime == "" || spec.Runtime == "js" {
		spec.Runtime = RuntimeDeno
	}
	// Container and daemon tasks may run indefinitely; don't impose a default timeout.
	if spec.Timeout == 0 && spec.Runtime != RuntimeDocker && spec.Runtime != RuntimePodman && !spec.Trigger.Daemon {
		spec.Timeout = 60 * time.Second
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
		if _, err := os.Stat(ts); err == nil {
			return ts
		}
		return filepath.Join(s.TaskDir, "task.js")
	case RuntimeDocker, RuntimePodman:
		return ""
	default:
		// For subprocess runtimes, look for any task.* file in the task dir.
		for _, ext := range []string{".py", ".jl", ".rb", ".sh", ".ts", ".js", ".mjs"} {
			p := filepath.Join(s.TaskDir, "task"+ext)
			if _, err := os.Stat(p); err == nil {
				return p
			}
		}
		return ""
	}
}

// Script reads and returns the task script source.
func (s *Spec) Script() (string, error) {
	b, err := os.ReadFile(s.ScriptPath())
	if err != nil {
		return "", fmt.Errorf("read script %s: %w", s.ScriptPath(), err)
	}
	return string(b), nil
}

func (s *Spec) validate() error {
	if s.Name == "" {
		return fmt.Errorf("name is required")
	}
	triggers := 0
	if s.Trigger.Cron != "" {
		triggers++
	}
	if s.Trigger.Webhook != "" {
		triggers++
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
	}
	if triggers == 0 {
		return fmt.Errorf("at least one trigger must be configured (cron, webhook, manual, or chain)")
	}
	if triggers > 1 {
		return fmt.Errorf("only one trigger type is allowed per task")
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
	default:
		// Any other non-empty runtime is accepted; executor presence is checked at run time.
	}
	return nil
}

// ChainOn returns the normalized "on" condition for a chain trigger.
// Defaults to "success" if unset.
func (c *ChainTrigger) ChainOn() string {
	if c.On == "" {
		return "success"
	}
	return c.On
}
