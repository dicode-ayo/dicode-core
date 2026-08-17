# Code scanning (CodeQL)

How CodeQL is configured for this repository, how to reproduce its results on your
own machine, and the standing triage for every alert that is deliberately left open.

## What runs

Scanning uses GitHub's **default setup** — there is no `codeql.yml` in
`.github/workflows/`. As reported by `/code-scanning/default-setup`:

```
query_suite:  default
threat_model: remote
schedule:     weekly
languages:    actions, go, javascript, javascript-typescript, python, typescript
```

`schedule: weekly` is the *additional* scheduled scan. It is not the only one: `main` is
analysed on every push, which `/code-scanning/analyses?ref=refs/heads/main` shows
directly — one analysis per commit SHA, several a day. The Go analysis on `main` has
reported 18 results on every commit for weeks.

Two consequences follow, and both surprise people:

**Pull requests fail on alerts that are new relative to the `main` baseline** — not on
the absolute alert count. A PR can be red while introducing no new vulnerability, which
is the failure mode the model pack below exists to prevent.

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

That is the cost this page is written to remove.

## The model pack

`.github/codeql/extensions/dicode-path-barriers/` declares this repository's own path
sanitizers to CodeQL as [models-as-data](https://docs.github.com/en/code-security/tutorials/customize-code-scanning/creating-and-working-with-codeql-packs)
barriers, so the `go/path-injection` query stops reporting reads that are already
bounded — wherever those reads happen to live.

Two functions are modelled, both via `barrierModel` on their returned path:

| Function | Why the return value is bounded |
|---|---|
| `pkg/task.resolveInclude` | Returns only after a lexical strict-descendant check against the task's parent dir *and* a symlink-canonicalized containment check. The value returned is the canonicalized path, not the caller's lexical guess. |
| `pkg/webui.safeTaskFilePath` | Rejects any filename that is not a bare `Base` name, then re-checks containment with `filepath.Rel` after `Join`. |

A barrier model is a claim CodeQL takes on trust. What actually keeps these two
functions honest is their Go test suites — `pkg/task/hash_test.go` pins traversal
rejection, bare `..` rejection and symlink escape; `internal/pathguard/pathguard_test.go`
pins the containment primitive. Weakening either function without failing those tests
is the scenario in which the model would lie, so treat them as load-bearing.

### What is deliberately not modelled

`pathguard.Within`, `pathguard.WithinResolved` and `taskset.containedPath` are this
repo's other containment guards, and `barrierGuardModel` **cannot express any of
them**. Go's implementation in `semmle/go/dataflow/ExternalFlow.qll` requires the guard
to be the boolean value of the call expression itself:

```ql
g.asExpr().(CallExpr).getAnArgument() = e
```

Every one of these helpers returns `(bool, error)` or `error` and is tested through a
destructured variable, so none of them match. Independently, `convertAcceptingValue`
implements only `"true"` and `"false"` — `"null"`, the shape an error-returning guard
would need, is commented out as *"not supported yet"*. Rows for all three were written
and measured against a local database; they silenced nothing. Their alerts stay
dismissed in the Security tab instead.

`pathguard.Within` would not be modelled even if it could be: it is purely lexical, and
is safe only when the caller already built the path from validated segments — a
property of the call site, not of the function.

### Whether default setup honours it

GitHub documents model packs in default setup as supported for C/C++, C#, Java/Kotlin,
Python, Ruby and Rust. **Go is not on that list**, even though the Go pack itself
implements the `barrierModel` extensible predicate and honours it locally.

Because PR analyses are diff-informed, this cannot be settled on a PR that touches no
Go source. The check is the first `main` analysis after the pack merges: the Go result
count should fall from 18 to 12. If it stays at 18, default setup is ignoring the pack,
and the fix is to migrate to advanced setup — a `.github/workflows/codeql.yml` passing
the pack to `github/codeql-action/init` explicitly — rather than to change the models.

## Reproducing the results locally

Worth doing: it takes a few minutes and it is the only way to iterate on a model or
check a suspected false positive without pushing.

The local numbers match CI exactly: the unmodelled full Go suite yields 18 results
locally, which is what every `main` analysis reports. Applying the pack takes that to
12.

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

Run one query, with the model pack applied:

```bash
codeql database analyze /tmp/cq-db-go \
  codeql/go-queries:Security/CWE-022/TaintedPath.ql \
  --model-packs=dicode/path-barriers \
  --additional-packs=.github/codeql/extensions \
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

Omitting `--model-packs`/`--additional-packs` gives the unmodelled baseline, so
diffing the two `file:line` lists shows exactly what a model change did. Measure any
new model that way before committing it: a row that silences nothing is dead config,
which is how the three guard rows above were caught.

## Standing triage

The full `go-code-scanning` suite reports **12** results with the pack applied: the nine
`go/path-injection` below, plus two `go/cookie-secure-not-set`
(`pkg/webui/sessions_db.go:377`, `:391`) and one `go/weak-sensitive-data-hashing`
(`pkg/webui/apikeys.go:258`). Every one of the twelve is dismissed in the Security tab
with a written reason, so a red CodeQL check now means something new arrived.

`go/path-injection` reports **15** results unmodelled and **9** with the pack applied.
The nine are open on purpose, in three groups.

**Operator-authored config paths** — `pkg/taskset/loader.go:31`, `:49`,
`pkg/task/spec.go:692`, `pkg/task/pipeline.go:89`. These paths come from `dicode.yaml`
/ `taskset.yaml`, and the only HTTP route reaching them sits behind `requireAuth`.
Whoever can set one can already ship task code that runs on the host, so reading a YAML
file is strictly less power. This is the trusted-author model in
[ADR-0002](./adr/0002-trust-boundary-is-the-merge.md). Note that "the author is trusted"
is a claim about the *merge* boundary — it is not the same claim as "this string is not
remote input", and `threat_model: remote` is right to flag them.

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
the reasons above.

`fsutil`'s helpers are unopinionated pass-throughs and should not grow validation of
their own — containment belongs at the call site, which is where it is. The residual
weakness is that `Within` is lexical, so a symlink already planted inside the cache tree
could redirect a write; that requires prior filesystem access, at which point writing to
the binary cache is not the interesting capability.

### One dismissal is wrong

`pkg/taskset/resolver.go:484` is dismissed as a false positive on the grounds that
`resolveYAMLPath` "only ever receives a path already cleared by `containedPath`". It
does not. In `resolveRef`, the non-git branch runs `containedPath` inside
`if cloneRoot != ""`, and the function's own doc comment says *"Pass `""` to skip the
containment check"* — so there is a path to `resolveYAMLPath` on which no containment
check runs at all.

The alert is still arguably acceptable: the taint path CodeQL reports originates at the
session-authenticated add-source route, where an operator names a host path
deliberately, which lands it in the trusted-author group above. But the recorded reason
is a false premise rather than a judgement, and should be replaced with the real one.
