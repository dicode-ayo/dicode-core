# Code scanning (CodeQL)

How CodeQL is configured for this repository, how to reproduce its results on your
own machine, and the standing triage for every alert that is deliberately left open.

## What runs

Scanning uses GitHub's **default setup** — there is no `codeql.yml` in
`.github/workflows/`. As reported by `/code-scanning/default-setup`:

```yaml
query_suite:  default
threat_model: remote
schedule:     weekly
languages:    actions, go, javascript, javascript-typescript, python, typescript
```

`schedule: weekly` is the *additional* scheduled scan. It is not the only one: `main` is
analysed on every push, which `/code-scanning/analyses?ref=refs/heads/main` shows
directly — one analysis per commit SHA, several a day.

Two consequences follow, and both surprise people:

**Pull requests fail on alerts that are new relative to the `main` baseline** — not on
the absolute alert count. A PR can be red while introducing no new vulnerability, which
is why the triage below matters more than the alert count does.

**A PR's analysis is diff-informed**, so it is not a way to test an analysis change. A
PR that edits no Go files reports `results=0` for Go regardless of what the query would
find on the full tree. To see the effect of a model or query change, either run it
locally as below, or read the post-merge analysis on `main`.

## Why relocating code turns the check red

CodeQL keys an alert to the *function containing the sink*. Moving a flagged read into
a new enclosing function closes the old alert and opens a new one, even when no
boundary check changed.

PR #699 hit this: it extracted `collectEntries` and `writeFileContribution` out of
`task.Hash` without adding, removing or altering a single check, and CodeQL reported
two new high-severity vulnerabilities. The reviewer then has to reconstruct the safety
argument from nothing, because no triage was recorded against the old alert.

## Why models-as-data does not fix it

