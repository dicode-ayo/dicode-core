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
	"unicode"

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
                         first); without @ref, "main", else the remote's
                         default branch
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
	// remoteDefault retries a missing main branch on the remote's HEAD branch.
	remoteDefault bool
	// untrusted strips every token_env declared inside the TaskSet.
	untrusted        bool
	allowedTokenEnvs []string
	// err, when set, is reported as this target's failure instead of
	// resolving it.
	err error
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
			if i+1 >= len(args) || args[i+1] == "" || strings.HasPrefix(args[i+1], "-") {
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
			if configPath == "" {
				return fmt.Errorf("--config requires a value\n%s", taskSearchUsage)
			}
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
			fmt.Fprintf(stderr, "dicode: warning: %s\n", cleanText(gitops.StripURLCredentials(w)))
		}
		if err != nil {
			failed++
			fmt.Fprintf(stderr, "dicode: %s: %s\n", cleanText(t.location), cleanText(gitops.StripURLCredentials(err.Error())))
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
		if _, err := fmt.Fprintf(stdout, "%-40s %-30s %s\n", "ID", "NAME", "DESCRIPTION"); err != nil {
			return err
		}
		for _, r := range results {
			if _, err := fmt.Fprintf(stdout, "%-40s %-30s %s\n", cleanText(r.ID), orDash(cleanText(r.Name)), orDash(oneLine(r.Description, 60))); err != nil {
				return err
			}
		}
	}

	if failed > 0 {
		return fmt.Errorf("%d of %d TaskSets could not be searched", failed, len(targets))
	}
	return nil
}

// configuredSearchTargets lists the TaskSets dicode.yaml configures. It only
// reads a config whose owner is exactly the calling user, root included: the
// config picks which env tokens go to which remotes.
func configuredSearchTargets(configPath string) ([]searchTarget, error) {
	fi, err := os.Lstat(configPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("no config at %s — pass --config PATH, or --taskset to search a TaskSet directly", configPath)
		}
		return nil, err
	}
	if !fi.Mode().IsRegular() || !configOwnedBy(fi, os.Getuid()) {
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
		var loc string
		if e.Ref.IsGit() {
			loc = gitops.StripURLCredentials(e.Ref.URL)
		} else if loc, err = filepath.Abs(e.Ref.Path); err != nil {
			return nil, err
		}
		targets = append(targets, searchTarget{
			label:            name,
			location:         loc,
			ref:              e.Ref,
			overrides:        e.Overrides,
			allowedTokenEnvs: cfg.SourceSecurity.AllowedTokenEnvs,
		})
	}
	return targets, nil
}

// explicitSearchTargets turns --taskset values into targets. A value is a git
// remote when it has a scheme or is SCP shorthand; anything else is a local
// path, which takes no @ref. Every --taskset is untrusted, local paths
// included: credentials named by its own entries are never used. Labels that
// share a basename get -2, -3, ... in flag order.
func explicitSearchTargets(values []string) ([]searchTarget, error) {
	targets, err := parseExplicitTargets(values)
	if err != nil {
		return nil, err
	}
	seen := map[string]int{}
	for i := range targets {
		base := targets[i].label
		seen[base]++
		if n := seen[base]; n > 1 {
			label := fmt.Sprintf("%s-%d", base, n)
			for seen[label] > 0 {
				n++
				label = fmt.Sprintf("%s-%d", base, n)
			}
			seen[base] = n
			seen[label]++
			targets[i].label = label
		}
		targets[i].untrusted = true
	}
	return targets, nil
}

func parseExplicitTargets(values []string) ([]searchTarget, error) {
	targets := make([]searchTarget, 0, len(values))
	for _, v := range values {
		if !isRemoteTaskSet(v) {
			abs, err := filepath.Abs(v)
			if err != nil {
				return nil, err
			}
			t := searchTarget{
				label:    tasksetLabel(abs),
				location: abs,
				ref:      &taskset.Ref{Path: abs},
			}
			if _, err := os.Stat(abs); err != nil {
				t.err = err
			}
			targets = append(targets, t)
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
			label:         tasksetLabel(url),
			location:      safe,
			ref:           ref,
			refFallback:   refName,
			remoteDefault: refName == "",
		})
	}
	return targets, nil
}

