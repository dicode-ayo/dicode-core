package approval

import (
	"fmt"
	"sort"
	"strings"

	"github.com/dicode/dicode/pkg/task"
	"go.uber.org/zap"
)

// Trigger kinds reported by State.
const (
	TriggerCron    = "cron"
	TriggerWebhook = "webhook"
	TriggerManual  = "manual"
	TriggerChain   = "chain"
	TriggerDaemon  = "daemon"
)

// Where an environment variable's value comes from. The reference itself is
// reported; it is never followed.
const (
	EnvFromHost   = "host"
	EnvFromSecret = "secret"
	EnvFromTask   = "task"
	EnvLiteral    = "literal"
)

// State is the review surface for a pending task: the resolved task as it will
// run if the operator arms it. It is derived entirely from the parsed spec and
// the task directory, so it needs no baseline — a task with no git history, no
// prior approval and no cached snapshot still renders completely.
//
// Two invariants bound what may appear here (ADR-0003):
//
//   - No secret is dereferenced. An env entry renders as its declaration — the
//     exposed name and where the value comes from — and the reference is never
//     followed. Literal values written inline in task.yaml do not render at
//     all, in any field.
//   - No code bytes render. Files appear as an inventory of names, sizes and
//     hashes; a change is visible without any content being displayed.
//
// The spec fields and PendingHash come from the pend-time entry while the
// inventory reads the live directory, so a push landing in between can leave
// the file list describing newer bytes than the hash on the same screen. That
// cannot arm unreviewed content — approval binds to PendingHash and FireGuard
// re-hashes the directory at fire time — it only means the two halves of one
// screen can describe adjacent generations.
type State struct {
	TaskID string `json:"task_id"`
	// PendingHash is the content hash the gate observed when it held this
	// task. A caller approving what it reviewed must send this back via
	// ApproveIfHash: the task can re-pend at a newer hash between render and
	// click, and an unbound approval would arm that unreviewed version.
	PendingHash string `json:"pending_hash"`
	Kind        string `json:"kind"`
	Name        string `json:"name,omitempty"`
	Description string `json:"description,omitempty"`
	// Runtime and Image are empty for a pipeline, which runs no code of its own.
	Runtime     string `json:"runtime,omitempty"`
	Image       string `json:"image,omitempty"`
	NetworkMode string `json:"network_mode,omitempty"`
	// Timeout is the resolved wall-clock budget, formatted (e.g. "1m30s").
	Timeout     string          `json:"timeout,omitempty"`
	Triggers    []Trigger       `json:"triggers,omitempty"`
	Permissions Permissions     `json:"permissions"`
	Env         []EnvDecl       `json:"env,omitempty"`
	Params      []ParamDecl     `json:"params,omitempty"`
	Container   *Container      `json:"container,omitempty"`
	Stages      []Stage         `json:"stages,omitempty"`
	Files       []task.FileMeta `json:"files,omitempty"`
	// FilesError explains why Files is absent when the inventory could not be
	// built. A dir-less task legitimately has no files, so an empty list alone
	// cannot distinguish "nothing to list" from "the listing failed" — and a
	// review surface that hides its own gaps is worse than one that admits
	// them.
	FilesError string `json:"files_error,omitempty"`
	Enabled    bool   `json:"enabled"`
	MCPExposed bool   `json:"mcp_exposed,omitempty"`
}

// Trigger is one resolved way the task can fire. Cron expressions and webhook
// paths are concrete: an override can rewrite either from outside the task
// directory, and the operator is arming what resolution produced.
type Trigger struct {
	Kind    string `json:"kind"`
	Cron    string `json:"cron,omitempty"`
	Webhook string `json:"webhook,omitempty"`
	// Auth is the webhook's resolved auth mode.
	Auth string `json:"auth,omitempty"`
	// Signed reports that the webhook verifies an HMAC signature. An
	// unresolved ${VAR} placeholder is not a secret and does not count: under
	// auth: session it survives normalization, and reporting it as signed
	// would claim a verification this webhook cannot perform. The secret
	// itself is never rendered.
	Signed bool `json:"signed,omitempty"`
	// Restart is the daemon restart policy.
	Restart string `json:"restart,omitempty"`
	// ChainFrom is the upstream task ID whose completion fires this one, and
	// ChainOn the completion status that does it.
	ChainFrom string `json:"chain_from,omitempty"`
	ChainOn   string `json:"chain_on,omitempty"`
	// ChainParams names the inputs the edge forwards downstream. The values
	// are author-written literals, so only the names render.
	ChainParams []string `json:"chain_params,omitempty"`
}