The obvious answer is to declare this repo's path sanitizers to CodeQL as
[models-as-data](https://docs.github.com/en/code-security/tutorials/customize-code-scanning/creating-and-working-with-codeql-packs)
barriers, so the query stops reporting bounded reads wherever they live. That was tried,
measured, and abandoned. **Do not retry it without reading this section** — it works, in
the sense that the alerts disappear, which is exactly what makes it dangerous.

A `barrierModel` row silences the four `pkg/task/hash.go` reads and the two in
`pkg/webui/server.go`:

```yaml
- ["github.com/dicode/dicode/pkg/task", "", False, "resolveInclude", "", "", "ReturnValue[0]", "path-injection", "manual"]
```

The justification looks airtight: `resolveInclude` returns only after a lexical
strict-descendant check *and* a symlink-canonicalized containment check, so the path it
returns is bounded. It is also wrong, and the SARIF says so. Every one of the four
suppressed flows enters through **`absDir`**, not through the `hash_include` value:

```
pkg/webui/sources.go:321  selection of Body        (add-source JSON → LocalPath)
  → pkg/taskset/source.go  devRootPath → resolver.go:445 resolveYAMLPath
  → pkg/task/hash.go:147   SSA def(absDir) → :148 Join/Clean → :178 candidate
  → pathguard.ResolveExisting → :189 realCandidate      ← barrier applied here
  → :299 os.ReadFile   :315 os.Stat   :343 os.Lstat   :353 os.Readlink
```

`resolveInclude` bounds the include *relative to* `boundary`, and
`boundary = filepath.Dir(absDir)`. When `absDir` is the tainted value, the containment
check is relative to the attacker's own directory and constrains nothing absolute. The
model would suppress four filesystem sinks under a guarantee that covers none of the
flows reaching them — permanently, and silently, including for any future path by which
a less-trusted value reaches `Hash`.

This generalizes: **`barrierModel` is unconditional over the returned value, while every
containment helper in this repo is relative to a caller-supplied root.** The two do not
compose. A model would only be honest for a sanitizer that resolves against a root the
caller cannot choose.

`barrierGuardModel`, the conditional form, cannot be used either. Go's implementation in
`semmle/go/dataflow/ExternalFlow.qll` requires the guard to be the boolean value of the
call expression itself:

```ql
g.asExpr().(CallExpr).getAnArgument() = e
```

`pathguard.Within`, `pathguard.WithinResolved` and `taskset.containedPath` all return
`(bool, error)` or `error` and are tested through a destructured variable, so none match.
Independently, `convertAcceptingValue` implements only `"true"` and `"false"` — `"null"`,
the shape an error-returning guard needs, is commented out as *"not supported yet"*. Rows
for all three were written and measured; they silenced nothing.

Two further reasons not to reach for this, if the above is ever fixed upstream:

- GitHub documents model packs in default setup for C/C++, C#, Java/Kotlin, Python, Ruby
  and Rust. **Go is absent from that list**, so a pack that works locally may be inert in
  CI — which would leave the suppression dormant rather than absent, ready to activate
  silently if Go is added or the repo moves to advanced setup.
- `safeTaskFilePath` is symlink-blind. It does a purely lexical `Rel` check and never
  stats, so a link committed at `taskDir/style.css` is followed by the `os.ReadFile` and
  `os.WriteFile` in `apiGetFile`/`apiSaveFile`. Certifying it to CodeQL as a path
  sanitizer would record, in machine-readable form, the opposite of the decision this
  repo makes everywhere else — `pkg/webui/task_delete.go`, `taskset.containedPath` and
  `Spec.ScriptPath` all treat git-materialized symlinks as in scope. See the open item
  below.

So the relocation churn is not solved here. It is explained, and the alerts it churns
are triaged below with reasons that survive being re-read.

## Reproducing the results locally

Worth doing: it takes a few minutes, and reading a result's taint path is the only
reliable way to check whether a suspected false positive really is one. It is what
showed that the `hash.go` alerts are directory-taint rather than include-taint.

Local results match CI's: the full Go suite yields 18 results, which is what an analysis
of `main` reports. If a local run disagrees, the bundle version or the query suite has
drifted from what Actions runs.

Install the bundle — use the **bundle**, not a bare CLI, so the query and extractor
versions match what Actions runs:

```bash
mkdir -p ~/.cache/codeql && cd ~/.cache/codeql
gh release download codeql-bundle-v2.26.3 --repo github/codeql-action \
  --pattern 'codeql-bundle-linux64.tar.zst'
tar --zstd -xf codeql-bundle-linux64.tar.zst
export PATH="$HOME/.cache/codeql/codeql:$PATH"
```

Build a database (~1 min; re-build after any source change):

```bash
codeql database create /tmp/cq-db-go --language=go --source-root=. --overwrite
```

Run one query:

```bash
codeql database analyze /tmp/cq-db-go \
  codeql/go-queries:Security/CWE-022/TaintedPath.ql \
  --format=sarif-latest --output=/tmp/tp.sarif --threads=4 --rerun
```

Run the whole suite that default setup's `default` query suite maps to, by swapping the
query for `codeql/go-queries:codeql-suites/go-code-scanning.qls`.

Read the results as `file:line`:

```bash
jq -r '.runs[0].results[]
  | "\(.locations[0].physicalLocation.artifactLocation.uri):\(.locations[0].physicalLocation.region.startLine)"' \
  /tmp/tp.sarif | sort
```

To see *why* a result survives, print its taint path — this is what shows you the
guard CodeQL did not accept, or the branch on which no guard runs:

```bash
jq -r '.runs[0].results[]
  | select(.locations[0].physicalLocation.artifactLocation.uri=="pkg/taskset/resolver.go")
  | .codeFlows[0].threadFlows[0].locations[]
  | "\(.location.physicalLocation.artifactLocation.uri):\(.location.physicalLocation.region.startLine)  \(.location.message.text)"' \
  /tmp/tp.sarif
```

A model pack can be applied with `--model-packs=<name> --additional-packs=<dir>`, and
diffing the two `file:line` lists shows exactly what it did. Measure before committing
one: a row that silences nothing is dead config, and — worse — a row that silences
something for a reason that does not apply to it looks identical to success. Read the
taint paths of everything a row removes.

## Standing triage

The full `go-code-scanning` suite reports **18** results: **15** `go/path-injection`,
two `go/cookie-secure-not-set` (`pkg/webui/sessions_db.go:377`, `:391`) and one
`go/weak-sensitive-data-hashing` (`pkg/webui/apikeys.go:258`). All eighteen are unfixed,
deliberately, and each is dismissed in the Security tab with a written reason. That is
what makes a red check meaningful: anything not on this list is something new.

The fifteen `go/path-injection` results fall into three groups.

**Operator-supplied source paths** — ten results: `pkg/taskset/loader.go:31`, `:49`,
`pkg/task/spec.go:692`, `pkg/task/pipeline.go:89`, `pkg/taskset/resolver.go:484`, and
`pkg/task/hash.go:299`, `:315`, `:343`, `:353`, plus `pkg/webui/server.go:1047` and
`:1077` by a different route (see below).

The first nine all carry the same taint: `LocalPath` from the add-source request body
(`pkg/webui/sources.go:321`), which becomes a taskset root and then a task directory.
Whoever can set it can already ship task code that runs on the host, so reading a YAML
file or hashing a directory is strictly less power. This is the trusted-author model in
[ADR-0002](./adr/0002-trust-boundary-is-the-merge.md). "The author is trusted" is a claim
about the *merge* boundary — not the same claim as "this string is not remote input" —
and `threat_model: remote` is right to flag them.

The four `hash.go` results are the ones #699 relocated, and they belong **here**, not
under "a sanitizer we did not model". Their existing dismissals cite `resolveInclude`'s
containment, which bounds the `hash_include` value; the taint in every one of these
flows is `absDir` instead. Those dismissal texts should be replaced with the reason
above.

`pkg/webui/server.go:1047`/`:1077` are the task-file editor's `os.ReadFile`/`os.WriteFile`.
Their taint is the `filename` URL parameter, which is gated on a nine-entry exact-match
allowlist and then forced to a bare `Base` name by `safeTaskFilePath`, so the request
cannot steer the path. Those two are genuine false positives — but see the symlink item
below, which is a *different* weakness at the same two sinks.

**A guard CodeQL cannot be told about** — `internal/installer/installer.go:92` and
`internal/fsutil/fsutil.go:17`, `:45`, `:56`. These four are one finding, not four.
They share a source and a guard: the `version` form value on the
runtime-install route (`pkg/webui/server.go:3084`) becomes
`~/.cache/dicode/<runtime>/<version>/<bin>` in `pkg/deno`/`pkg/uv`, and reaches
`installer.EnsureBinary`. Traversal there is genuinely reachable in principle — a
`version` of `../../..` is attacker-shaped input — and what stops it is
`pathguard.Within(cacheBase, cachePath)` at `installer.go:83`, which also rejects
equality with the base. Every one of the four sinks sits after that check:
`installer.go:92` directly, and the three `fsutil` helpers via `installer.go:88` and
`:125`. The guard dominates; it is simply not expressible as a `barrierGuardModel` for
the reasons above. (Precisely, `Within` accepts `p == root`; the `cachePath ==
filepath.Clean(cacheBase)` clause alongside it is what rejects the base itself.)

`fsutil`'s helpers are unopinionated pass-throughs and should not grow validation of
their own — containment belongs at the call site, which is where it is. The residual
weakness is that `Within` is lexical, so a symlink already planted inside the cache tree
could redirect a write; that requires prior filesystem access, at which point writing to
the binary cache is not the interesting capability.

### Open: the task-file editor follows symlinks

Not a CodeQL finding — `go/path-injection` does not model symlink following — but it
surfaced while checking whether `safeTaskFilePath` could be certified as a sanitizer, and
it is the most serious thing on this page.

`safeTaskFilePath` (`pkg/webui/server.go:988`) does a purely lexical `Rel` check and
never stats the candidate. A git source that commits `tasks/foo/style.css` as a symlink
to, say, `~/.ssh/authorized_keys` gets it materialized as a real link by go-git;
`style.css` is in `allowedFiles` but is not a script candidate, so `Spec.ScriptPath`'s
symlink rejection never applies and the task registers normally. `GET
/api/tasks/{id}/files/style.css` then reads the target, and `POST` writes attacker
content to it as the daemon user.

The route is session-gated and the author is trusted at the merge boundary, so this sits
inside ADR-0002. But the repo has decided elsewhere that this specific class *is*
defended — `pkg/webui/task_delete.go:107`, `taskset.containedPath`, `resolveInclude`, and
`Spec.ScriptPath`, which rejects a symlinked `task.ts` with the comment "Symlinks are
rejected to prevent reading files outside the task directory". The editor is the
inconsistent one.

Fix: `os.Lstat` + `ModeSymlink` rejection matching `ScriptPath`, or
`pathguard.WithinResolved`, with a case added to `pkg/webui/safetaskpath_test.go`. Note
that `resolveInclude` deliberately *supports* symlinked shared modules, so tightening the
editor needs a decision about whether legitimately symlinked task files exist.

### A dismissal that rests on a false premise

`pkg/taskset/resolver.go:484` is dismissed as a false positive on the grounds that
`resolveYAMLPath` "only ever receives a path already cleared by `containedPath`". It
does not. In `resolveRef` the non-git branch runs `containedPath` inside
`if cloneRoot != ""`, and the doc comment says *"Pass `""` to skip the containment
check"*, so there is a route to `resolveYAMLPath` on which no containment check runs.

Tracing every caller shows the alert is nonetheless correctly dismissed, for a
different reason than the one recorded. `cloneRoot` is empty in exactly two places, both
deliberate:

- `Resolve` (`resolver.go:116`) resolving the **root** taskset ref, where there is no
  enclosing directory to contain the path to — the root is what defines the boundary.
- `resolver.go:135` for **local, non-git sources**. Git sources get
  `cloneRoot = repoDir`.

Inside `resolveRef` the git branch calls `containedPath(localDir, resolved)`
unconditionally. So containment is enforced exactly where repo-authored content supplies
the path — the case where a committed symlink or `../` could escape a clone — and
skipped exactly where an operator names a host path directly through the
session-authenticated add-source route. That is the trusted-author case above, not an
unguarded traversal.

This is the rationale that holds for that alert. The text in the Security tab predates
it and states the false premise instead; treat this section as authoritative until the
two agree.