// isRemoteTaskSet reports whether a --taskset value names a git remote: it
// has a scheme, or remoteSchemeAllowed accepts it (SCP shorthand).
func isRemoteTaskSet(v string) bool {
	return strings.Contains(v, "://") || remoteSchemeAllowed(v)
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

// tasksetLabel names a --taskset by the last element of its URL or path, or
// a remote's host when its path names nothing better.
func tasksetLabel(loc string) string {
	loc = gitops.StripURLCredentials(loc)
	if host, path, ok := gitops.ParseRemote(loc); ok {
		if l := pathLabel(path); l != "taskset" {
			return l
		}
		return host
	}
	return pathLabel(loc)
}

func pathLabel(loc string) string {
	loc = strings.TrimRight(loc, "/\\")
	i := strings.LastIndexAny(loc, "/\\")
	base := loc[i+1:]
	for _, suffix := range []string{".git", ".yaml", ".yml"} {
		base = strings.TrimSuffix(base, suffix)
	}
	if base == "taskset" && i > 0 {
		return pathLabel(loc[:i])
	}
	if base == "" || base == "." || base == ".." {
		return "taskset"
	}
	return base
}

func searchTaskSet(ctx context.Context, cacheDir string, t searchTarget, terms []string) ([]searchResult, []string, error) {
	if t.err != nil {
		return nil, nil, t.err
	}
	r := taskset.NewResolver(cacheDir, false, zap.NewNop())
	r.SetAllowedTokenEnvs(t.allowedTokenEnvs)
	r.SetUntrustedRoot(t.untrusted)
	ref := t.ref
	var commit string
	if ref.IsGit() {
		// Pulling first fills the resolver's clone cache, so Resolve reuses
		// this clone and commit names the root TaskSet's checkout.
		// Explicit refs never carry Auth, so the trusted clone partition this
		// fills holds only tokenless clones.
		dir, err := r.Pull(ctx, ref)
		if err != nil && t.remoteDefault && errors.Is(err, gitops.ErrRefNotFound) {
			branch, derr := gitops.DefaultBranch(ctx, ref.URL)
			if derr == nil && branch != ref.TrackedName() {
				headRef := &taskset.Ref{URL: ref.URL, Branch: branch}
				if taskset.ValidateBranchName(branch, "") == nil && taskset.ValidateRefTarget(headRef) == nil {
					ref = headRef
					dir, err = r.Pull(ctx, ref)
				}
			}
		}
		if err != nil && t.refFallback != "" && errors.Is(err, gitops.ErrRefNotFound) {
			tagRef := &taskset.Ref{URL: t.ref.URL, Tag: t.refFallback}
			if verr := taskset.ValidateRefTarget(tagRef); verr != nil {
				return nil, nil, err
			}
			ref = tagRef
			dir, err = r.Pull(ctx, ref)
		}
		if err != nil {
			return nil, nil, err
		}
		if c, _, herr := gitops.HeadInfo(dir); herr == nil {
			commit = c
		}
	}
	resolved, failures, err := r.Resolve(ctx, t.label, ref, nil, t.overrides, nil)
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
			res.Commit = commit
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

// cleanText makes untrusted text safe for a terminal: control characters
// (C0, DEL and C1, so ESC, BEL, CR and newlines too) become spaces and
// whitespace runs collapse to one space.
func cleanText(s string) string {
	s = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, s)
	return strings.Join(strings.Fields(s), " ")
}

func oneLine(s string, max int) string {
	s = cleanText(s)
	if r := []rune(s); len(r) > max {
		return string(r[:max-1]) + "…"
	}
	return s
}

// isNoDaemonTaskSearch reports whether args (os.Args[1:]) invoke
// `task search`, which main runs before dialing the daemon.
func isNoDaemonTaskSearch(args []string) bool {
	return len(args) > 1 && args[0] == "task" && args[1] == "search"
}
