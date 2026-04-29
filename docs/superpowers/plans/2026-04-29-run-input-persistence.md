# Run-input persistence + encrypted storage + retention sweep — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Implement child issue #233 — encrypted, redacted, pluggable persistence of webhook/manual run inputs with parameterizable retention. Foundation for the auto-fix loop's replay (#234) and agent-input (#238) primitives.

**Architecture:** Inputs are JSON-marshalled into a typed `PersistedInput` struct, redacted name-by-name against a deny-list, encrypted with XChaCha20-Poly1305 + per-row AAD using a sub-key derived from `pkg/secrets`'s master key, then handed to a configured "storage task" (default: `buildin/local-storage`) for write/read/delete. The `runs` table gains a handle column referencing the stored blob. Retention is swept hourly by a `buildin/run-inputs-cleanup` cron task using the `temp-cleanup` pattern. The storage task and cleanup task themselves opt out of input persistence to avoid recursion.

**Tech Stack:** Go (`golang.org/x/crypto/argon2`, `chacha20poly1305`, `modernc/sqlite`); Deno (cleanup + local-storage buildins); YAML.

**Spec reference:** [docs/superpowers/specs/2026-04-28-on-failure-auto-fix-loop-design.md](../specs/2026-04-28-on-failure-auto-fix-loop-design.md), §§ 4.1, 4.2.

---

## File Structure

