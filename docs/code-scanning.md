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
- Certifying a function as a sanitizer records a judgement in machine-readable form, and
  the judgement can be wrong in ways nothing re-checks. `safeTaskFilePath` was
  symlink-blind at the time it was proposed as a barrier — the model would have asserted
  containment the function did not provide.

Fixing the code at the boundary — see the triage below — removes alerts without any of
this. It is the better tool wherever it applies.

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

`go/path-injection` reported 15 results. Five are **fixed** by the changes alongside this
document; the remaining ten are one finding, accepted by design and dismissed in the
Security tab with a written reason.

### Fixed

| Was | Fix |
|---|---|
| `pkg/webui/server.go:1047`, `:1077` | `canonicalTaskFile` returns the entry from `allowedFiles` rather than the request's copy, so no request-derived string reaches `filepath.Join`. |
| `internal/installer/installer.go:92`, `internal/fsutil/fsutil.go:17`, `:45` | The runtime-install route now validates `version` against `runtimeVersionPattern` before it becomes a cache path *and* a release download URL. `pathguard.Within` in `EnsureBinary` remains as the second layer. |

Both are boundary fixes: the value is constrained where it enters, not audited at each
of the sinks it reaches. That is also why they close several alerts each.

`internal/fsutil/fsutil.go:56` still reports, but by a different route — it is reached
from `resolveYAMLPath`, not the installer — so it belongs to the group below.

### Accepted: operator-supplied source paths

Ten results, one taint: `LocalPath` on the add-source request body
(`pkg/webui/sources.go:321`), which becomes a taskset root and then a task directory,
reaching `pkg/task/hash.go:299`, `:315`, `:343`, `:353`, `pkg/taskset/resolver.go:484`,
`internal/fsutil/fsutil.go:56`, `pkg/taskset/loader.go:31`, `:49`,
`pkg/task/spec.go:692` and `pkg/task/pipeline.go:89`.

Naming a host directory *is* the local-source feature; there is no root to confine it to
without removing the feature. Whoever can set it can already ship task code that runs on
the host, so reading a YAML file or hashing a directory is strictly less power. This is
the trusted-author model in [ADR-0002](./adr/0002-trust-boundary-is-the-merge.md).
"The author is trusted" is a claim about the *merge* boundary — not the same claim as
"this string is not remote input" — and `threat_model: remote` is right to flag it.

The four `hash.go` results are the ones #699 relocated, and they belong here. Their
earlier dismissals cited `resolveInclude`'s containment, which bounds the `hash_include`
value; the taint in every one of these flows is `absDir` instead.

Three results outside `go/path-injection` are also accepted and dismissed with reasons:
`go/cookie-secure-not-set` on `pkg/webui/sessions_db.go:377` and `:391`, and
`go/weak-sensitive-data-hashing` on `pkg/webui/apikeys.go:258`.

That is what makes a red check meaningful: anything not on this list is something new.

### Also fixed: the task-file editor followed symlinks

Never a CodeQL finding — `go/path-injection` does not model symlink following — but it
surfaced while checking whether `safeTaskFilePath` could honestly be certified as a
sanitizer, and it was the most serious thing found.

`safeTaskFilePath` did a purely lexical `Rel` check and never stat'd the candidate. A
source repo that commits `tasks/foo/style.css` as a symlink gets it materialized as a
real on-disk link by go-git; `style.css` is in `allowedFiles` but is not a script
candidate, so `Spec.ScriptPath`'s symlink rejection never applied and the task registered
normally. `GET /api/tasks/{id}/files/style.css` then read the target, and `POST` wrote
attacker content to it as the daemon user.

It now canonicalizes with `pathguard.WithinResolved`, matching `pkg/webui/task_delete.go`,
`taskset.containedPath` and `resolveInclude`. A link pointing at a sibling inside the task
dir stays editable, because that is contained; one resolving outside is rejected, as is an
escape through a symlinked intermediate directory.

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
