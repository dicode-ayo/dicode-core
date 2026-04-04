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

### Future providers

All use the same `Provider` interface — no task code changes needed when you switch providers:

| Type | Description |
|---|---|
| `vault` | HashiCorp Vault (kv-v2 or kv-v1) |
| `aws-secrets-manager` | AWS Secrets Manager |
| `gcp-secret-manager` | GCP Secret Manager |
| `doppler` | Doppler secrets platform |
| `1password` | 1Password Connect |
| `infisical` | Infisical open-source secrets manager |

Example (future):
```yaml
secrets:
  providers:
    - type: local
    - type: vault
      address: https://vault.example.com
      token_env: VAULT_TOKEN
      mount: secret
    - type: env
```

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