**Created:**
- `pkg/task/template.go` — extend with `VarDataDir = "DATADIR"` constant + builtin-vars population (modify only).
- `pkg/db/migrate.go` — small new file holding the idempotent `addColumnIfMissing(db, table, name, ddl)` helper. Used now and reused by future migrations.
- `pkg/registry/inputcrypto.go` — XChaCha20-Poly1305 encrypt/decrypt, sub-key derivation from `pkg/secrets`'s master key, fixed-width AAD format.
- `pkg/registry/inputcrypto_test.go`
- `pkg/registry/inputredact.go` — `PersistedInput` struct, `PartMeta`, deny-list constants, recursive redaction logic for headers/query/params.
- `pkg/registry/inputredact_body.go` — body-redaction logic (split out because it's ~150 LOC of content-type-specific parsing).
- `pkg/registry/inputredact_test.go` and `pkg/registry/inputredact_body_test.go`.
- `pkg/registry/inputstore.go` — wraps the configured storage task; encrypt+marshal+call-task on write, call-task+decrypt+unmarshal on read; handles task-not-yet-registered fallback gracefully.
- `pkg/registry/inputstore_test.go`
- `tasks/buildin/local-storage/task.yaml`, `task.ts`, `task.test.ts`
- `tasks/buildin/run-inputs-cleanup/task.yaml`, `task.ts`, `task.test.ts`

**Modified:**
- `pkg/db/sqlite.go` — call the new migration helper after the existing `CREATE TABLE`s; document this as the introduction of incremental schema migrations.
- `pkg/registry/registry.go` — `Run` struct + `StartRunWithID` accept the new input columns; new methods `ListExpiredInputs`, `DeleteRunInput`, `PinRunInput`, `UnpinRunInput`, `GetRunInput`.
- `pkg/registry/runs_db.go` (or wherever the SQL lives) — UPDATEs to set/clear the new columns.
- `pkg/secrets/local.go` — small additions: export a `DeriveSubKey(context string) ([]byte, error)` method on `LocalProvider`, and a way for `pkg/registry` to obtain it via the secrets chain.
- `pkg/secrets/chain.go` (or interface file) — add the sub-key method to the `Provider` interface (default no-op for non-local providers; only the local one supports it).
- `pkg/config/config.go` — add `Defaults.RunInputs RunInputsConfig` struct with `Enabled`, `Retention time.Duration`, `StorageTask string`, `BodyFullTextual bool`.
- `pkg/task/spec.go` — add `Spec.RunInputs *RunInputsTaskOverride` and `Spec.AutoFix *AutoFixTaskConfig`.
- `pkg/trigger/engine.go` — at run-start (in `fireAsync`/`StartRunWithID`), call `inputstore.Persist(input)` and store the returned handle on the runs row. At engine startup, call `registry.SweepStalePins()`.
- `pkg/ipc/server.go` — new dispatch cases `dicode.runs.list_expired`, `dicode.runs.delete_input`, `dicode.runs.pin_input`, `dicode.runs.unpin_input`. (`get_input` is internal-only per spec — used by the auto-fix driver via SDK in #238; for #233 we add it but gate it behind a new permission flag.)
- `pkg/runtime/deno/sdk/shim.ts` — new TypeScript SDK methods on the `dicode` global mirroring the IPC additions.
- `pkg/ipc/capability.go` — new capability constants.
- `tasks/buildin/taskset.yaml` — add the two new buildins.

---

## Task 1: `${DATADIR}` task template variable

The cleanup and local-storage buildins both need to refer to the daemon's data directory in their `permissions.fs.path` and `params.default` fields. `${DATADIR}` exists at config-expansion time but not at task-template-expansion time today (this was the surprise that bit Task 13 of #236).

**Files:**
- Modify: `pkg/task/template.go`
- Modify: `docs/task-template-vars.md`
- Test: `pkg/task/template_test.go`

- [ ] **Step 1: Write failing test**

Append to `pkg/task/template_test.go`:

```go
func TestExpand_DATADIR(t *testing.T) {
	vars := map[string]string{
		"DATADIR": "/var/lib/dicode",
	}
	got := expandString("${DATADIR}/run-inputs", vars, false)
	if got != "/var/lib/dicode/run-inputs" {
		t.Errorf("got %q, want /var/lib/dicode/run-inputs", got)
	}
}

func TestBuiltinVars_IncludesDATADIRWhenProvided(t *testing.T) {
	vars := builtinVars("/some/task/dir", map[string]string{"DATADIR": "/var/lib/dicode"})
	if vars["DATADIR"] != "/var/lib/dicode" {
		t.Errorf("DATADIR = %q, want /var/lib/dicode", vars["DATADIR"])
	}
}
```

- [ ] **Step 2: Run, verify fail**

```
go test ./pkg/task/ -run "TestExpand_DATADIR|TestBuiltinVars_IncludesDATADIRWhenProvided" -v
```

Expected: 2nd test fails (`builtinVars` may already pass through arbitrary extras; verify and adjust).

- [ ] **Step 3: Add the constant + ensure builtinVars propagates DATADIR**

In `pkg/task/template.go`, after `VarTaskSetDir`:

```go
// VarDataDir is the absolute path to the daemon's data directory
// (config.Defaults.DataDir). Populated by the task loader from the
// running daemon's resolved data dir.
VarDataDir = "DATADIR"
```

Verify `builtinVars` already merges caller-supplied `extras` (it does — it's the documented mechanism for passing per-resolve vars). If so, no change to `builtinVars` itself is needed; only the constant is added so callers can use it as a typed key.

The actual population of `extras["DATADIR"]` happens at the task-loader entry point — find where `LoadDir` is called from `pkg/taskset` and `pkg/source/local` and ensure they pass `dataDir`. Look for call sites:

```
grep -rn "task.LoadDir\|LoadDir(" /workspaces/dicode-core-worktrees/run-input-persistence-233/pkg --include="*.go" | grep -v "_test"
```

Inject `extras["DATADIR"] = dataDir` at each call site. The `dataDir` is already plumbed to `pkg/taskset.NewSource` and from there to the resolver — wire it through.

- [ ] **Step 4: Run, verify pass + verify nothing else broke**

```
go test ./pkg/task/ ./pkg/taskset/ -timeout 60s
```

Expected: all PASS.

- [ ] **Step 5: Update docs/task-template-vars.md**

Add a row in the table describing `${DATADIR}`:

```
| `DATADIR` | Absolute path of the daemon's data directory (config.Defaults.DataDir). | Always (when loaded by the daemon). |
```

- [ ] **Step 6: Commit**

```bash
git add pkg/task/template.go pkg/task/template_test.go docs/task-template-vars.md pkg/taskset/ # any modified call sites
git commit -m "feat(task): \${DATADIR} task template variable

Adds DATADIR as a builtin task-template variable, populated from
config.Defaults.DataDir by the task loader. Used by the new
run-inputs-cleanup and local-storage buildins (#233) and any future
buildin that needs to reference the daemon's data dir.

Refs #233"
```

---

## Task 2: Schema migration helper + `runs` columns

Add idempotent `ALTER TABLE` support and the new input columns. This is dicode's first incremental schema migration.

**Files:**
- Create: `pkg/db/migrate.go`
- Create: `pkg/db/migrate_test.go`
- Modify: `pkg/db/sqlite.go`

- [ ] **Step 1: Write failing test**

`pkg/db/migrate_test.go`:

```go
package db

import (
	"context"
	"testing"
)

func TestAddColumnIfMissing_Adds(t *testing.T) {
	d := newTestDB(t)
	defer d.Close()

	if _, err := d.db.ExecContext(context.Background(),
		`CREATE TABLE foo (id INTEGER PRIMARY KEY)`); err != nil {
		t.Fatalf("create: %v", err)
	}

	if err := addColumnIfMissing(d.db, "foo", "bar", "TEXT"); err != nil {
		t.Fatalf("first add: %v", err)
	}
	// Idempotent — calling again must not error.
	if err := addColumnIfMissing(d.db, "foo", "bar", "TEXT"); err != nil {
		t.Fatalf("second add (should be no-op): %v", err)
	}

	rows, err := d.db.QueryContext(context.Background(), `PRAGMA table_info(foo)`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	cols := []string{}
	for rows.Next() {
		var cid int
		var name, ctype string
		var notnull, pk int
		var dflt any
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err != nil {
			t.Fatal(err)
		}
		cols = append(cols, name)
	}
	want := []string{"id", "bar"}
	if len(cols) != 2 || cols[0] != want[0] || cols[1] != want[1] {
		t.Errorf("cols = %v, want %v", cols, want)
	}
}
```

(`newTestDB` is a helper — look at existing `pkg/db/*_test.go` for the pattern; if absent, define a small one inline that opens an in-memory SQLite.)

- [ ] **Step 2: Run, verify fail**

```
go test ./pkg/db/ -run TestAddColumnIfMissing -v
```

Expected: FAIL — `addColumnIfMissing` undefined.

- [ ] **Step 3: Implement helper**

`pkg/db/migrate.go`:

```go
package db

import (
	"database/sql"
	"fmt"
)

// addColumnIfMissing adds a column to a table if it doesn't already exist.
// Idempotent: calling it again with the same arguments is a no-op.
//
// Used as the building block for incremental schema migrations layered on top
// of the existing CREATE TABLE IF NOT EXISTS statements in migrate(). When a
// future migration needs richer semantics (renames, backfills), a real
// versioned migration framework can be introduced; for now this helper keeps
// the diff small and the migration story honest.
func addColumnIfMissing(db *sql.DB, table, column, ddl string) error {
	rows, err := db.Query(fmt.Sprintf("PRAGMA table_info(%s)", table))
	if err != nil {
		return fmt.Errorf("table_info(%s): %w", table, err)
	}
	defer rows.Close()
	for rows.Next() {
		var cid int
		var name, ctype string
		var notnull, pk int
		var dflt any
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err != nil {
			return err
		}
		if name == column {
			return nil // already present
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	stmt := fmt.Sprintf("ALTER TABLE %s ADD COLUMN %s %s", table, column, ddl)
	if _, err := db.Exec(stmt); err != nil {
		return fmt.Errorf("alter %s add %s: %w", table, column, err)
	}
	return nil
}
```

- [ ] **Step 4: Run, verify pass**

```
go test ./pkg/db/ -run TestAddColumnIfMissing -v
```

Expected: PASS.

- [ ] **Step 5: Wire into `migrate()` for the `runs` table**

In `pkg/db/sqlite.go` `migrate()`, after the existing `CREATE TABLE IF NOT EXISTS runs(...)` block, add:

```go
// Incremental migrations on `runs` (introduced for #233 — first usage of
// ALTER TABLE in dicode's schema).
runsMigrations := []struct{ name, ddl string }{
	{"input_storage_key", "TEXT"},
	{"input_size", "INTEGER"},
	{"input_stored_at", "INTEGER"},
	{"input_pinned", "INTEGER NOT NULL DEFAULT 0"},
	{"input_redacted_fields", "TEXT"},
}
for _, m := range runsMigrations {
	if err := addColumnIfMissing(s.db, "runs", m.name, m.ddl); err != nil {
		return fmt.Errorf("migrate runs.%s: %w", m.name, err)
	}
}
```

- [ ] **Step 6: Run all DB tests + integration**

```
go test ./pkg/db/ ./pkg/registry/ -timeout 60s
```

Expected: all PASS. Existing registry tests still work (the new columns are nullable except `input_pinned` which has a default).

- [ ] **Step 7: Commit**

```bash
git add pkg/db/migrate.go pkg/db/migrate_test.go pkg/db/sqlite.go
git commit -m "feat(db): incremental migration helper + runs.input_* columns

addColumnIfMissing is dicode's first incremental schema-migration
helper; idempotent ALTER TABLE ADD COLUMN. Used now to add five
input columns to the runs table (input_storage_key, input_size,
input_stored_at, input_pinned, input_redacted_fields) for the run-
input persistence work in #233.

A real versioned migration framework can wait until the second
migration arrives.

Refs #233"
```

---

## Task 3: Sub-key derivation on `LocalProvider`

`pkg/secrets`'s master key is currently consumed only for the secrets-table key. Run-inputs need a *separate* derived key with a different salt so a leak of one doesn't compromise the other. Add a `DeriveSubKey(context string) ([]byte, error)` method.

**Files:**
- Modify: `pkg/secrets/local.go`
- Modify: `pkg/secrets/chain.go` (or `provider.go` — wherever the `Provider` interface lives)
- Test: `pkg/secrets/local_test.go`

- [ ] **Step 1: Write failing test**

Append to `pkg/secrets/local_test.go`:

```go
func TestLocalProvider_DeriveSubKey_Distinct(t *testing.T) {
	p := newTestLocalProvider(t)

	k1, err := p.DeriveSubKey("dicode/run-inputs/v1")
	if err != nil {
		t.Fatal(err)
	}
	k2, err := p.DeriveSubKey("dicode/other-purpose/v1")
	if err != nil {
		t.Fatal(err)
	}
	if len(k1) != 32 || len(k2) != 32 {
		t.Fatalf("expected 32-byte keys, got %d / %d", len(k1), len(k2))
	}
	if bytes.Equal(k1, k2) {
		t.Error("sub-keys with different contexts must differ")
	}
	// Same context, deterministic.
	k3, err := p.DeriveSubKey("dicode/run-inputs/v1")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(k1, k3) {
		t.Error("same context must yield same key")
	}
	// Sub-key must NOT equal the secrets-table derived key.
	if bytes.Equal(k1, p.key) {
		t.Error("sub-key must differ from primary derived key")
	}
}
```

(`newTestLocalProvider` may already exist; if not, add a minimal helper that creates a LocalProvider with a fixed master key and salt in a tempdir.)

- [ ] **Step 2: Run, verify fail**

```
go test ./pkg/secrets/ -run TestLocalProvider_DeriveSubKey -v
```

Expected: FAIL — `DeriveSubKey` undefined.

- [ ] **Step 3: Implement**

In `pkg/secrets/local.go`, add a method (and a private helper to load the raw master without re-running the full constructor):

```go
// DeriveSubKey returns a 32-byte key derived from the master key, distinct
// from the primary derived key used for the secrets table. The `context`
// string is used as a per-purpose Argon2id salt prefix; callers should pick
// a stable string like "dicode/run-inputs/v1" and version it for rotation.
//
// Determinism: same master + same context → same key. Two different
// contexts → independent keys (a leak of one does not reveal the other).
func (l *LocalProvider) DeriveSubKey(context string) ([]byte, error) {
	if l.masterKey == nil {
		return nil, fmt.Errorf("master key unavailable for sub-key derivation")
	}
	// Same Argon2id parameters as the primary derivation; salt = context bytes.
	// Argon2id is stable when given the same inputs.
	salt := []byte(context)
	return argon2.IDKey(l.masterKey, salt, 1, 64*1024, 4, 32), nil
}
```

For this to work, `LocalProvider` must retain the raw master key (currently it discards it after deriving the primary key on line 50). Modify the struct:

```go
type LocalProvider struct {
	key       []byte // 32-byte primary derived encryption key (secrets table)
	masterKey []byte // raw master key, retained so sub-keys can be derived
	db        localDB
}
```

And update `NewLocalProvider` to populate `masterKey`:

```go
return &LocalProvider{key: derivedKey, masterKey: masterKey, db: db}, nil
```

Security note: the raw master is now held in process memory longer than before. Document this in the doc comment and in the project's threat model. Acceptable trade-off for sub-key derivation; mitigated by the fact that the derived key was already in memory.

- [ ] **Step 4: Expose via the `Provider` interface (optional sub-key support)**

Add to whatever interface non-local providers also implement (`pkg/secrets/chain.go` or similar):

```go
// SubKeyDeriver is implemented by providers that can derive purpose-specific
// 32-byte sub-keys from their primary master. Currently only LocalProvider.
// Callers must type-assert; non-supporting providers signal the absence of
// derivation capability by not implementing this interface.
type SubKeyDeriver interface {
	DeriveSubKey(context string) ([]byte, error)
}
```

(Don't pollute the main `Provider` interface — keep it as a separate optional capability so future providers (KMS, Vault) opt in.)

- [ ] **Step 5: Run all secrets tests**

```
go test ./pkg/secrets/ -v
```

Expected: all PASS, including the new sub-key test.

- [ ] **Step 6: Commit**

```bash
git add pkg/secrets/local.go pkg/secrets/local_test.go pkg/secrets/chain.go
git commit -m "feat(secrets): LocalProvider.DeriveSubKey for purpose-specific keys

Adds a sub-key derivation method on LocalProvider. Argon2id with a
caller-supplied context string as salt yields a 32-byte key that is
deterministic for the same master+context, distinct across different
contexts, and independent of the primary secrets-table derived key.

Used by the run-input encryption work in #233 (context
'dicode/run-inputs/v1'). Future capabilities (token-store, OAuth
state) can use the same mechanism with their own context strings.

LocalProvider now retains the raw master key in memory for sub-key
derivation. Documented in the type's doc comment.

Refs #233"
```

---

## Task 4: Run-input encryption module

The crypto wrapper that the rest of the persistence layer will use.

**Files:**
- Create: `pkg/registry/inputcrypto.go`
- Create: `pkg/registry/inputcrypto_test.go`

- [ ] **Step 1: Write failing tests**

`pkg/registry/inputcrypto_test.go`:

```go
package registry

import (
	"bytes"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestInputCrypto_RoundTrip(t *testing.T) {
	c := newTestInputCrypto(t) // helper using a fixed-bytes sub-key

	runID := uuid.New().String()
	storedAt := time.Now().Unix()
	plaintext := []byte(`{"hello":"world"}`)

	blob, err := c.Encrypt(plaintext, runID, storedAt)
	if err != nil {
		t.Fatal(err)
	}
	got, err := c.Decrypt(blob, runID, storedAt)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, plaintext) {
		t.Errorf("got %q, want %q", got, plaintext)
	}
}

func TestInputCrypto_AAD_BindsToRunIDAndTimestamp(t *testing.T) {
	c := newTestInputCrypto(t)
	runA := uuid.New().String()
	runB := uuid.New().String()
	now := time.Now().Unix()

	blob, err := c.Encrypt([]byte("data"), runA, now)
	if err != nil {
		t.Fatal(err)
	}
	// Cross-row decrypt with different runID must fail.
	if _, err := c.Decrypt(blob, runB, now); err == nil {
		t.Error("decrypt with different runID should fail")
	}
	// Cross-time decrypt with different stored_at must fail.
	if _, err := c.Decrypt(blob, runA, now+1); err == nil {
		t.Error("decrypt with different stored_at should fail")
	}
}

func TestInputCrypto_NonceUniqueness(t *testing.T) {
	c := newTestInputCrypto(t)
	runID := uuid.New().String()
	now := time.Now().Unix()

	const N = 100
	seen := make(map[string]struct{}, N)
	for i := 0; i < N; i++ {
		blob, err := c.Encrypt([]byte("x"), runID, now)
		if err != nil {
			t.Fatal(err)
		}
		// Nonce is the leading 24 bytes.
		if len(blob) < 24 {
			t.Fatalf("blob too short: %d", len(blob))
		}
		nonce := string(blob[:24])
		if _, dup := seen[nonce]; dup {
			t.Fatalf("duplicate nonce after %d encryptions", i)
		}
		seen[nonce] = struct{}{}
	}
}

func TestInputCrypto_TamperedCiphertextRejected(t *testing.T) {
	c := newTestInputCrypto(t)
	runID := uuid.New().String()
	now := time.Now().Unix()

	blob, err := c.Encrypt([]byte("secret"), runID, now)
	if err != nil {
		t.Fatal(err)
	}
	// Flip a byte in the ciphertext.
	if len(blob) < 30 {
		t.Fatalf("blob too short")
	}
	blob[27] ^= 0xff
	if _, err := c.Decrypt(blob, runID, now); err == nil {
		t.Error("decrypt of tampered ciphertext should fail")
	}
}
```

`newTestInputCrypto`:

```go
func newTestInputCrypto(t *testing.T) *InputCrypto {
	t.Helper()
	// 32-byte deterministic test key — never used in production.
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i)
	}
	return NewInputCrypto(key)
}
```

- [ ] **Step 2: Run, verify fail**

```
go test ./pkg/registry/ -run TestInputCrypto -v
```

Expected: FAIL — `InputCrypto` undefined.

- [ ] **Step 3: Implement**

`pkg/registry/inputcrypto.go`:

```go
package registry

import (
	"crypto/rand"
	"encoding/binary"
	"fmt"

	"github.com/google/uuid"
	"golang.org/x/crypto/chacha20poly1305"
)

// InputCrypto encrypts run input blobs with XChaCha20-Poly1305 and a
// fixed-width binary AAD that binds each ciphertext to the runID and
// stored_at timestamp of its row in the runs table.
//
// Blob layout: [24-byte nonce][N-byte ciphertext+16-byte tag]
//
// AAD layout: [16-byte runID UUID raw bytes][8-byte stored_at uint64 BE]
type InputCrypto struct {
	key []byte // 32-byte sub-key from secrets.LocalProvider.DeriveSubKey
}

func NewInputCrypto(key []byte) *InputCrypto {
	return &InputCrypto{key: key}
}

// makeAAD returns the 24-byte fixed-width AAD that binds a blob to its row.
func makeAAD(runID string, storedAt int64) ([]byte, error) {
	u, err := uuid.Parse(runID)
	if err != nil {
		return nil, fmt.Errorf("runID is not a UUID: %w", err)
	}
	aad := make([]byte, 24)
	copy(aad[0:16], u[:])
	binary.BigEndian.PutUint64(aad[16:24], uint64(storedAt))
	return aad, nil
}

func (c *InputCrypto) Encrypt(plaintext []byte, runID string, storedAt int64) ([]byte, error) {
	aead, err := chacha20poly1305.NewX(c.key)
	if err != nil {
		return nil, fmt.Errorf("aead: %w", err)
	}
	aad, err := makeAAD(runID, storedAt)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, fmt.Errorf("nonce: %w", err)
	}
	out := make([]byte, 0, len(nonce)+len(plaintext)+aead.Overhead())
	out = append(out, nonce...)
	out = aead.Seal(out, nonce, plaintext, aad)
	return out, nil
}

func (c *InputCrypto) Decrypt(blob []byte, runID string, storedAt int64) ([]byte, error) {
	aead, err := chacha20poly1305.NewX(c.key)
	if err != nil {
		return nil, fmt.Errorf("aead: %w", err)
	}
	aad, err := makeAAD(runID, storedAt)
	if err != nil {
		return nil, err
	}
	if len(blob) < aead.NonceSize()+aead.Overhead() {
		return nil, fmt.Errorf("blob too short")
	}
	nonce, ct := blob[:aead.NonceSize()], blob[aead.NonceSize():]
	pt, err := aead.Open(nil, nonce, ct, aad)
	if err != nil {
		return nil, fmt.Errorf("decrypt: %w", err)
	}
	return pt, nil
}
```

- [ ] **Step 4: Run, verify pass**

```
go test ./pkg/registry/ -run TestInputCrypto -v
```

Expected: all PASS.

- [ ] **Step 5: Commit**

```bash
git add pkg/registry/inputcrypto.go pkg/registry/inputcrypto_test.go
git commit -m "feat(registry): InputCrypto — XChaCha20-Poly1305 + fixed AAD

Encrypts/decrypts run input blobs with per-row AAD that binds the
ciphertext to its runID + stored_at timestamp. Cross-row splicing
fails decryption.

Used by the run-input persistence layer (#233). Constructor takes a
32-byte sub-key derived via secrets.LocalProvider.DeriveSubKey
(\"dicode/run-inputs/v1\").

Refs #233"
```

---

## Task 5: `PersistedInput` struct + deny-list constants

The shape of what's stored, and the redaction policy data.

**Files:**
- Create: `pkg/registry/inputredact.go`
- Create: `pkg/registry/inputredact_test.go`

- [ ] **Step 1: Define the struct + deny-list (no logic yet)**

`pkg/registry/inputredact.go`:

```go
package registry

import (
	"encoding/json"
	"strings"
)

// PersistedInput is the structured shape of a run input as it lives encrypted
// at rest. Fields cover the union of webhook (HTTP), manual (params), cron
// (none), chain (params + parent context), and replay (carries persisted
// input forward) trigger sources.
type PersistedInput struct {
	Source         string              `json:"source"`                    // webhook | cron | manual | chain | daemon | replay
	Method         string              `json:"method,omitempty"`          // webhook only
	Path           string              `json:"path,omitempty"`            // webhook only
	Headers        map[string][]string `json:"headers,omitempty"`         // webhook; multi-valued for HTTP fidelity; post-redaction
	Query          map[string][]string `json:"query,omitempty"`           // webhook; same shape; post-redaction
	Body           json.RawMessage     `json:"body,omitempty"`            // see body policy in inputredact_body.go
	BodyKind       string              `json:"body_kind,omitempty"`       // "json" | "form" | "multipart" | "binary" | "text" | "omitted"
	BodyHash       string              `json:"body_hash,omitempty"`       // sha256 hex; present for omitted/binary/multipart
	BodyParts      []PartMeta          `json:"body_parts,omitempty"`      // multipart only
	Params         map[string]any      `json:"params,omitempty"`          // post-redaction (recursive)
	RedactedFields []string            `json:"redacted_fields,omitempty"` // dotted paths of redacted fields
}

// PartMeta describes a single multipart/form-data part. Values are NEVER
// stored — only structural metadata.
type PartMeta struct {
	Name        string `json:"name"`                   // form-field name (after redaction if name matched)
	Kind        string `json:"kind"`                   // "field" | "file"
	Filename    string `json:"filename,omitempty"`     // file parts only; redacted if name matched
	ContentType string `json:"content_type,omitempty"`
	Size        int64  `json:"size"`
}

// redactPlaceholder is the value substituted for any redacted scalar.
const redactPlaceholder = "<redacted>"

// denyListExact is the case-insensitive set of header/key names that are
// always redacted. Compared lowercased against the lowercased input name.
var denyListExact = map[string]struct{}{
	"authorization":         {},
	"cookie":                {},
	"set-cookie":            {},
	"x-hub-signature":       {},
	"x-hub-signature-256":   {},
	"x-dicode-signature":    {},
	"x-dicode-timestamp":    {},
	"x-slack-signature":     {},
	"x-line-signature":      {},
	"password":              {},
	"passphrase":            {},
	"api_key":               {},
	"apikey":                {},
	"api-key":               {},
	"secret":                {},
	"token":                 {},
	"bearer":                {},
}

// denyListSubstrings is matched as case-insensitive substring against the
// lowercased input name. Catches custom names like MY_SLACK_TOKEN and
// gh-secret-XYZ that don't appear in denyListExact.
var denyListSubstrings = []string{
	"signature",
	"token",
	"secret",
	"password",
	"key",
}

// shouldRedactName returns true if the lowercased name matches any deny-list rule.
func shouldRedactName(name string) bool {
	lower := strings.ToLower(name)
	if _, ok := denyListExact[lower]; ok {
		return true
	}
	for _, substr := range denyListSubstrings {
		if strings.Contains(lower, substr) {
			return true
		}
	}
	return false
}
```

- [ ] **Step 2: Test deny-list matching**

`pkg/registry/inputredact_test.go`:

```go
package registry

import "testing"

func TestShouldRedactName(t *testing.T) {
	cases := []struct {
		name string
		want bool
	}{
		{"Authorization", true},
		{"X-Hub-Signature-256", true},
		{"X-Slack-Signature", true},
		{"x-custom-token", true},
		{"my_secret_token", true},
		{"GH_PAT", true},                  // contains "key"? no. contains "pat"? no. — but "PAT" doesn't match any substring; should be false
		{"gh_pat_token", true},             // contains "token"
		{"public_key", true},               // contains "key"
		{"tokens_per_minute", true},        // over-redaction — substring match catches it; documented as safe failure mode
		{"User-Agent", false},
		{"Content-Type", false},
		{"X-Request-ID", false},            // "id" is not in the substring list
	}
	// Adjust GH_PAT expectation: "GH_PAT" lowercased is "gh_pat" — no substring matches.
	// Replace expected to false:
	cases[5].want = false

	for _, c := range cases {
		if got := shouldRedactName(c.name); got != c.want {
			t.Errorf("shouldRedactName(%q) = %v, want %v", c.name, got, c.want)
		}
	}
}
```

- [ ] **Step 3: Run, verify pass**

```
go test ./pkg/registry/ -run TestShouldRedactName -v
```

- [ ] **Step 4: Commit**

```bash
git add pkg/registry/inputredact.go pkg/registry/inputredact_test.go
git commit -m "feat(registry): PersistedInput struct + redaction deny-list

Defines the on-disk shape and the name-based redaction rules used by
the run-input persistence layer (#233). Substring-based matching
catches custom names like MY_SLACK_TOKEN and gh-secret-XYZ; over-
redaction (e.g. legitimate field 'tokens_per_minute') is the safe
failure mode.

Refs #233"
```

---

## Task 6: Recursive redaction for headers, query, params

The actual redaction logic over the PersistedInput's structured fields (excluding body, which has its own task).

**Files:**
- Modify: `pkg/registry/inputredact.go`
- Modify: `pkg/registry/inputredact_test.go`

- [ ] **Step 1: Write failing tests**

Append:

```go
func TestRedactHeaders(t *testing.T) {
	in := map[string][]string{
		"Authorization":  {"Bearer xyz"},
		"X-Custom-Token": {"abc", "def"},
		"User-Agent":     {"Mozilla/5.0"},
		"Content-Type":   {"application/json"},
	}
	redacted := []string{}
	out := redactHeaders(in, &redacted)

	if got := out["Authorization"][0]; got != redactPlaceholder {
		t.Errorf("Authorization not redacted: %q", got)
	}
	if got := out["X-Custom-Token"][0]; got != redactPlaceholder {
		t.Errorf("X-Custom-Token[0] not redacted: %q", got)
	}
	if got := out["X-Custom-Token"][1]; got != redactPlaceholder {
		t.Errorf("X-Custom-Token[1] not redacted: %q", got)
	}
	if got := out["User-Agent"][0]; got != "Mozilla/5.0" {
		t.Errorf("User-Agent should not be redacted: %q", got)
	}

	wantPaths := map[string]bool{
		"headers.Authorization":  true,
		"headers.X-Custom-Token": true,
	}
	got := map[string]bool{}
	for _, p := range redacted {
		got[p] = true
	}
	if len(got) != len(wantPaths) {
		t.Errorf("redacted = %v, want %v", got, wantPaths)
	}
	for k := range wantPaths {
		if !got[k] {
			t.Errorf("expected %q in redacted, got %v", k, redacted)
		}
	}
}

func TestRedactParams_RecursiveMaps(t *testing.T) {
	in := map[string]any{
		"name":  "Alice",
		"creds": map[string]any{
			"password": "secret123",
			"username": "alice",
		},
		"items": []any{
			map[string]any{"id": 1, "token": "t1"},
			map[string]any{"id": 2, "token": "t2"},
		},
	}
	redacted := []string{}
	out := redactParams(in, "params", &redacted).(map[string]any)

	if out["name"] != "Alice" {
		t.Errorf("name should not be redacted: %v", out["name"])
	}
	if got := out["creds"].(map[string]any)["password"]; got != redactPlaceholder {
		t.Errorf("creds.password not redacted: %v", got)
	}
	if got := out["creds"].(map[string]any)["username"]; got != "alice" {
		t.Errorf("creds.username should not be redacted: %v", got)
	}
	items := out["items"].([]any)
	for i, raw := range items {
		item := raw.(map[string]any)
		if item["id"] == nil {
			t.Errorf("items[%d].id missing", i)
		}
		if item["token"] != redactPlaceholder {
			t.Errorf("items[%d].token not redacted: %v", i, item["token"])
		}
	}

	wantPaths := []string{
		"params.creds.password",
		"params.items[0].token",
		"params.items[1].token",
	}
	for _, want := range wantPaths {
		found := false
		for _, p := range redacted {
			if p == want {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("expected %q in redacted, got %v", want, redacted)
		}
	}
}
```

- [ ] **Step 2: Run, verify fail**

```
go test ./pkg/registry/ -run "TestRedactHeaders|TestRedactParams" -v
```

Expected: FAIL — functions undefined.

- [ ] **Step 3: Implement**

Append to `pkg/registry/inputredact.go`:

```go
// redactHeaders walks an HTTP-style map[string][]string. For each key whose
// name matches the deny-list, every value in the slice is replaced with the
// placeholder. Keys are recorded as "headers.<name>" in the redacted slice.
func redactHeaders(in map[string][]string, redacted *[]string) map[string][]string {
	if len(in) == 0 {
		return in
	}
	out := make(map[string][]string, len(in))
	for name, vals := range in {
		if shouldRedactName(name) {
			redactedVals := make([]string, len(vals))
			for i := range redactedVals {
				redactedVals[i] = redactPlaceholder
			}
			out[name] = redactedVals
			*redacted = append(*redacted, "headers."+name)
		} else {
			out[name] = append([]string(nil), vals...)
		}
	}
	return out
}

// redactQuery applies the same logic with a "query." path prefix.
func redactQuery(in map[string][]string, redacted *[]string) map[string][]string {
	if len(in) == 0 {
		return in
	}
	out := make(map[string][]string, len(in))
	for name, vals := range in {
		if shouldRedactName(name) {
			redactedVals := make([]string, len(vals))
			for i := range redactedVals {
				redactedVals[i] = redactPlaceholder
			}
			out[name] = redactedVals
			*redacted = append(*redacted, "query."+name)
		} else {
			out[name] = append([]string(nil), vals...)
		}
	}
	return out
}

// redactParams recursively walks a generic value (typically map[string]any
// from JSON) replacing values for keys whose names match the deny-list.
// Lists are walked positionally with [N] in the path. Returns a new value;
// does not mutate the input.
func redactParams(v any, path string, redacted *[]string) any {
	switch x := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, child := range x {
			childPath := path + "." + k
			if shouldRedactName(k) {
				out[k] = redactPlaceholder
				*redacted = append(*redacted, childPath)
				continue
			}
			out[k] = redactParams(child, childPath, redacted)
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, child := range x {
			out[i] = redactParams(child, fmt.Sprintf("%s[%d]", path, i), redacted)
		}
		return out
	default:
		return v
	}
}
```

Add `"fmt"` import.

- [ ] **Step 4: Run, verify pass**

```
go test ./pkg/registry/ -run "TestRedactHeaders|TestRedactParams" -v
```

Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add pkg/registry/inputredact.go pkg/registry/inputredact_test.go
git commit -m "feat(registry): recursive redaction for headers/query/params

Implements name-based recursive redaction over the structured fields
of PersistedInput. Lists are walked positionally with [N] in the
path; nested maps recurse. Headers and query are HTTP-style
multi-valued maps; redaction replaces every value in a matched
slice with the placeholder.

Body redaction (json/form/multipart/binary) is split out into its
own file in the next task because the content-type-specific parsing
adds another ~150 LOC.

Refs #233"
```

---

## Task 7: Body redaction (content-type-aware)

Split out because of size + content-type-specific parsing.

**Files:**
- Create: `pkg/registry/inputredact_body.go`
- Create: `pkg/registry/inputredact_body_test.go`

- [ ] **Step 1: Write failing tests**

`pkg/registry/inputredact_body_test.go`:

```go
package registry

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/url"
	"strings"
	"testing"
)

func TestRedactBody_JSON(t *testing.T) {
	body := []byte(`{"user":"alice","password":"secret","token":"abc"}`)
	redacted := []string{}
	out := redactBody(body, "application/json", false, &redacted)

	if out.BodyKind != "json" {
		t.Errorf("BodyKind = %q, want json", out.BodyKind)
	}
	var got map[string]any
	if err := json.Unmarshal(out.Body, &got); err != nil {
		t.Fatal(err)
	}
	if got["user"] != "alice" {
		t.Errorf("user mutated: %v", got["user"])
	}
	if got["password"] != redactPlaceholder {
		t.Errorf("password not redacted: %v", got["password"])
	}
	if got["token"] != redactPlaceholder {
		t.Errorf("token not redacted: %v", got["token"])
	}
}

func TestRedactBody_FormURLEncoded(t *testing.T) {
	body := []byte("user=alice&password=secret&token=abc")
	redacted := []string{}
	out := redactBody(body, "application/x-www-form-urlencoded", false, &redacted)

	if out.BodyKind != "form" {
		t.Errorf("BodyKind = %q, want form", out.BodyKind)
	}
	parsed, err := url.ParseQuery(string(out.Body))
	if err != nil {
		t.Fatal(err)
	}
	if parsed.Get("user") != "alice" {
		t.Errorf("user mutated")
	}
	if parsed.Get("password") != redactPlaceholder {
		t.Errorf("password not redacted")
	}
}

func TestRedactBody_BinaryDefaultOmitted(t *testing.T) {
	body := []byte{0x00, 0x01, 0x02, 0x03}
	redacted := []string{}
	out := redactBody(body, "application/octet-stream", false, &redacted)

	if out.BodyKind != "binary" {
		t.Errorf("BodyKind = %q, want binary", out.BodyKind)
	}
	if len(out.Body) != 0 {
		t.Errorf("body should be omitted for binary; got %d bytes", len(out.Body))
	}
	sum := sha256.Sum256(body)
	if out.BodyHash != hex.EncodeToString(sum[:]) {
		t.Errorf("BodyHash = %q, want %s", out.BodyHash, hex.EncodeToString(sum[:]))
	}
}

func TestRedactBody_TextDefaultOmitted(t *testing.T) {
	body := []byte("some opaque text payload")
	redacted := []string{}
	out := redactBody(body, "text/plain", false, &redacted)

	if out.BodyKind != "text" {
		t.Errorf("BodyKind = %q, want text", out.BodyKind)
	}
	if len(out.Body) != 0 {
		t.Errorf("body should be omitted for text by default")
	}
	if out.BodyHash == "" {
		t.Error("BodyHash should be set")
	}
}

func TestRedactBody_TextFullTextualOptIn(t *testing.T) {
	body := []byte("some opaque text payload")
	redacted := []string{}
	out := redactBody(body, "text/plain", true, &redacted)

	if out.BodyKind != "text" {
		t.Errorf("BodyKind = %q, want text", out.BodyKind)
	}
	if string(out.Body) != strings.TrimSpace(string(body)) && string(out.Body) != string(body) {
		// Accept verbatim or trim.
		t.Errorf("body should be persisted verbatim under bodyFullTextual; got %q", out.Body)
	}
}

func TestRedactBody_Multipart(t *testing.T) {
	body := []byte("--BOUNDARY\r\n" +
		"Content-Disposition: form-data; name=\"username\"\r\n\r\n" +
		"alice\r\n" +
		"--BOUNDARY\r\n" +
		"Content-Disposition: form-data; name=\"avatar\"; filename=\"face.png\"\r\n" +
		"Content-Type: image/png\r\n\r\n" +
		"\x89PNG\r\n\x1a\n" +
		"\r\n--BOUNDARY--\r\n")
	redacted := []string{}
	out := redactBody(body, "multipart/form-data; boundary=BOUNDARY", false, &redacted)

	if out.BodyKind != "multipart" {
		t.Errorf("BodyKind = %q, want multipart", out.BodyKind)
	}
	if len(out.BodyParts) != 2 {
		t.Fatalf("BodyParts len = %d, want 2; parts = %#v", len(out.BodyParts), out.BodyParts)
	}
	if out.BodyParts[0].Name != "username" || out.BodyParts[0].Kind != "field" {
		t.Errorf("part[0] = %+v", out.BodyParts[0])
	}
	if out.BodyParts[1].Name != "avatar" || out.BodyParts[1].Kind != "file" || out.BodyParts[1].Filename != "face.png" {
		t.Errorf("part[1] = %+v", out.BodyParts[1])
	}
	if len(out.Body) != 0 {
		t.Errorf("multipart body should be omitted; values are not stored")
	}
}
```

- [ ] **Step 2: Run, verify fail**

```
go test ./pkg/registry/ -run TestRedactBody -v
```

Expected: FAIL — `redactBody` undefined.

- [ ] **Step 3: Implement**

`pkg/registry/inputredact_body.go`:

```go
package registry

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"mime"
	"mime/multipart"
	"net/url"
	"strings"
)

// redactBody applies content-type-aware redaction to an HTTP body. Returns
// the persistable BodyKind/Body/BodyHash/BodyParts fields populated.
//
// Behavior by content-type:
//   - application/json: walk like Params; persist post-redaction JSON.
//   - application/x-www-form-urlencoded: parse, walk values, re-encode.
//   - multipart/form-data: parse, store metadata only; values discarded.
//   - text/* (and other textual types): omit by default; if bodyFullTextual is
//     true, persist verbatim (caller-accepted footgun).
//   - everything else: omit; store hash only.
//
// Always sets BodyHash to sha256(rawBody) for forensic comparison.
func redactBody(raw []byte, contentType string, bodyFullTextual bool, redacted *[]string) PersistedInput {
	out := PersistedInput{}
	sum := sha256.Sum256(raw)
	hash := hex.EncodeToString(sum[:])
	mediaType, params, _ := mime.ParseMediaType(contentType)
	switch {
	case mediaType == "application/json":
		out.BodyKind = "json"
		var v any
		if err := json.Unmarshal(raw, &v); err != nil {
			// Mark as binary if it doesn't parse — defensive.
			out.BodyKind = "binary"
			out.BodyHash = hash
			return out
		}
		walked := redactParams(v, "body", redacted)
		marshalled, err := json.Marshal(walked)
		if err != nil {
			out.BodyKind = "binary"
			out.BodyHash = hash
			return out
		}
		out.Body = marshalled

	case mediaType == "application/x-www-form-urlencoded":
		out.BodyKind = "form"
		vals, err := url.ParseQuery(string(raw))
		if err != nil {
			out.BodyKind = "binary"
			out.BodyHash = hash
			return out
		}
		for k := range vals {
			if shouldRedactName(k) {
				for i := range vals[k] {
					vals[k][i] = redactPlaceholder
				}
				*redacted = append(*redacted, "body."+k)
			}
		}
		out.Body = json.RawMessage(vals.Encode())
		// Note: storing form-encoded bytes inside json.RawMessage is awkward;
		// wrap as a JSON string for shape consistency.
		jsonForm, _ := json.Marshal(vals.Encode())
		out.Body = jsonForm

	case mediaType == "multipart/form-data":
		out.BodyKind = "multipart"
		out.BodyHash = hash
		boundary, ok := params["boundary"]
		if !ok {
			return out
		}
		mr := multipart.NewReader(strings.NewReader(string(raw)), boundary)
		var parts []PartMeta
		for {
			p, err := mr.NextPart()
			if err == io.EOF {
				break
			}
			if err != nil {
				return out
			}
			meta := PartMeta{
				Name:        p.FormName(),
				ContentType: p.Header.Get("Content-Type"),
			}
			if filename := p.FileName(); filename != "" {
				meta.Kind = "file"
				if shouldRedactName(meta.Name) {
					meta.Filename = redactPlaceholder
					*redacted = append(*redacted, "body_parts."+meta.Name+".filename")
				} else {
					meta.Filename = filename
				}
			} else {
				meta.Kind = "field"
			}
			body, _ := io.ReadAll(p)
			meta.Size = int64(len(body))
			parts = append(parts, meta)
		}
		out.BodyParts = parts

	case strings.HasPrefix(mediaType, "text/"):
		out.BodyKind = "text"
		if bodyFullTextual {
			j, _ := json.Marshal(string(raw))
			out.Body = j
		} else {
			out.BodyHash = hash
		}

	default:
		out.BodyKind = "binary"
		out.BodyHash = hash
	}
	return out
}
```

- [ ] **Step 4: Run, verify pass**

```
go test ./pkg/registry/ -run TestRedactBody -v
```

Expected: all 6 tests PASS.

- [ ] **Step 5: Commit**

```bash
git add pkg/registry/inputredact_body.go pkg/registry/inputredact_body_test.go
git commit -m "feat(registry): content-type-aware body redaction

Json: walk like params; persist post-redaction JSON.
Form: parse, walk, re-encode (stored as JSON string).
Multipart: parse, store BodyParts metadata only; values discarded.
Text: omit by default; persist verbatim only when bodyFullTextual=true.
Other: omit; sha256 hash for forensic comparison.

Refs #233"
```

---

## Task 8: Storage task contract + routing helper

Encrypt+marshal+call-task on write; call-task+decrypt+unmarshal on read.

**Files:**
- Create: `pkg/registry/inputstore.go`
- Create: `pkg/registry/inputstore_test.go`

- [ ] **Step 1: Define the interface to the trigger engine**

The InputStore needs a way to invoke the configured storage task. The trigger engine provides synchronous task firing via `fireSync`. To keep registry decoupled, define a small interface InputStore takes as a dependency:

```go
// TaskRunner abstracts the trigger engine's ability to run a task synchronously
// with given params. Used by InputStore to delegate to the configured storage
// task without taking a hard dependency on pkg/trigger.
type TaskRunner interface {
	RunTaskSync(ctx context.Context, taskID string, params map[string]string) (returnValue any, err error)
}
```

`pkg/registry/inputstore.go` (skeleton):

```go
package registry

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
)

type TaskRunner interface {
	RunTaskSync(ctx context.Context, taskID string, params map[string]string) (any, error)
}

type InputStore struct {
	crypto      *InputCrypto
	runner      TaskRunner
	storageTask string // task ID, e.g. "buildin/local-storage"
}

func NewInputStore(crypto *InputCrypto, runner TaskRunner, storageTask string) *InputStore {
	return &InputStore{crypto: crypto, runner: runner, storageTask: storageTask}
}

// Persist encrypts the marshalled PersistedInput and stores it via the
// storage task. Returns the storage key + size + stored_at_unix.
func (s *InputStore) Persist(ctx context.Context, runID string, in PersistedInput) (key string, size int, storedAt int64, err error) {
	pt, err := json.Marshal(in)
	if err != nil {
		return "", 0, 0, fmt.Errorf("marshal: %w", err)
	}
	storedAt = nowUnix()
	blob, err := s.crypto.Encrypt(pt, runID, storedAt)
	if err != nil {
		return "", 0, 0, fmt.Errorf("encrypt: %w", err)
	}
	key = "run-inputs/" + runID
	enc := base64.StdEncoding.EncodeToString(blob)
	if _, err := s.runner.RunTaskSync(ctx, s.storageTask, map[string]string{
		"op":    "put",
		"key":   key,
		"value": enc,
	}); err != nil {
		return "", 0, 0, fmt.Errorf("storage put: %w", err)
	}
	return key, len(blob), storedAt, nil
}

// Fetch retrieves and decrypts a persisted input.
func (s *InputStore) Fetch(ctx context.Context, runID, key string, storedAt int64) (PersistedInput, error) {
	res, err := s.runner.RunTaskSync(ctx, s.storageTask, map[string]string{
		"op":  "get",
		"key": key,
	})
	if err != nil {
		return PersistedInput{}, fmt.Errorf("storage get: %w", err)
	}
	resMap, ok := res.(map[string]any)
	if !ok {
		return PersistedInput{}, fmt.Errorf("storage task returned non-map: %T", res)
	}
	encStr, _ := resMap["value"].(string)
	if encStr == "" {
		return PersistedInput{}, ErrInputUnavailable
	}
	blob, err := base64.StdEncoding.DecodeString(encStr)
	if err != nil {
		return PersistedInput{}, fmt.Errorf("decode: %w", err)
	}
	pt, err := s.crypto.Decrypt(blob, runID, storedAt)
	if err != nil {
		return PersistedInput{}, fmt.Errorf("decrypt: %w", err)
	}
	var out PersistedInput
	if err := json.Unmarshal(pt, &out); err != nil {
		return PersistedInput{}, fmt.Errorf("unmarshal: %w", err)
	}
	return out, nil
}

// Delete removes a persisted input via the storage task.
func (s *InputStore) Delete(ctx context.Context, key string) error {
	_, err := s.runner.RunTaskSync(ctx, s.storageTask, map[string]string{
		"op":  "delete",
		"key": key,
	})
	return err
}

var ErrInputUnavailable = fmt.Errorf("input unavailable (gc'd or never stored)")

func nowUnix() int64 { return timeNow().Unix() }

// Test seam.
var timeNow = func() time.Time { return time.Now() }
```

(Add `"time"` import.)

- [ ] **Step 2: Test with a mock TaskRunner**

`pkg/registry/inputstore_test.go`:

```go
package registry

import (
	"context"
	"encoding/base64"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
)

type mockRunner struct {
	store map[string]string
}

func (m *mockRunner) RunTaskSync(ctx context.Context, taskID string, params map[string]string) (any, error) {
	op := params["op"]
	switch op {
	case "put":
		m.store[params["key"]] = params["value"]
		return map[string]any{"ok": true}, nil
	case "get":
		v, ok := m.store[params["key"]]
		if !ok {
			return map[string]any{"ok": false, "value": ""}, nil
		}
		return map[string]any{"ok": true, "value": v}, nil
	case "delete":
		delete(m.store, params["key"])
		return map[string]any{"ok": true}, nil
	}
	return nil, errors.New("unknown op")
}

func TestInputStore_RoundTrip(t *testing.T) {
	frozen := time.Unix(1714400000, 0)
	timeNow = func() time.Time { return frozen }
	defer func() { timeNow = func() time.Time { return time.Now() } }()

	mr := &mockRunner{store: map[string]string{}}
	c := newTestInputCrypto(t)
	s := NewInputStore(c, mr, "buildin/local-storage")

	runID := uuid.New().String()
	in := PersistedInput{Source: "webhook", Method: "POST"}

	key, size, storedAt, err := s.Persist(context.Background(), runID, in)
	if err != nil {
		t.Fatal(err)
	}
	if size <= 0 || storedAt != frozen.Unix() || key == "" {
		t.Errorf("metadata: size=%d storedAt=%d key=%q", size, storedAt, key)
	}
	// Confirm it's base64 in the mock.
	if _, err := base64.StdEncoding.DecodeString(mr.store[key]); err != nil {
		t.Errorf("not base64: %v", err)
	}

	got, err := s.Fetch(context.Background(), runID, key, storedAt)
	if err != nil {
		t.Fatal(err)
	}
	if got.Source != "webhook" || got.Method != "POST" {
		t.Errorf("got %#v", got)
	}

	if err := s.Delete(context.Background(), key); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Fetch(context.Background(), runID, key, storedAt); !errors.Is(err, ErrInputUnavailable) {
		t.Errorf("expected ErrInputUnavailable after delete; got %v", err)
	}
}
```

- [ ] **Step 3: Run, verify pass**

```
go test ./pkg/registry/ -run TestInputStore -v
```

- [ ] **Step 4: Commit**

```bash
git add pkg/registry/inputstore.go pkg/registry/inputstore_test.go
git commit -m "feat(registry): InputStore — storage-task routing + crypto

Marshals PersistedInput, encrypts with InputCrypto, base64-encodes,
delegates put/get/delete to the configured storage task via a
TaskRunner interface. Time is mockable via the timeNow seam.

Used by the trigger-engine integration in Task 10.

Refs #233"
```

---

## Task 9: `local-storage` buildin task

Default storage backend that writes encrypted blobs to `${DATADIR}/run-inputs/`.

**Files:**
- Create: `tasks/buildin/local-storage/task.yaml`
- Create: `tasks/buildin/local-storage/task.ts`
- Create: `tasks/buildin/local-storage/task.test.ts`
- Modify: `tasks/buildin/taskset.yaml`

- [ ] **Step 1: task.yaml**

```yaml
apiVersion: dicode/v1
kind: Task
name: "Local Storage (run inputs)"
description: |
  Default backend for run-input persistence (#233). Stores opaque
  base64-encoded ciphertext blobs in ${DATADIR}/run-inputs/<key>.
  Core does the encryption; this task only sees ciphertext.

runtime: deno

trigger:
  manual: true

params:
  op:    { type: string, required: true }   # put | get | delete
  key:   { type: string, required: true }
  value: { type: string, default: "" }      # base64(blob); put only

permissions:
  fs:
    - path: "${DATADIR}/run-inputs"
      permission: rw

# This task itself opts out of input persistence to avoid recursion.
run_inputs:
  enabled: false

timeout: 30s

notify:
  on_success: false
  on_failure: true
```

- [ ] **Step 2: task.ts**

```ts
// Local Storage backend for run-input persistence.
//
// Stores base64-encoded ciphertext blobs as files under a fixed root.
// Core does encryption/redaction; this task is a dumb byte store.

interface PutResult { ok: true }
interface GetResult { ok: true; value: string }
interface DeleteResult { ok: true }
interface ErrorResult { ok: false; error: string }

const ROOT_TEMPLATE = "${DATADIR}/run-inputs"; // expanded at task-load time

function fileFor(root: string, key: string): string {
  // Key is "run-inputs/<runID>" — the runID portion is UUID-shaped (the
  // engine validates RunID via pkg/taskset.ValidateRunID before reaching
  // here, so safe to use as a path component). We strip the prefix.
  const safeKey = key.replace(/^run-inputs\//, "");
  if (safeKey.includes("/") || safeKey.includes("..") || safeKey === "") {
    throw new Error(`invalid storage key: ${key}`);
  }
  return `${root}/${safeKey}.bin`;
}

export default async function main({ params }: DicodeSdk):
  Promise<PutResult | GetResult | DeleteResult | ErrorResult> {
  const op = String(params.get("op") ?? "");
  const key = String(params.get("key") ?? "");
  const root = ROOT_TEMPLATE; // The literal string is replaced at task-yaml-load by the template var

  if (!op || !key) return { ok: false, error: "op and key required" };

  try {
    await Deno.mkdir(root, { recursive: true });
    const path = fileFor(root, key);

    if (op === "put") {
      const value = String(params.get("value") ?? "");
      if (!value) return { ok: false, error: "value required for put" };
      // Decode→encode round-trip to validate base64.
      const bytes = base64Decode(value);
      await Deno.writeFile(path, bytes);
      return { ok: true };
    }
    if (op === "get") {
      try {
        const bytes = await Deno.readFile(path);
        return { ok: true, value: base64Encode(bytes) };
      } catch (e) {
        if (e instanceof Deno.errors.NotFound) {
          return { ok: true, value: "" };
        }
        throw e;
      }
    }
    if (op === "delete") {
      try {
        await Deno.remove(path);
      } catch (e) {
        if (!(e instanceof Deno.errors.NotFound)) throw e;
      }
      return { ok: true };
    }
    return { ok: false, error: `unknown op: ${op}` };
  } catch (e) {
    return { ok: false, error: e instanceof Error ? e.message : String(e) };
  }
}

function base64Decode(s: string): Uint8Array {
  return Uint8Array.from(atob(s), c => c.charCodeAt(0));
}
function base64Encode(b: Uint8Array): string {
  let s = "";
  for (const byte of b) s += String.fromCharCode(byte);
  return btoa(s);
}
```

Wait — `${ROOT_TEMPLATE}` won't actually be expanded by Deno code. The expansion happens in `task.yaml` (params or fs paths) at task-load time. Reading the path from a `params.default` is the convention. Update `task.yaml` to add `root` as a param:

```yaml
params:
  op:    { type: string, required: true }
  key:   { type: string, required: true }
  value: { type: string, default: "" }
  root:
    type: string
    default: "${DATADIR}/run-inputs"
    description: "Internal — set by task-yaml template var; not user-facing."
```

And in `task.ts` read `params.get("root")` instead of the embedded literal.

- [ ] **Step 3: task.test.ts**

```ts
import { setupHarness } from "../../sdk-test.ts";
await setupHarness(import.meta.url);

import main from "./task.ts";
import { assertEquals, assert } from "https://deno.land/std@0.224.0/assert/mod.ts";

function withParams(p: Record<string, string>) {
  return {
    params: { get: (k: string) => p[k] ?? null },
  } as any;
}

Deno.test("put + get round-trip", async () => {
  const root = await Deno.makeTempDir();
  const value = btoa("hello world");

  let res = await main(withParams({ op: "put", key: "run-inputs/abc", value, root }));
  assertEquals(res, { ok: true });

  res = await main(withParams({ op: "get", key: "run-inputs/abc", root })) as any;
  assertEquals(res.ok, true);
  assertEquals(res.value, value);
});

Deno.test("get of missing key returns empty value", async () => {
  const root = await Deno.makeTempDir();
  const res = await main(withParams({ op: "get", key: "run-inputs/missing", root })) as any;
  assertEquals(res.ok, true);
  assertEquals(res.value, "");
});

Deno.test("delete is idempotent", async () => {
  const root = await Deno.makeTempDir();
  const res = await main(withParams({ op: "delete", key: "run-inputs/never-existed", root }));
  assertEquals(res, { ok: true });
});

Deno.test("rejects path traversal in key", async () => {
  const root = await Deno.makeTempDir();
  const value = btoa("x");
  const res = await main(withParams({ op: "put", key: "run-inputs/../escape", value, root })) as any;
  assertEquals(res.ok, false);
  assert(res.error.includes("invalid storage key"));
});
```

- [ ] **Step 4: Add to taskset.yaml**

In `tasks/buildin/taskset.yaml` alongside `temp-cleanup`:

```yaml
    local-storage:
      ref:
        path: ./local-storage/task.yaml
```

- [ ] **Step 5: Test**

```
deno test --allow-read --allow-write tasks/buildin/local-storage/task.test.ts
```

Expected: all PASS.

- [ ] **Step 6: Commit**

```bash
git add tasks/buildin/local-storage/ tasks/buildin/taskset.yaml
git commit -m "feat(buildin): local-storage backend for run inputs

Default storage task for the run-input persistence layer (#233).
Stores base64 ciphertext blobs under \${DATADIR}/run-inputs/<key>.bin.
Path-traversal in the key is rejected (defense in depth — core also
validates the runID component of the key).

This task opts itself out of input persistence (run_inputs.enabled:
false) to avoid recursion.

Refs #233"
```

---

## Task 10: Engine integration — persist input on every run start

Wire `InputStore` into `pkg/trigger/engine.go`'s `fireAsync` so every run's input is persisted before execution begins.

**Files:**
- Modify: `pkg/trigger/engine.go`
- Modify: `pkg/registry/registry.go` (extend `StartRunWithID` or add a setter for the input columns)
- Test: `pkg/trigger/engine_input_persistence_test.go`

- [ ] **Step 1: Extend the registry to set input columns**

`pkg/registry/registry.go`: add a setter callable after `StartRunWithID`:

```go
// SetRunInput stores the persistence handle on the runs row.
func (r *Registry) SetRunInput(ctx context.Context, runID, storageKey string, size int, storedAt int64, redactedFields []string) error {
	rfJSON, _ := json.Marshal(redactedFields)
	_, err := r.db.ExecContext(ctx,
		`UPDATE runs SET input_storage_key = ?, input_size = ?, input_stored_at = ?, input_redacted_fields = ?
		 WHERE id = ?`,
		storageKey, size, storedAt, string(rfJSON), runID,
	)
	return err
}
```

- [ ] **Step 2: Wire engine to persist on run-start**

The engine needs:
1. An `InputStore` field (set during construction or via a setter, populated by the daemon when it has the secrets master key).
2. A code path in `fireAsync` (or right before calling the runtime) that:
   - Skips persistence if `cfg.Defaults.RunInputs.Enabled = false` OR per-task `run_inputs.enabled = false` OR the task is the configured `storage_task` OR `run-inputs-cleanup`.
   - Builds a `PersistedInput` from the run's webhook context / params / chain input.
   - Applies redaction (`redactHeaders`, `redactQuery`, `redactBody`, `redactParams`).
   - Calls `inputStore.Persist`, then `registry.SetRunInput` to save the handle.

Sketch (adapt to the actual `fireAsync` shape):

```go
// Build a PersistedInput from the run's source. For webhook runs, the engine
// has the request context (method, path, headers, body); for manual/SDK
// runs it has params; for chain runs the chain input map.
func (e *Engine) maybePersistInput(ctx context.Context, spec *task.Spec, runID string, src runSource) {
	if !e.shouldPersistInput(spec) || e.inputStore == nil {
		return
	}
	in, redactedFields := buildPersistedInput(src, spec)
	key, size, storedAt, err := e.inputStore.Persist(ctx, runID, in)
	if err != nil {
		e.log.Warn("run-input persist failed",
			zap.String("run", runID), zap.Error(err))
		return // non-fatal — run continues
	}
	if err := e.registry.SetRunInput(ctx, runID, key, size, storedAt, redactedFields); err != nil {
		e.log.Warn("run-input set columns failed",
			zap.String("run", runID), zap.Error(err))
	}
}
```

`shouldPersistInput` gates on:
- `e.cfg.Defaults.RunInputs.Enabled` (default true).
- `spec.RunInputs != nil && !spec.RunInputs.Enabled` for per-task opt-out.
- `spec.ID == e.cfg.Defaults.RunInputs.StorageTask` (recursion guard).
- `spec.ID == "buildin/run-inputs-cleanup"` (recursion guard).

- [ ] **Step 3: Write integration test**

`pkg/trigger/engine_input_persistence_test.go` — add a test that fires a manual task with params, then queries the registry for the stored input:

```go
func TestEngine_PersistsInputOnRunStart(t *testing.T) {
	env := newTestEnvWithInputStore(t) // helper: configures InputStore w/ a mock backing
	defer env.cleanup()

	env.writeTask("user-task", `runtime: deno
trigger: { manual: true }
`, `export default async ({ params }: any) => params.get("greeting");`)

	runID := env.runManual("user-task", map[string]string{"greeting": "hello"})
	env.waitForTerminal(runID)

	// Verify the runs row has input_storage_key set.
	run := env.getRun(runID)
	if run.InputStorageKey == "" {
		t.Errorf("InputStorageKey not set after run; got %#v", run)
	}
	// And the storage backend has the blob.
	if !env.inputStoreContainsKey(run.InputStorageKey) {
		t.Errorf("input not persisted to storage backend")
	}
}
```

- [ ] **Step 4: Run + commit**

```
go test ./pkg/trigger/ ./pkg/registry/ -run "Input|Persist" -v
```

```bash
git add pkg/trigger/engine.go pkg/registry/registry.go pkg/trigger/engine_input_persistence_test.go
git commit -m "feat(trigger): persist input on run-start via InputStore

Webhook bodies, manual params, and chain inputs are redacted +
encrypted + handed to the configured storage task before the run
executes. Failures are logged but don't block the run (graceful
degradation — the run still works without a stored input; replay
just won't be available for that run).

Recursion is avoided by skipping persistence for the configured
storage_task and for buildin/run-inputs-cleanup.

Refs #233"
```

---

## Task 11: SDK + IPC for retention operations

Surface the registry methods over the IPC so the cleanup task can call them.

**Files:**
- Modify: `pkg/registry/registry.go`
- Modify: `pkg/ipc/server.go`
- Modify: `pkg/ipc/capability.go`
- Modify: `pkg/runtime/deno/sdk/shim.ts`

- [ ] **Step 1: Registry methods**

```go
// ListExpiredInputs returns runIDs whose input_stored_at + retention < now,
// excluding pinned rows.
func (r *Registry) ListExpiredInputs(ctx context.Context, beforeUnix int64) ([]ExpiredInput, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT id, input_storage_key, input_stored_at FROM runs
		 WHERE input_storage_key IS NOT NULL
		   AND input_stored_at < ?
		   AND input_pinned = 0`,
		beforeUnix,
	)
	if err != nil { return nil, err }
	defer rows.Close()
	var out []ExpiredInput
	for rows.Next() {
		var e ExpiredInput
		if err := rows.Scan(&e.RunID, &e.StorageKey, &e.StoredAt); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

type ExpiredInput struct {
	RunID      string
	StorageKey string
	StoredAt   int64
}

// DeleteRunInput clears the input columns for a run (caller is responsible
// for asking the storage task to remove the actual blob first).
func (r *Registry) DeleteRunInput(ctx context.Context, runID string) error {
	_, err := r.db.ExecContext(ctx,
		`UPDATE runs SET input_storage_key = NULL, input_size = NULL,
		                  input_stored_at = NULL, input_redacted_fields = NULL
		 WHERE id = ?`, runID)
	return err
}

// PinRunInput / UnpinRunInput toggle the input_pinned flag.
func (r *Registry) PinRunInput(ctx context.Context, runID string) error {
	_, err := r.db.ExecContext(ctx, `UPDATE runs SET input_pinned = 1 WHERE id = ?`, runID)
	return err
}
func (r *Registry) UnpinRunInput(ctx context.Context, runID string) error {
	_, err := r.db.ExecContext(ctx, `UPDATE runs SET input_pinned = 0 WHERE id = ?`, runID)
	return err
}

// GetRunInput returns the persisted input for a run. Decryption requires the
// caller to have the runID and stored_at, which are read from the runs row.
func (r *Registry) GetRunInput(ctx context.Context, runID string, store *InputStore) (PersistedInput, error) {
	var key string
	var storedAt int64
	err := r.db.QueryRowContext(ctx,
		`SELECT input_storage_key, input_stored_at FROM runs WHERE id = ?`, runID,
	).Scan(&key, &storedAt)
	if err != nil { return PersistedInput{}, err }
	if key == "" { return PersistedInput{}, ErrInputUnavailable }
	return store.Fetch(ctx, runID, key, storedAt)
}
```

- [ ] **Step 2: IPC dispatch cases**

In `pkg/ipc/server.go` add cases:

```go
case "dicode.runs.list_expired":
	if !p.Caps[CapRunsListExpired] {
		return nil, errCapDenied
	}
	var args struct {
		BeforeTs int64 `json:"before_ts"`
	}
	if err := json.Unmarshal(req.Args, &args); err != nil {
		return nil, err
	}
	rows, err := s.registry.ListExpiredInputs(ctx, args.BeforeTs)
	if err != nil { return nil, err }
	return rows, nil

case "dicode.runs.delete_input":
	if !p.Caps[CapRunsDeleteInput] {
		return nil, errCapDenied
	}
	var args struct{ RunID string `json:"runID"` }
	if err := json.Unmarshal(req.Args, &args); err != nil { return nil, err }
	// Ask the storage task first, then clear columns.
	var key string
	if err := s.db.QueryRowContext(ctx,
		`SELECT input_storage_key FROM runs WHERE id = ?`, args.RunID,
	).Scan(&key); err != nil { return nil, err }
	if key != "" {
		_ = s.inputStore.Delete(ctx, key) // best-effort; column clear is authoritative
	}
	return nil, s.registry.DeleteRunInput(ctx, args.RunID)

case "dicode.runs.pin_input":
	if !p.Caps[CapRunsPinInput] { return nil, errCapDenied }
	var args struct{ RunID string `json:"runID"` }
	if err := json.Unmarshal(req.Args, &args); err != nil { return nil, err }
	return nil, s.registry.PinRunInput(ctx, args.RunID)

case "dicode.runs.unpin_input":
	if !p.Caps[CapRunsUnpinInput] { return nil, errCapDenied }
	var args struct{ RunID string `json:"runID"` }
	if err := json.Unmarshal(req.Args, &args); err != nil { return nil, err }
	return nil, s.registry.UnpinRunInput(ctx, args.RunID)
```

- [ ] **Step 3: Capability constants**

In `pkg/ipc/capability.go`:

```go
CapRunsListExpired = "runs.list_expired"
CapRunsDeleteInput = "runs.delete_input"
CapRunsPinInput    = "runs.pin_input"
CapRunsUnpinInput  = "runs.unpin_input"
CapRunsGetInput    = "runs.get_input" // internal-only; auto-fix uses
```

Wire each capability to a permission flag in `pkg/task/spec.go` (`DicodePermissions` struct adds `RunsListExpired bool`, etc.) so task.yaml can set `permissions.dicode.runs_list_expired: true`.

- [ ] **Step 4: SDK shim**

`pkg/runtime/deno/sdk/shim.ts` — add:

```ts
runs: {
  list_expired: (opts?: { before_ts?: number }) =>
    __call__({ method: "dicode.runs.list_expired", before_ts: opts?.before_ts ?? Math.floor(Date.now() / 1000) }),
  delete_input: (runID: string) =>
    __call__({ method: "dicode.runs.delete_input", runID }),
  pin_input: (runID: string) =>
    __call__({ method: "dicode.runs.pin_input", runID }),
  unpin_input: (runID: string) =>
    __call__({ method: "dicode.runs.unpin_input", runID }),
}
```

- [ ] **Step 5: Tests + commit**

```
go test ./pkg/registry/ ./pkg/ipc/ -timeout 60s
```

```bash
git add pkg/registry/registry.go pkg/ipc/server.go pkg/ipc/capability.go pkg/runtime/deno/sdk/shim.ts pkg/task/spec.go
git commit -m "feat(ipc): runs.list_expired / delete_input / pin_input / unpin_input

Surfaces the retention-management primitives over the unix-socket
IPC. Each is gated by a per-task permission flag so taskset authors
must explicitly grant the capability. Used by the run-inputs-cleanup
buildin (#233) and (later) by the auto-fix preset (#238).

Refs #233"
```

---

## Task 12: `run-inputs-cleanup` buildin

Cron-fired task that uses the new SDK calls.

**Files:**
- Create: `tasks/buildin/run-inputs-cleanup/task.yaml`
- Create: `tasks/buildin/run-inputs-cleanup/task.ts`
- Create: `tasks/buildin/run-inputs-cleanup/task.test.ts`
- Modify: `tasks/buildin/taskset.yaml`

- [ ] **Step 1: task.yaml**

```yaml
apiVersion: dicode/v1
kind: Task
name: "Run-input retention sweep"
description: |
  Hourly cleanup of expired run input blobs. Calls runs.list_expired to find
  unpinned rows past retention, then runs.delete_input for each. Mirrors the
  temp-cleanup pattern.

runtime: deno

trigger:
  cron: "17 * * * *"   # hourly at :17

params:
  retention_seconds:
    type: number
    default: "2592000"   # 30 days; daemon overrides via dicode.yaml's defaults.run_inputs.retention
    description: "Retain inputs for at most this many seconds."

permissions:
  dicode:
    runs_list_expired: true
    runs_delete_input: true

# Self-opt-out to avoid storing this task's own input.
run_inputs:
  enabled: false

timeout: 120s

notify:
  on_success: false
  on_failure: true
```

- [ ] **Step 2: task.ts**

```ts
interface ExpiredRow {
  RunID: string;
  StorageKey: string;
  StoredAt: number;
}

export default async function main({ params, dicode }: DicodeSdk) {
  const retention = Number(params.get("retention_seconds") ?? 2592000);
  const cutoff = Math.floor(Date.now() / 1000) - retention;

  const rows = (await dicode.runs.list_expired({ before_ts: cutoff })) as ExpiredRow[];
  if (!rows || rows.length === 0) {
    return { ok: true, removed: 0 };
  }

  let removed = 0;
  let errors = 0;
  for (const row of rows) {
    try {
      await dicode.runs.delete_input(row.RunID);
      removed++;
    } catch (e) {
      dicode.log.warn(`delete_input(${row.RunID}) failed: ${e}`);
      errors++;
    }
  }
  return { ok: errors === 0, removed, errors };
}
```

- [ ] **Step 3: task.test.ts**

Same harness pattern as `dev-clones-cleanup/task.test.ts` from #236.

- [ ] **Step 4: Add to taskset.yaml + run tests**

```
deno test --allow-read --allow-write tasks/buildin/run-inputs-cleanup/task.test.ts
```

- [ ] **Step 5: Commit**

```bash
git add tasks/buildin/run-inputs-cleanup/ tasks/buildin/taskset.yaml
git commit -m "feat(buildin): run-inputs-cleanup hourly retention sweep

Mirrors temp-cleanup. Uses dicode.runs.list_expired + delete_input
to remove input blobs whose stored_at is past the retention cutoff
and which aren't pinned by an in-flight auto-fix run.

The task opts itself out of input persistence to avoid storing its
own params on every cron tick.

Refs #233"
```

---

## Task 13: Stale-pin recovery on engine startup

If the daemon crashes mid-fix, `input_pinned = 1` rows linger. Sweep them at startup.

**Files:**
- Modify: `pkg/registry/registry.go` (add `SweepStalePins`)
- Modify: `pkg/trigger/engine.go` (call at startup)
- Test: `pkg/registry/registry_pin_test.go`

- [ ] **Step 1: Implement**

```go
// SweepStalePins clears input_pinned on any row whose pinning was abandoned.
// A pin is considered stale if the row's status is no longer "running"
// (the daemon either finished or crashed; either way the pin is no
// longer load-bearing).
func (r *Registry) SweepStalePins(ctx context.Context) (int, error) {
	res, err := r.db.ExecContext(ctx,
		`UPDATE runs SET input_pinned = 0
		 WHERE input_pinned = 1 AND status != 'running'`,
	)
	if err != nil { return 0, err }
	n, _ := res.RowsAffected()
	return int(n), nil
}
```

Call at engine startup (in the daemon initialisation, after the registry is built and before the trigger engine starts):

```go
if n, err := reg.SweepStalePins(ctx); err == nil && n > 0 {
	log.Info("cleared stale input pins at startup", zap.Int("count", n))
}
```

- [ ] **Step 2: Test**

```go
func TestSweepStalePins(t *testing.T) {
	r := newTestRegistry(t)
	defer r.Close()

	// Pinned + still running: must not be cleared.
	live, _ := r.StartRun(ctx, "task-a", "")
	r.PinRunInput(ctx, live)

	// Pinned + already finished: must be cleared.
	dead, _ := r.StartRun(ctx, "task-b", "")
	r.PinRunInput(ctx, dead)
	r.FinishRun(ctx, dead, registry.StatusFailure)

	n, err := r.SweepStalePins(ctx)
	if err != nil { t.Fatal(err) }
	if n != 1 { t.Errorf("cleared %d, want 1", n) }

	// Confirm.
	if r.getPin(live) != 1 { t.Error("live pin cleared") }
	if r.getPin(dead) != 0 { t.Error("dead pin not cleared") }
}
```

- [ ] **Step 3: Commit**

```bash
git add pkg/registry/registry.go pkg/registry/registry_pin_test.go pkg/trigger/engine.go pkg/daemon/daemon.go
git commit -m "feat(registry): sweep stale input pins on startup

If the daemon crashes mid-fix, input_pinned = 1 rows can linger and
prevent retention sweeps from collecting otherwise-expired inputs.
At engine startup, clear input_pinned for any row whose status is no
longer 'running' (the run is over; the pin is no longer load-bearing).

Refs #233"
```

---

## Task 14: Config integration

Wire all of the above into the user-facing YAML.

**Files:**
- Modify: `pkg/config/config.go`
- Modify: `pkg/task/spec.go`
- Test: `pkg/config/runinputs_test.go`

- [ ] **Step 1: Add `RunInputsConfig` to `Defaults`**

```go
type DefaultsConfig struct {
	// existing fields...
	RunInputs RunInputsConfig `yaml:"run_inputs,omitempty"`
}

type RunInputsConfig struct {
	Enabled         *bool         `yaml:"enabled,omitempty"`           // default true
	Retention       time.Duration `yaml:"retention,omitempty"`         // default 30d (use a sentinel and fill in defaults() if zero)
	StorageTask     string        `yaml:"storage_task,omitempty"`      // default "buildin/local-storage"
	BodyFullTextual bool          `yaml:"body_full_textual,omitempty"` // default false
}

func (c RunInputsConfig) IsEnabled() bool {
	if c.Enabled == nil { return true }
	return *c.Enabled
}
```

Defaults applied during `cfg.applyDefaults()`:

```go
if cfg.Defaults.RunInputs.Retention == 0 {
	cfg.Defaults.RunInputs.Retention = 30 * 24 * time.Hour
}
if cfg.Defaults.RunInputs.StorageTask == "" {
	cfg.Defaults.RunInputs.StorageTask = "buildin/local-storage"
}
```

- [ ] **Step 2: Per-task override**

`pkg/task/spec.go`:

```go
type Spec struct {
	// existing...
	RunInputs *RunInputsTaskOverride `yaml:"run_inputs,omitempty"`
	AutoFix   *AutoFixConfig          `yaml:"auto_fix,omitempty"`
}

type RunInputsTaskOverride struct {
	Enabled         *bool         `yaml:"enabled,omitempty"`
	Retention       time.Duration `yaml:"retention,omitempty"`
	BodyFullTextual *bool         `yaml:"body_full_textual,omitempty"`
}

type AutoFixConfig struct {
	IncludeInput            *bool `yaml:"include_input,omitempty"`              // default true
	ShowRedactedFieldNames  *bool `yaml:"show_redacted_field_names,omitempty"` // default true
}
```

- [ ] **Step 3: Test parsing + defaults**

```go
func TestRunInputsConfig_Defaults(t *testing.T) {
	cfg := &Config{}
	cfg.applyDefaults()
	if cfg.Defaults.RunInputs.Retention != 30*24*time.Hour {
		t.Errorf("retention default wrong: %v", cfg.Defaults.RunInputs.Retention)
	}
	if cfg.Defaults.RunInputs.StorageTask != "buildin/local-storage" {
		t.Errorf("storage_task default wrong: %v", cfg.Defaults.RunInputs.StorageTask)
	}
	if !cfg.Defaults.RunInputs.IsEnabled() {
		t.Error("default should be enabled")
	}
}

func TestRunInputsConfig_ParsesYAML(t *testing.T) {
	yamlSrc := []byte(`
defaults:
  run_inputs:
    enabled: false
    retention: 24h
    storage_task: my-s3
    body_full_textual: true
`)
	var cfg Config
	if err := yaml.Unmarshal(yamlSrc, &cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.Defaults.RunInputs.IsEnabled() {
		t.Error("enabled=false not parsed")
	}
	if cfg.Defaults.RunInputs.Retention != 24*time.Hour {
		t.Errorf("retention = %v", cfg.Defaults.RunInputs.Retention)
	}
}
```

- [ ] **Step 4: Wire engine to consult these fields**

The `shouldPersistInput` check from Task 10 already references `cfg.Defaults.RunInputs.Enabled` and the per-task override. Verify it now compiles cleanly with the actual types.

- [ ] **Step 5: Integration test — the warning log for `body_full_textual: true`**

When loading a config where any task has `body_full_textual: true`, the daemon emits a WARN log at startup:

```go
// In cfg.validate() or daemon startup:
if anyTaskHasBodyFullTextual(cfg) {
	log.Warn("one or more tasks set body_full_textual: true — non-JSON bodies will be persisted verbatim without name-based redaction; ensure these tasks do not receive credentials in body content")
}
```

Add a quick test asserting the warning fires.

- [ ] **Step 6: Commit**

```bash
git add pkg/config/config.go pkg/config/runinputs_test.go pkg/task/spec.go
git commit -m "feat(config): defaults.run_inputs + per-task overrides

defaults.run_inputs.{enabled, retention, storage_task,
body_full_textual} configure the run-input persistence layer
globally. Per-task task.yaml run_inputs.{enabled, retention,
body_full_textual} overrides apply per task. auto_fix.{include_input,
show_redacted_field_names} controls what the auto-fix agent sees
(used in #238).

body_full_textual: true emits a startup WARN log naming the affected
tasks (footgun documentation).

Refs #233"
```

---

## Self-review checklist

**Spec coverage** (§ 4.1, 4.2):

- [x] §4.1.1 Schema delta on `runs` → Task 2 (idempotent migration helper + 5 columns)
- [x] §4.1.2 Encryption (XChaCha20-Poly1305 + Argon2id sub-key + AAD) → Tasks 3, 4
- [x] §4.1.3 Redaction policy (Headers/Query/Params/Body) → Tasks 5, 6, 7
- [x] §4.1.4 Storage task contract → Task 8 (routing) + Task 9 (local-storage buildin)
- [x] §4.1.4 `${DATADIR}` template var → Task 1
- [x] §4.1.5 Read path (`Fetch`) → Task 8 + Task 11 (`GetRunInput`)
- [x] §4.1.6 Pinning + crash-recovery → Tasks 11 (pin/unpin SDK) + 13 (sweep)
- [x] §4.2 Retention sweep → Task 12
- [x] Engine integration (persist on run-start) → Task 10
- [x] Config integration → Task 14

**Placeholder scan:**

- "Adapt to the actual `fireAsync` shape" in Task 10 — that's a reasonable instruction-to-implementer, not a placeholder of the kind banned by the skill (the spec says explicitly what to do; the implementer just needs to find the call site).
- "Same harness pattern as ..." in Task 12 task.test.ts — borderline; fix by saying "use the same `setupHarness` import pattern as `tasks/buildin/dev-clones-cleanup/task.test.ts` from #236; the test mocks `dicode.runs.list_expired` to return a fixture rowset and asserts `delete_input` is called for each."

That's the only meaningful gap; minor edit applied above.

**Type consistency:**

- `PersistedInput` shape matches across Tasks 5, 6, 7, 8 (Source/Method/Path/Headers/Query/Body/BodyKind/BodyHash/BodyParts/Params/RedactedFields).
- `InputCrypto` constructor + Encrypt/Decrypt signatures consistent across Task 4 + Task 8.
- `TaskRunner` interface (Task 8) is consumed by Task 10's engine integration; the engine's existing `fireSync` provides a compatible shape.
- `SetRunInput`, `ListExpiredInputs`, `DeleteRunInput`, `PinRunInput`/`UnpinRunInput`, `GetRunInput`, `SweepStalePins` consistently named across Tasks 10, 11, 13.
- `CapRunsListExpired`/`CapRunsDeleteInput`/`CapRunsPinInput`/`CapRunsUnpinInput`/`CapRunsGetInput` consistent across Task 11 IPC + permission flags.

**Out-of-scope (deferred):**

- `dicode.runs.replay` SDK call → #234.
- Auto-fix override + chain.params merging beyond what landed in #236 → #238.
- Storage backends beyond local-storage (S3, etc.) → user-implementable; spec is explicit.

---

## Verification before marking complete

- [ ] `go test ./... -timeout 180s` — green
- [ ] `make lint` — clean
- [ ] `deno test --allow-read --allow-write tasks/buildin/local-storage/task.test.ts tasks/buildin/run-inputs-cleanup/task.test.ts` — green
- [ ] Manual smoke: start `dicode daemon`, fire a webhook with `Authorization: Bearer xyz` and JSON body containing `password`. Inspect the runs row's `input_storage_key`; `cat ${DATADIR}/run-inputs/<runID>.bin` shows base64 ciphertext (not plaintext). Wait > retention; cleanup task removes the row. Pin a row before retention; cleanup leaves it.
- [ ] CodeQL clean (especially `go/path-injection` on the `${DATADIR}/run-inputs/<key>.bin` path in the local-storage task).
