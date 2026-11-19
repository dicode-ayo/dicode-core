package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"

	"go.uber.org/zap"

	"github.com/dicode/dicode/internal/gitops"
	"github.com/dicode/dicode/pkg/config"
	"github.com/dicode/dicode/pkg/task"
	"github.com/dicode/dicode/pkg/taskset"
)

const taskSearchUsage = `usage: dicode task search <query...> [--taskset <git-url-or-path>[@ref]]... [--config PATH] [--json]`

const taskSearchHelp = taskSearchUsage + `

Searches TaskSets for tasks whose id, name, or description contain every
query word (case-insensitive). Without --taskset, searches the TaskSets
configured in dicode.yaml. Needs no running daemon.

Flags:
  --taskset <src>[@ref]  search this TaskSet instead of the configured ones
                         (repeatable). <src> is an http(s)/ssh git URL or a
                         local path; @ref names a branch or tag (branch tried
                         first, default branch "main")
  --config PATH          config to read sources from (default dicode.yaml)
  --json                 print a JSON array instead of columns`

// remoteSchemeAllowed gates which git URLs --taskset may clone. Tests widen it
// to file:// fixtures.
var remoteSchemeAllowed = taskset.IsAllowedRefScheme

// searchResult is one matching task. The JSON keys are a stable interface:
// every key is always present, empty when unknown.
type searchResult struct {
	Source      string `json:"source"`
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description"`
	Location    string `json:"location"`
	Ref         string `json:"ref"`
	Commit      string `json:"commit"`
}

// searchTarget is one TaskSet to resolve. Location is display-safe.
type searchTarget struct {
	label     string
	location  string
	ref       *taskset.Ref
	overrides *taskset.Overrides
	// refFallback, when set, is retried as a tag after a branch lookup finds
	// no such ref on the remote.
	refFallback string
}

func cmdTaskSearch(args []string) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	return runTaskSearch(ctx, args, os.Stdout, os.Stderr)
}

func runTaskSearch(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	var (
		tasksets   []string
		configPath = defaultConfigPath
		asJSON     bool
		terms      []string
	)
	parseFlags := true
	for i := 0; i < len(args); i++ {
		a := args[i]
		if parseFlags && a == "--" {
			parseFlags = false
			continue
		}
		if !parseFlags || !strings.HasPrefix(a, "-") {
			terms = append(terms, strings.Fields(a)...)
			continue
		}
		switch {
		case a == "--help" || a == "-h":
			fmt.Fprintln(stderr, taskSearchHelp)
			return nil
		case a == "--json":
			asJSON = true
		case a == "--taskset" || a == "--config":
			if i+1 >= len(args) || args[i+1] == "" {
				return fmt.Errorf("%s requires a value\n%s", a, taskSearchUsage)
			}
			if a == "--taskset" {
				tasksets = append(tasksets, args[i+1])
			} else {
				configPath = args[i+1]
			}
			i++
		case strings.HasPrefix(a, "--taskset="):
			v := strings.TrimPrefix(a, "--taskset=")
			if v == "" {
				return fmt.Errorf("--taskset requires a value\n%s", taskSearchUsage)
			}
			tasksets = append(tasksets, v)
		case strings.HasPrefix(a, "--config="):
			configPath = strings.TrimPrefix(a, "--config=")
		default:
			return fmt.Errorf("unknown flag %q\n%s", a, taskSearchUsage)
		}
	}
	if len(terms) == 0 {
		return errors.New(taskSearchUsage)
	}

	var targets []searchTarget
	var err error
	if len(tasksets) > 0 {
		targets, err = explicitSearchTargets(tasksets)
	} else {
		targets, err = configuredSearchTargets(configPath)
	}
	if err != nil {
		return err
	}

	cacheDir, err := os.MkdirTemp("", "dicode-task-search-")
	if err != nil {
		return fmt.Errorf("create temp dir: %w", err)
	}
	defer os.RemoveAll(cacheDir)

	results := []searchResult{}
	failed := 0
	for _, t := range targets {
		found, warnings, err := searchTaskSet(ctx, cacheDir, t, terms)
		for _, w := range warnings {
			fmt.Fprintf(stderr, "dicode: warning: %s\n", gitops.StripURLCredentials(w))
		}
		if err != nil {
			failed++
			fmt.Fprintf(stderr, "dicode: %s: %s\n", t.location, gitops.StripURLCredentials(err.Error()))
			continue
		}
		results = append(results, found...)
	}
	sort.SliceStable(results, func(i, j int) bool { return results[i].ID < results[j].ID })

	if asJSON {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(results); err != nil {
			return err
		}
	} else if len(results) == 0 {
		fmt.Fprintf(stderr, "no tasks match %q\n", strings.Join(terms, " "))
	} else {
		fmt.Fprintf(stdout, "%-40s %-30s %s\n", "ID", "NAME", "DESCRIPTION")
		for _, r := range results {
			fmt.Fprintf(stdout, "%-40s %-30s %s\n", r.ID, orDash(r.Name), orDash(oneLine(r.Description, 60)))
		}
	}

	if failed > 0 {
		return fmt.Errorf("%d of %d TaskSets could not be searched", failed, len(targets))
	}
	return nil
}