// Permissions is the resolved, post-override capability set — what the task
// may reach if armed. Every field here is policy rather than value, so all of
// it resolves and all of it renders.
type Permissions struct {
	Net            []string                `json:"net,omitempty"`
	FS             []FSAccess              `json:"fs,omitempty"`
	Run            []string                `json:"run,omitempty"`
	Sys            []string                `json:"sys,omitempty"`
	EnvReadExposed bool                    `json:"env_read_exposed,omitempty"`
	Dicode         *task.DicodePermissions `json:"dicode,omitempty"`
}

// FSAccess is one filesystem grant. task.FSEntry carries yaml tags only, so it
// would marshal by Go field name; this surface is consumed as JSON, so the
// grant gets field names the renderer can read.
type FSAccess struct {
	Path       string `json:"path"`
	Permission string `json:"permission"`
}

// EnvDecl is one environment variable declaration: the name the task sees and
// where its value comes from. Never the value.
type EnvDecl struct {
	Name string `json:"name"`
	Kind string `json:"kind"`
	// Ref is the host env var name, secrets-store key, or provider task ID the
	// declaration points at. Empty for a literal, whose value never renders.
	Ref string `json:"ref,omitempty"`
	// HasDefault reports that a literal fallback is configured for a missing
	// secret. The fallback itself never renders.
	HasDefault bool   `json:"has_default,omitempty"`
	Optional   bool   `json:"optional,omitempty"`
	IfMissing  string `json:"if_missing,omitempty"`
}

// ParamDecl is one program input. HasDefault reports that a default is
// configured without rendering it: a default is author-written literal
// content, which this surface does not display (ADR-0003).
type ParamDecl struct {
	Name        string `json:"name"`
	Type        string `json:"type,omitempty"`
	Description string `json:"description,omitempty"`
	Required    bool   `json:"required,omitempty"`
	HasDefault  bool   `json:"has_default,omitempty"`
}

// Container is the resolved docker/podman configuration. These fields decide
// what the container may reach independently of Permissions — network_mode
// overrides permissions.net outright — so they belong on the review surface.
type Container struct {
	Volumes     []string `json:"volumes,omitempty"`
	Ports       []string `json:"ports,omitempty"`
	ExtraHosts  []string `json:"extra_hosts,omitempty"`
	CapAdd      []string `json:"cap_add,omitempty"`
	CapDrop     []string `json:"cap_drop,omitempty"`
	SecurityOpt []string `json:"security_opt,omitempty"`
	User        string   `json:"user,omitempty"`
	ReadOnly    bool     `json:"read_only,omitempty"`
	PullPolicy  string   `json:"pull_policy,omitempty"`
	Entrypoint  []string `json:"entrypoint,omitempty"`
	Command     []string `json:"command,omitempty"`
	// EnvNames lists the names of literal container env vars. docker.env_vars
	// holds values written inline in task.yaml, so only the names render.
	EnvNames []string `json:"env_names,omitempty"`
}

// Stage is one step of a pipeline.
type Stage struct {
	Task      string   `json:"task"`
	DependsOn []string `json:"depends_on,omitempty"`
	// Overridden reports that this stage patches the target task's spec before
	// dispatch, so the operator knows the stage does not run the task as its
	// own review surface would show it.
	Overridden bool `json:"overridden,omitempty"`
}

