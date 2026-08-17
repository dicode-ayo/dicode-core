# Code scanning (CodeQL)

How CodeQL is configured here, how to reproduce its results on your own machine, and the
standing reason for every alert that stays open.

## What runs

Scanning uses GitHub's **default setup** — there is no `codeql.yml` in
`.github/workflows/`. As reported by `/code-scanning/default-setup`:

```yaml
query_suite:  default
threat_model: remote
schedule:     weekly
languages:    actions, go, javascript, javascript-typescript, python, typescript
```

`schedule: weekly` is the *additional* scheduled scan, not the only one: `main` is
analysed on every push, one analysis per commit SHA, which
`/code-scanning/analyses?ref=refs/heads/main` shows directly.

Two consequences surprise people:

**Pull requests fail on alerts that are new relative to the `main` baseline**, not on the
absolute count. A PR can be red while introducing no new vulnerability.

**A PR's analysis is diff-informed**, so it cannot validate an analysis change. A PR that
edits no Go files reports `results=0` for Go regardless of what the query would find on
the full tree. Run it locally, or read a `main` analysis.

## Standing triage

The Go suite reports **13** results: 10 `go/path-injection`, 2 `go/cookie-secure-not-set`
(`pkg/webui/sessions_db.go:377`, `:391`) and 1 `go/weak-sensitive-data-hashing`
(`pkg/webui/apikeys.go:258`). Every one is dismissed in the Security tab with a written
reason. That is what makes a red check meaningful: anything not on this list is new.

All 10 path-injection results are **one finding**. `LocalPath` on the add-source request
body (`pkg/webui/sources.go:321`) becomes a taskset root and then a task directory,
reaching `pkg/task/hash.go:299`, `:315`, `:343`, `:353`, `pkg/taskset/resolver.go:484`,
`internal/fsutil/fsutil.go:56`, `pkg/taskset/loader.go:31`, `:49`, `pkg/task/spec.go:692`
and `pkg/task/pipeline.go:89`.

Naming a host directory *is* the local-source feature; there is no root to confine it to
without removing the feature, and whoever can set it can already ship task code that runs
on the host. This is the trusted-author model in
[ADR-0002](./adr/0002-trust-boundary-is-the-merge.md). Note that "the author is trusted"
is a claim about the *merge* boundary — not the same claim as "this string is not remote
input" — so `threat_model: remote` is right to flag it.

Two constructs exist to keep other request values out of path expressions, and quietly
undoing either brings alerts back:

- `canonicalTaskFile` (`pkg/webui/server.go`) returns the entry from `allowedFiles`
  rather than the caller's matching string, so the editor builds paths from a literal.
- `runtimeVersionPattern` bounds the runtime `version` form value before it becomes both
  a cache path and a release download URL.

## Why the alert set churns on refactors

CodeQL keys an alert to the *function containing the sink*, so moving a flagged read into
a new enclosing function closes one alert and opens another even when no check changed.
PR #699 extracted `collectEntries` and `writeFileContribution` out of `task.Hash`, altered
nothing, and was reported as two new high-severity vulnerabilities.

A dismissal does not survive that, because the new alert is a different alert. Expect to
re-dismiss the `hash.go` entries after any refactor that moves them, and use this page's
reason rather than inventing a new one.

## Why not model our sanitizers

The tempting fix for the above is to declare this repo's path helpers to CodeQL as
[models-as-data](https://docs.github.com/en/code-security/tutorials/customize-code-scanning/creating-and-working-with-codeql-packs)
barriers. It was built, measured, and rejected. **Read this before rebuilding it** — the
alerts do disappear, which is what makes it dangerous.

A `barrierModel` row on `resolveInclude`'s return silences the four `hash.go` reads,
justified by that function's lexical strict-descendant check plus a symlink-canonicalized
containment check. The justification does not hold: every one of those flows enters
through **`absDir`**, not the `hash_include` value, and `boundary = filepath.Dir(absDir)`.
When `absDir` is the tainted value, containment is measured against the attacker's own
directory and bounds nothing. The model would blind four filesystem sinks permanently,
including against any future route by which a less-trusted value reaches `Hash`.

Generalised: **`barrierModel` is unconditional over a returned value, while every
containment helper here is relative to a caller-supplied root.** They do not compose. A
model would only be honest for a sanitizer resolving against a root the caller cannot
choose.

`barrierGuardModel`, the conditional form, is unusable for a separate reason. Go's
implementation in `semmle/go/dataflow/ExternalFlow.qll` requires the guard to be the
boolean value of the call expression itself (`g.asExpr().(CallExpr).getAnArgument() = e`),
whereas `pathguard.Within`, `pathguard.WithinResolved` and `taskset.containedPath` all
return `(bool, error)` or `error` and are tested through a destructured variable.
`convertAcceptingValue` also implements only `"true"`/`"false"` — `"null"`, the shape an
error-returning guard needs, is commented out as *"not supported yet"*. Rows for all three
were measured and silenced nothing.

Separately, GitHub documents model packs in default setup for C/C++, C#, Java/Kotlin,
Python, Ruby and Rust; **Go is absent from that list**, so a pack that works locally may
be inert in CI — leaving a suppression dormant rather than absent.

Constraining a value where it enters removes alerts without asserting anything to the
tool. Prefer it wherever it applies.

## Reproducing the results locally

Worth doing: reading a result's taint path is the only reliable way to decide whether a
suspected false positive really is one. It is what showed that the `hash.go` alerts are
directory-taint rather than include-taint.

A local run should agree with CI exactly. If it does not, the bundle version or the query
suite has drifted from what Actions runs.

Install the bundle — the **bundle**, not a bare CLI, so query and extractor versions match
Actions:

```bash
mkdir -p ~/.cache/codeql && cd ~/.cache/codeql
gh release download codeql-bundle-v2.26.3 --repo github/codeql-action \
  --pattern 'codeql-bundle-linux64.tar.zst'
tar --zstd -xf codeql-bundle-linux64.tar.zst
export PATH="$HOME/.cache/codeql/codeql:$PATH"
```

Build a database (~1 min; rebuild after any source change):

```bash
codeql database create /tmp/cq-db-go --language=go --source-root=. --overwrite
```

Run one query, or swap it for `codeql/go-queries:codeql-suites/go-code-scanning.qls` to
run the whole suite that default setup's `default` maps to:

```bash
codeql database analyze /tmp/cq-db-go \
  codeql/go-queries:Security/CWE-022/TaintedPath.ql \
  --format=sarif-latest --output=/tmp/tp.sarif --threads=4 --rerun
```

Read the results as `file:line`:

```bash
jq -r '.runs[0].results[]
  | "\(.locations[0].physicalLocation.artifactLocation.uri):\(.locations[0].physicalLocation.region.startLine)"' \
  /tmp/tp.sarif | sort
```

To see *why* a result survives — the guard CodeQL did not accept, or the branch on which
no guard runs — print its taint path:

```bash
jq -r '.runs[0].results[]
  | select(.locations[0].physicalLocation.artifactLocation.uri=="pkg/taskset/resolver.go")
  | .codeFlows[0].threadFlows[0].locations[]
  | "\(.location.physicalLocation.artifactLocation.uri):\(.location.physicalLocation.region.startLine)  \(.location.message.text)"' \
  /tmp/tp.sarif
```

Diffing two such `file:line` lists is how you measure any change to the analysis. Do it
before committing one: a row that silences nothing is dead config, and a row that silences
something for a reason that does not apply to it looks identical to success.