// configuredSearchTargets lists the TaskSets dicode.yaml configures. Like
// cliDataDirFor, it only reads a config the calling user owns.
func configuredSearchTargets(configPath string) ([]searchTarget, error) {
	fi, err := os.Lstat(configPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("no config at %s — pass --config PATH, or --taskset to search a TaskSet directly", configPath)
		}
		return nil, err
	}
	if !fi.Mode().IsRegular() || !ownedByCurrentUser(fi) {
		return nil, fmt.Errorf("refusing to read %s: not a regular file owned by the current user", configPath)
	}
	cfg, err := config.Load(configPath)
	if err != nil {
		return nil, errors.New(gitops.StripURLCredentials(err.Error()))
	}
	allowlist, err := cfg.SourceSecurity.Allowlist()
	if err != nil {
		return nil, err
	}
	gitops.SetInternalHostAllowlist(allowlist)

	names := make([]string, 0, len(cfg.Spec.Entries))
	for name, e := range cfg.Spec.Entries {
		if e != nil && e.Ref != nil {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	targets := make([]searchTarget, 0, len(names))
	for _, name := range names {
		e := cfg.Spec.Entries[name]
		loc := e.Ref.Path
		if e.Ref.IsGit() {
			loc = gitops.StripURLCredentials(e.Ref.URL)
		}
		targets = append(targets, searchTarget{label: name, location: loc, ref: e.Ref, overrides: e.Overrides})
	}
	return targets, nil
}

// explicitSearchTargets turns --taskset values into targets. A value is a git
// remote when it has a scheme or is SCP shorthand; anything else is a local
// path, which takes no @ref.
func explicitSearchTargets(values []string) ([]searchTarget, error) {
	targets := make([]searchTarget, 0, len(values))
	for _, v := range values {
		if !strings.Contains(v, "://") && !taskset.IsAllowedRefScheme(v) {
			abs, err := filepath.Abs(v)
			if err != nil {
				return nil, err
			}
			if _, err := os.Stat(abs); err != nil {
				return nil, fmt.Errorf("--taskset %s: %w", v, err)
			}
			targets = append(targets, searchTarget{
				label:    tasksetLabel(abs),
				location: abs,
				ref:      &taskset.Ref{Path: abs},
			})
			continue
		}
		url, refName := splitRefSuffix(v)
		safe := gitops.StripURLCredentials(url)
		if !remoteSchemeAllowed(url) {
			return nil, fmt.Errorf("--taskset %s: git URL must use http, https, or ssh", safe)
		}
		ref := &taskset.Ref{URL: url}
		if refName != "" {
			ref.Branch = refName
		}
		targets = append(targets, searchTarget{
			label:       tasksetLabel(url),
			location:    safe,
			ref:         ref,
			refFallback: refName,
		})
	}
	return targets, nil
}

// splitRefSuffix splits a trailing @ref off a git URL. Only an @ after the
// host counts, so userinfo ("https://user@host/…") and the SCP user
// ("git@host:…") are never mistaken for a ref.
func splitRefSuffix(raw string) (url, ref string) {
	start := 0
	if i := strings.Index(raw, "://"); i >= 0 {
		slash := strings.Index(raw[i+3:], "/")
		if slash < 0 {
			return raw, ""
		}
		start = i + 3 + slash
	} else if colon := strings.Index(raw, ":"); colon >= 0 {
		start = colon
	}
	at := strings.LastIndex(raw[start:], "@")
	if at < 0 {
		return raw, ""
	}
	return raw[:start+at], raw[start+at+1:]
}

// tasksetLabel names a --taskset by the last element of its URL or path.
func tasksetLabel(loc string) string {
	loc = strings.TrimRight(gitops.StripURLCredentials(loc), "/")
	base := loc[strings.LastIndexAny(loc, "/:\\")+1:]
	for _, suffix := range []string{".git", ".yaml", ".yml"} {
		base = strings.TrimSuffix(base, suffix)
	}
	if base == "taskset" {
		return tasksetLabel(filepath.Dir(loc))
	}
	if base == "" {
		return "taskset"
	}
	return base
}

func searchTaskSet(ctx context.Context, cacheDir string, t searchTarget, terms []string) ([]searchResult, []string, error) {
	r := taskset.NewResolver(cacheDir, false, zap.NewNop())
	ref := t.ref
	resolved, failures, err := r.Resolve(ctx, t.label, ref, nil, t.overrides, nil)
	if err != nil && t.refFallback != "" && errors.Is(err, gitops.ErrRefNotFound) {
		ref = &taskset.Ref{URL: t.ref.URL, Tag: t.refFallback}
		if verr := taskset.ValidateRefTarget(ref); verr != nil {
			return nil, nil, err
		}
		resolved, failures, err = r.Resolve(ctx, t.label, ref, nil, t.overrides, nil)
	}
	if err != nil {
		return nil, nil, err
	}

	var warnings []string
	for _, f := range failures {
		warnings = append(warnings, fmt.Sprintf("%s: %v", f.ID, f.Error))
	}
	sort.Strings(warnings)

	var out []searchResult
	for _, rt := range resolved {
		name, desc := taskNameDescription(rt.Kinded)
		if !matchesAll(terms, rt.ID, name, desc) {
			continue
		}
		res := searchResult{
			Source:      t.label,
			ID:          rt.ID,
			Name:        name,
			Description: desc,
			Location:    t.location,
		}
		if ref.IsGit() {
			res.Ref = ref.TrackedName()
			if commit, _, err := gitops.HeadInfo(rt.TaskDir); err == nil {
				res.Commit = commit
			}
		}
		out = append(out, res)
	}
	return out, warnings, nil
}

func taskNameDescription(k task.Kinded) (string, string) {
	switch s := k.(type) {
	case *task.Spec:
		return s.Name, s.Description
	case *task.PipelineTask:
		return s.Name, s.Description
	}
	return "", ""
}

// matchesAll reports whether every term occurs, case-insensitively, in at
// least one of fields.
func matchesAll(terms []string, fields ...string) bool {
	hay := strings.ToLower(strings.Join(fields, "\n"))
	for _, t := range terms {
		if !strings.Contains(hay, strings.ToLower(t)) {
			return false
		}
	}
	return true
}

func oneLine(s string, max int) string {
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > max {
		return string(r[:max-1]) + "…"
	}
	return s
}