// State returns the review surface for a pending task. Returns an error when
// the task is not currently pending.
func (g *Gate) State(id string) (State, error) {
	g.mu.Lock()
	ent, isPending := g.pending[id]
	g.mu.Unlock()
	if !isPending {
		return State{}, fmt.Errorf("task %q is not pending approval", id)
	}

	// ent.kinded is read outside the lock. This relies on the arm callback
	// (pkg/daemon's newArmDisarm) never mutating a spec it was handed in
	// place — its overrides (buildin/run-inputs-cleanup's retention_seconds,
	// buildin/relay-server-body's Enabled) register a copy rather than
	// writing through k, specifically so this read can never race that
	// write. That matters now that a pinned buildin task can pend (#832):
	// before, no spec reachable from the pending set was ever also live in
	// arm's hands, since Admit auto-approved BuiltinSource before the
	// pending branch ever ran; a pinned buildin can now reach both.
	kinded := ent.kinded
	if g.previewFn != nil {
		// Renders the end state Approve would actually produce, not the
		// as-shipped value Admit first observed: the same daemon-config
		// override arm applies at approval time (previewFn mirrors it)
		// would otherwise be invisible on this review surface until after
		// the operator has already approved it.
		kinded = g.previewFn(kinded)
	}

	st := State{
		TaskID:      id,
		PendingHash: ent.hash,
		Kind:        kinded.KindOf(),
		Enabled:     kinded.IsEnabled(),
	}

	switch s := kinded.(type) {
	case *task.Spec:
		stateFromSpec(&st, s)
	case *task.PipelineTask:
		stateFromPipeline(&st, s)
	}

	files, err := inventoryOf(kinded)
	if err != nil {
		// The spec-derived body is still a complete and accurate answer to
		// "what will run", so degrade rather than deny the operator a review
		// surface — but say so on the surface itself, not only in the log.
		g.log.Warn("approval: file inventory failed",
			zap.String("task", id), zap.Error(err))
		st.FilesError = err.Error()
	}
	st.Files = files
	return st, nil
}

func stateFromSpec(st *State, s *task.Spec) {
	st.Name = s.Name
	st.Description = s.Description
	st.Runtime = string(s.Runtime)
	st.MCPExposed = s.MCPExposed
	if s.Timeout > 0 {
		st.Timeout = s.Timeout.String()
	}
	st.Permissions = permissionsOf(s.Permissions)
	st.Env = envDeclsOf(s.Permissions.Env)
	st.Params = paramDeclsOf(s.Params)
	st.Triggers = triggersOfSpec(s.Trigger)
	if s.Docker != nil {
		st.Image = s.Docker.Image
		st.NetworkMode = s.Docker.NetworkMode
		st.Container = containerOf(s.Docker)
	}
}

func stateFromPipeline(st *State, p *task.PipelineTask) {
	st.Name = p.Name
	st.Description = p.Description
	if p.Timeout > 0 {
		st.Timeout = p.Timeout.String()
	}
	st.Triggers = triggersOfPipeline(p.Trigger)
	for _, stage := range p.Stages {
		st.Stages = append(st.Stages, Stage{
			Task:       stage.Task,
			DependsOn:  stage.DependsOn,
			Overridden: stage.Overrides != nil,
		})
	}
}

func permissionsOf(p task.Permissions) Permissions {
	fs := make([]FSAccess, 0, len(p.FS))
	for _, e := range p.FS {
		fs = append(fs, FSAccess{Path: e.Path, Permission: e.Permission})
	}
	if len(fs) == 0 {
		fs = nil
	}
	return Permissions{
		Net:            p.Net,
		FS:             fs,
		Run:            p.Run,
		Sys:            p.Sys,
		EnvReadExposed: p.EnvReadExposed,
		Dicode:         p.Dicode,
	}
}

// envDeclsOf classifies each entry by where its value comes from, mirroring
// EnvEntry's documented lookup rules. The reference is reported; resolving it
// is exactly what this surface must not do.
func envDeclsOf(entries []task.EnvEntry) []EnvDecl {
	var out []EnvDecl
	for _, e := range entries {
		d := EnvDecl{
			Name:       e.Name,
			HasDefault: e.Default != "",
			Optional:   e.Optional,
		}
		if e.IfMissing != nil {
			d.IfMissing = e.IfMissing.Task
		}
		switch {
		case e.Secret != "":
			d.Kind, d.Ref = EnvFromSecret, e.Secret
		case e.From != "":
			d.Kind, d.Ref = envRefOf(e.From)
		case e.Value != "":
			d.Kind = EnvLiteral
		default:
			// Bare entry: allowlisted from the host under its own name.
			d.Kind, d.Ref = EnvFromHost, e.Name
		}
		out = append(out, d)
	}
	return out
}

