# Secrets

Tasks need API keys, tokens, and passwords to do anything useful. Dicode manages secrets through a **provider chain** — a prioritized list of secret backends tried in order until the key is found.

Secrets are **never stored in task files**. Tasks only declare which keys they need.

---

## Provider chain

```yaml
secrets:
  providers:
    - type: local     # encrypted SQLite, checked first
    - type: env       # host environment variables, fallback
```

When a task needs `SLACK_TOKEN`, dicode tries each provider in order:
1. `local` — look for `SLACK_TOKEN` in the encrypted local store
2. `env` — look for `SLACK_TOKEN` in host environment variables
3. If not found in any provider → runtime error, run marked as `failed`

You can reorder providers or add/remove them in `dicode.yaml`.

---

## Providers

### `local` — encrypted local store

Secrets stored in the local encrypted SQLite database. This is the primary store for most users.

**Encryption:** ChaCha20-Poly1305 with a per-value random nonce. Keys are derived from a master key using Argon2id (resistant to brute force even if the DB file is stolen).

**Master key:** auto-generated on first use as `~/.dicode/master.key` (chmod 600). You can override with the `DICODE_MASTER_KEY` environment variable (useful for headless/Docker deployments where the key is injected from a secrets manager).

**CLI:**
```bash
dicode secrets set SLACK_TOKEN xoxb-1234-5678
dicode secrets get SLACK_TOKEN      # prints value
dicode secrets list                 # lists names only, never values
dicode secrets delete SLACK_TOKEN
```

**What's stored:** only the ciphertext. The plaintext value is only available in memory after decryption, during task execution.

### `env` — host environment variables

Reads from the process environment (`os.Getenv`). Zero configuration. Useful as a fallback for CI environments or when you're already managing secrets via a secrets manager that injects env vars.

```bash
SLACK_TOKEN=xoxb-... dicode
# or set in systemd unit, Docker env, etc.
```

### Future native providers

Native Go providers (vault, aws-secrets-manager, gcp-secret-manager) remain
on the roadmap but are not the recommended integration path. Most users
should reach for **provider tasks** instead — see below.

---

## Provider tasks (task: prefix)

For Doppler, 1Password, HashiCorp Vault, and other external secret stores,
dicode resolves secrets by spawning a normal task that calls the upstream
API. No daemon release is required to add a new provider — ship a task
folder and reference it from `from: task:<id>`.

### Consumer side — `from: task:<provider-id>`

```yaml
# tasks/my-app/task.yaml
name: "My App"
runtime: deno
trigger:
  cron: "*/5 * * * *"
permissions:
  env:
    - name: PG_URL
      from: task:secret-providers/doppler   # resolved via the doppler provider task
    - name: REDIS_URL
      from: task:secret-providers/doppler   # batched: one spawn for both
      optional: true
    - name: LOG_LEVEL
      from: env:LOG_LEVEL                   # explicit host env
```

The daemon groups every `from: task:<id>` entry by provider, spawns each
provider once per consumer launch, caches the result with the provider's
declared TTL, and merges the resolved values into the consumer's process
environment before launching it.

### Provider side — `dicode.output(map, { secret: true })`

A provider is any task that emits its resolved secrets via the secret-flag
overload of `output`:

```yaml
# tasks/buildin/secret-providers/doppler/task.yaml
name: "Doppler Secret Provider"
runtime: deno
trigger:
  manual: true
permissions:
  env:
    - name: DOPPLER_TOKEN
      secret: DOPPLER_TOKEN     # bootstrap once via `dicode secrets set DOPPLER_TOKEN ...`
  net:
    - api.doppler.com
provider:
  cache_ttl: 5m                  # 0 / omitted = no caching
```

```typescript
// task.ts
export default async function main({ params, output }: DicodeSdk) {
  const reqs = JSON.parse(await params.get("requests") ?? "[]");
  const out: Record<string, string> = {};
  // ... call upstream, populate `out` ...
  await output(out, { secret: true });
}
```

Daemon-side semantics for `secret: true`:

- Run-log records keys with `[redacted]` placeholders only — values never hit disk.
- Values feed the run-log redactor on the consumer launch (so a `console.log`
  of a resolved value is scrubbed).
- The map is returned to the resolver awaiting this provider call.
- Output must be a flat `Record<string, string>` — the daemon refuses
  nested objects.

### Failure modes

| Reason | When it fires |
|---|---|
| `provider_unavailable: <id>` | provider task crashed, timed out, or returned no map |
| `required_secret_missing: <KEY> from <id>` | provider returned a map but a non-optional KEY was absent |
| `provider_misconfigured: <id>` | task referenced via `task:<id>` is not a provider (missing `secret: true` flag) |

All three are recorded as the consumer run's `fail_reason` and trigger
the configured `on_failure_chain`. The provider's own run also fires its
own chain on its own crash.

### Built-in providers

| Path | Upstream | Bootstrap secret |
|---|---|---|
| `buildin/secret-providers/doppler` | Doppler REST API | `DOPPLER_TOKEN` |

More providers (`buildin/secret-providers/onepassword`, `vault`, …) ship
under the same path. Authoring your own takes a `task.yaml` + `task.ts`
pair — see [task-format](task-format.md).

---

## Declaring secrets in tasks

Tasks declare which secrets they need in `task.yaml`. This is the only place secrets appear in task files.

```yaml
# task.yaml
env:
  - SLACK_TOKEN
  - GMAIL_TOKEN
  - OPENAI_API_KEY
```

At runtime, `Deno.env.get("SLACK_TOKEN")` in a Deno task reads the variable, which is injected by the runtime after resolving it through the provider chain. The task doesn't know or care which provider holds the value.

**Boundary enforcement:** tasks can only access env vars declared in their `task.yaml`. The Deno `--allow-env` flag is scoped to declared vars, so `Deno.env.get()` calls for undeclared keys throw a permission error. This prevents tasks from fishing for arbitrary environment variables.

---

## Secret validation

`dicode task validate` checks that all declared secrets are resolvable:

```bash
dicode task validate morning-email-check
# ✅ task.yaml valid
# ⚠️  SLACK_TOKEN not found in any configured provider
# ⚠️  GMAIL_TOKEN not found in any configured provider
```

Missing secrets are warnings (not errors) at validate time, since secrets may be set up later. At runtime, a missing secret is a hard error.

---

## Security notes

- The encrypted SQLite file (`~/.dicode/dicode.db`) is only as secure as the master key. Protect `~/.dicode/master.key` (it's already chmod 600).
- On shared machines, consider using `DICODE_MASTER_KEY` from a hardware security module or OS keychain rather than the default file.
- Secrets are decrypted **in memory only**, for the duration of each task run. They are never written to the run log or exposed through the API.
- `dicode secrets list` shows secret names but never values.
- The MCP tool `list_secrets` also shows names only — AI agents can see what secrets exist but not their values.