// envRefOf splits an EnvEntry.From into its kind and reference. A bare value
// is a host env var name, matching the loader's backwards-compatible rule.
func envRefOf(from string) (kind, ref string) {
	if rest, ok := strings.CutPrefix(from, "task:"); ok {
		return EnvFromTask, rest
	}
	if rest, ok := strings.CutPrefix(from, "env:"); ok {
		return EnvFromHost, rest
	}
	return EnvFromHost, from
}

func paramDeclsOf(params task.Params) []ParamDecl {
	var out []ParamDecl
	for _, p := range params {
		out = append(out, ParamDecl{
			Name:        p.Name,
			Type:        p.Type,
			Description: p.Description,
			Required:    p.Required,
			HasDefault:  p.Default != "",
		})
	}
	return out
}

// triggerShape is the trigger surface common to kind: Task and
// kind: PipelineTask. A pipeline has no daemon shape — it is daemon-shaped iff
// its terminal stage is — so that one is appended by the caller.
type triggerShape struct {
	cron        string
	webhook     string
	webhookAuth task.WebhookAuthMode
	secret      string
	manual      bool
	chain       *task.ChainTrigger
}

// triggersOf renders every armed trigger shape. The validator permits only
// one, but rendering whatever resolution produced keeps this honest about an
// override that set a second.
func triggersOf(t triggerShape) []Trigger {
	var out []Trigger
	if t.cron != "" {
		out = append(out, Trigger{Kind: TriggerCron, Cron: t.cron})
	}
	if t.webhook != "" {
		out = append(out, Trigger{
			Kind:    TriggerWebhook,
			Webhook: t.webhook,
			Auth:    string(t.webhookAuth),
			Signed:  task.WebhookSecretResolved(t.secret),
		})
	}
	if t.manual {
		out = append(out, Trigger{Kind: TriggerManual})
	}
	if t.chain != nil {
		out = append(out, chainTrigger(t.chain))
	}
	return out
}

func triggersOfSpec(t task.TriggerConfig) []Trigger {
	out := triggersOf(triggerShape{
		cron: t.Cron, webhook: t.Webhook, webhookAuth: t.WebhookAuth,
		secret: t.WebhookSecret, manual: t.Manual, chain: t.Chain,
	})
	if t.Daemon {
		out = append(out, Trigger{Kind: TriggerDaemon, Restart: t.Restart})
	}
	return out
}

func triggersOfPipeline(t task.PipelineTrigger) []Trigger {
	return triggersOf(triggerShape{
		cron: t.Cron, webhook: t.Webhook, webhookAuth: t.WebhookAuth,
		secret: t.WebhookSecret, manual: t.Manual, chain: t.Chain,
	})
}

// chainTrigger renders a chain edge. Chain params are forwarded into the
// downstream task's input as author-written literals, so their names render
// and their values do not.
func chainTrigger(c *task.ChainTrigger) Trigger {
	t := Trigger{Kind: TriggerChain, ChainFrom: c.From, ChainOn: c.On}
	for name := range c.Params {
		t.ChainParams = append(t.ChainParams, name)
	}
	sort.Strings(t.ChainParams)
	return t
}

func containerOf(d *task.DockerConfig) *Container {
	c := &Container{
		Volumes:     d.Volumes,
		Ports:       d.Ports,
		ExtraHosts:  d.ExtraHosts,
		CapAdd:      d.CapAdd,
		CapDrop:     d.CapDrop,
		SecurityOpt: d.SecurityOpt,
		User:        d.User,
		ReadOnly:    d.ReadOnly,
		PullPolicy:  d.PullPolicy,
		Entrypoint:  d.Entrypoint,
		Command:     d.Command,
	}
	for name := range d.EnvVars {
		c.EnvNames = append(c.EnvNames, name)
	}
	sort.Strings(c.EnvNames)
	return c
}

// inventoryOf lists the files constituting k, or nothing for a dir-less
// (inline taskset) task, which has no directory to inventory.
func inventoryOf(k task.Kinded) ([]task.FileMeta, error) {
	var dir string
	var includes []string
	switch s := k.(type) {
	case *task.Spec:
		dir, includes = s.TaskDir, s.HashInclude
	case *task.PipelineTask:
		dir = s.TaskDir
	}
	if dir == "" {
		return nil, nil
	}
	return task.Inventory(dir, includes...)
}
