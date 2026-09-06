# Changelog

## [0.6.2](https://github.com/dicode-ayo/dicode-core/compare/v0.6.1...v0.6.2) (2026-09-06)


### Features

* **webui:** accept fire-time params on the manual run endpoint ([#836](https://github.com/dicode-ayo/dicode-core/issues/836)) ([35c5154](https://github.com/dicode-ayo/dicode-core/commit/35c51549857978fbc1b2de32f4e576cd8c979e9c))


### Bug Fixes

* **cli:** say when a data dir already holds a dashboard passphrase ([#834](https://github.com/dicode-ayo/dicode-core/issues/834)) ([6175180](https://github.com/dicode-ayo/dicode-core/commit/61751803aaa0712732662775b552b2e8ee7fad38))
* **tasks:** stop cron-firing ops/relay-edge with unsatisfiable required params ([#837](https://github.com/dicode-ayo/dicode-core/issues/837)) ([a7f6106](https://github.com/dicode-ayo/dicode-core/commit/a7f6106a97e4329ae6a1dc7b145e1c70b1537d1b))

## [0.6.1](https://github.com/dicode-ayo/dicode-core/compare/v0.6.0...v0.6.1) (2026-09-05)


### Features

* **taskset:** pin a git source to a tag instead of a moving branch ([#829](https://github.com/dicode-ayo/dicode-core/issues/829)) ([733561f](https://github.com/dicode-ayo/dicode-core/commit/733561fc7b1c0ddd50f384f454b41c2e254f0293))


### Bug Fixes

* **approval:** stop notifying and warning about a disabled task's pending hold ([#830](https://github.com/dicode-ayo/dicode-core/issues/830)) ([7ae2d92](https://github.com/dicode-ayo/dicode-core/commit/7ae2d926856543e4744bc81dd21ce8efc9e8258d))

## [0.6.0](https://github.com/dicode-ayo/dicode-core/compare/v0.5.3...v0.6.0) (2026-09-03)


### ⚠ BREAKING CHANGES

* an existing dicode.yaml pointing `buildin` at a path under this repo's tasks/ resolves nothing after upgrading. Repoint it at the git ref, as the updated onboarding preset now does.

### Bug Fixes

* **run-inputs,telegram:** unbreak the run-input sweep and link failed runs from notifications ([#818](https://github.com/dicode-ayo/dicode-core/issues/818)) ([ed6b3bf](https://github.com/dicode-ayo/dicode-core/commit/ed6b3bf74f3ceadfa0f95341076107bcebbef744))
* **tasktest:** stop rejecting test runs over params the run never reads ([#828](https://github.com/dicode-ayo/dicode-core/issues/828)) ([5e3612f](https://github.com/dicode-ayo/dicode-core/commit/5e3612f1d9768bee2cf0fe49aef406427e2a94b1))
* **tray:** point the no-SNI-host guidance at advice that actually works ([#824](https://github.com/dicode-ayo/dicode-core/issues/824)) ([c78fb5a](https://github.com/dicode-ayo/dicode-core/commit/c78fb5a705a488459b69b7f2054717e123c95930))


### Documentation

* **params:** fence the params_invalid example and record [#812](https://github.com/dicode-ayo/dicode-core/issues/812) in the changelog ([#814](https://github.com/dicode-ayo/dicode-core/issues/814)) ([ba01df9](https://github.com/dicode-ayo/dicode-core/commit/ba01df952b7fea3d090f99335551adcef613e8e3))


### Miscellaneous

* move the buildin taskset out to dicode-ayo/dicode-buildin ([#826](https://github.com/dicode-ayo/dicode-core/issues/826)) ([ff14f84](https://github.com/dicode-ayo/dicode-core/commit/ff14f848693a151af2bf7e00e6d37bcddedc59b9))

## [0.5.3](https://github.com/dicode-ayo/dicode-core/compare/v0.5.2...v0.5.3) (2026-09-02)


### Features

* **config:** add server.public_url for off-host notification links ([#805](https://github.com/dicode-ayo/dicode-core/issues/805)) ([f56dcfc](https://github.com/dicode-ayo/dicode-core/commit/f56dcfcd1d67711c2192a59f5233e1afe8d6156e)), closes [#796](https://github.com/dicode-ayo/dicode-core/issues/796)
* **telegram:** add buildin/telegram notification delivery ([8089cb6](https://github.com/dicode-ayo/dicode-core/commit/8089cb6f9e037c2615bce2e721f064f9eea8a519))


### Bug Fixes

* **approval:** render title/body in the approval notify hook ([#811](https://github.com/dicode-ayo/dicode-core/issues/811)) ([5e05768](https://github.com/dicode-ayo/dicode-core/commit/5e05768ff41501f256fc2dd79e30e46c5092aacf)), closes [#797](https://github.com/dicode-ayo/dicode-core/issues/797)
* **params:** enforce required params on the fire path, not only in task test ([#812](https://github.com/dicode-ayo/dicode-core/issues/812)) ([a9e52ca](https://github.com/dicode-ayo/dicode-core/commit/a9e52caac2281b4f43a2057c5fa5abf87d759f85))
* **webui:** send unauthenticated browsers to /login, not to the gated root ([#809](https://github.com/dicode-ayo/dicode-core/issues/809)) ([331132c](https://github.com/dicode-ayo/dicode-core/commit/331132c13e1a566ffc770597731c0ef6e9c37f9f)), closes [#808](https://github.com/dicode-ayo/dicode-core/issues/808)


### Documentation

* correct two CLAUDE.md claims that misdescribe the runtime ([#804](https://github.com/dicode-ayo/dicode-core/issues/804)) ([f6ac92b](https://github.com/dicode-ayo/dicode-core/commit/f6ac92b98e711c127bfecedebb2c6e3319ffab69))

## [0.5.2](https://github.com/dicode-ayo/dicode-core/compare/v0.5.1...v0.5.2) (2026-09-01)


### Bug Fixes

* **ai-agent:** a not_configured turn settles as a successful run ([#770](https://github.com/dicode-ayo/dicode-core/issues/770)) ([22b6a5e](https://github.com/dicode-ayo/dicode-core/commit/22b6a5e41cd2a7cdf7c9303f4f070dc29a9cd0e8))
* **authoring:** deleteGit removes the taskset entry, not just the directory ([#774](https://github.com/dicode-ayo/dicode-core/issues/774)) ([2c3fbe4](https://github.com/dicode-ayo/dicode-core/commit/2c3fbe4afdfd63da65e51123152e246128a27c2b))
* **authoring:** refuse a session whose sandbox the write tool cannot accept ([#769](https://github.com/dicode-ayo/dicode-core/issues/769)) ([10eef22](https://github.com/dicode-ayo/dicode-core/commit/10eef2216ea815850d8b7253833be5163b4e5b2f))
* **authoring:** scaffold task.ts instead of task.js ([#771](https://github.com/dicode-ayo/dicode-core/issues/771)) ([96d2313](https://github.com/dicode-ayo/dicode-core/commit/96d2313326c1f44e49dc9258282a0bfe967eb268))
* **cli:** run onboarding from init, and stop losing the first-run passphrase ([c57021d](https://github.com/dicode-ayo/dicode-core/commit/c57021def7866a4a9fbce896a68d99acd317399f))
* **deps:** update module github.com/cenkalti/backoff/v4 to v7 ([#791](https://github.com/dicode-ayo/dicode-core/issues/791)) ([653ebc8](https://github.com/dicode-ayo/dicode-core/commit/653ebc82989b7b6208b5ca01971f026a4988c2e5))
* **runtime:** route provider secret-output channel through per-run RunOptions ([#792](https://github.com/dicode-ayo/dicode-core/issues/792)) ([02bd870](https://github.com/dicode-ayo/dicode-core/commit/02bd870e3348e6e18c6d749f368b1d0613c1fa45))
* **taskset:** use the namespaced buildin/git-pr id in the agents' allowlist ([#773](https://github.com/dicode-ayo/dicode-core/issues/773)) ([f0cf3c1](https://github.com/dicode-ayo/dicode-core/commit/f0cf3c1b74840468f74226acc78d401088d97e6e))
* **webui:** wait for the reconciler to start before driving AddSource in tests ([#775](https://github.com/dicode-ayo/dicode-core/issues/775)) ([1344958](https://github.com/dicode-ayo/dicode-core/commit/134495844d86db1a200967909a67a4a54199734a))

## [0.5.1](https://github.com/dicode-ayo/dicode-core/compare/v0.5.0...v0.5.1) (2026-08-24)


### Features

* **ai:** authoring Phase 0 — ship buildin/task-create + thread the prompt ([#659](https://github.com/dicode-ayo/dicode-core/issues/659)) ([c53b691](https://github.com/dicode-ayo/dicode-core/commit/c53b691579eaea2e326212ed456214db6dd055bd))
* **ai:** give the agent the dicode capabilities its taskset declares ([#735](https://github.com/dicode-ayo/dicode-core/issues/735)) ([062f617](https://github.com/dicode-ayo/dicode-core/commit/062f6170ea25079363f99c7a0232138dbdaac6e3))
* **mcp:** make switch_dev_mode, test_task and list_sources act ([#724](https://github.com/dicode-ayo/dicode-core/issues/724)) ([22f0fb1](https://github.com/dicode-ayo/dicode-core/commit/22f0fb145ddf6184a8bd9b77f1e2aa7d3a77f852))


### Bug Fixes

* **ai-agent-claude-cli:** fail the run when a turn cannot run ([#749](https://github.com/dicode-ayo/dicode-core/issues/749)) ([a357e39](https://github.com/dicode-ayo/dicode-core/commit/a357e39364a67d318cbdfb2101cde4fcb4bb6df1))
* **ai-agent:** advertise skills and hand the bodies out on demand ([#759](https://github.com/dicode-ayo/dicode-core/issues/759)) ([48d14fc](https://github.com/dicode-ayo/dicode-core/commit/48d14fc879694be8dad801d5178d3e2b768aba59)), closes [#757](https://github.com/dicode-ayo/dicode-core/issues/757)
* **ai-agent:** send an explicit temperature, and show the authoring prompt a real task.yaml ([#758](https://github.com/dicode-ayo/dicode-core/issues/758)) ([3a7f3b6](https://github.com/dicode-ayo/dicode-core/commit/3a7f3b6c7608da60d4bf6300856e8f30431d61cf))
* **ai:** give the authoring agent a way to put files on disk ([#734](https://github.com/dicode-ayo/dicode-core/issues/734)) ([#738](https://github.com/dicode-ayo/dicode-core/issues/738)) ([c013bbb](https://github.com/dicode-ayo/dicode-core/commit/c013bbb8059e9b3710cde53913f579d8bf859ed8))
* **ai:** install skills in the layout the Claude CLI actually loads ([#733](https://github.com/dicode-ayo/dicode-core/issues/733)) ([#744](https://github.com/dicode-ayo/dicode-core/issues/744)) ([886c8b0](https://github.com/dicode-ayo/dicode-core/commit/886c8b04594405ac737a4716923df69b3be81309))
* **authoring:** check an AI turn's claims against disk before calling it a success ([#760](https://github.com/dicode-ayo/dicode-core/issues/760)) ([2c6b858](https://github.com/dicode-ayo/dicode-core/commit/2c6b858321125e94bf3d8b5b2c84fe3bbe98d97f)), closes [#755](https://github.com/dicode-ayo/dicode-core/issues/755)
* **authoring:** register the task `task create` scaffolds ([#728](https://github.com/dicode-ayo/dicode-core/issues/728)) ([7779598](https://github.com/dicode-ayo/dicode-core/commit/777959838bd453777d53c94010b04a03d9927091))
* **taskset:** expand ${VAR} in inline entry base specs ([#762](https://github.com/dicode-ayo/dicode-core/issues/762)) ([64ef623](https://github.com/dicode-ayo/dicode-core/commit/64ef62389512013d4780f800d3baec339b1cfd31))
* **taskset:** expand ${VAR} in override permission paths ([#725](https://github.com/dicode-ayo/dicode-core/issues/725)) ([9908923](https://github.com/dicode-ayo/dicode-core/commit/99089230328110e54654c05c4bf06249edbd7ac2))
* **taskset:** expand ${VAR} in trigger.chain.overrides supplied via an override layer ([#764](https://github.com/dicode-ayo/dicode-core/issues/764)) ([92e3a7e](https://github.com/dicode-ayo/dicode-core/commit/92e3a7e45fbd11c3b274a17285f4cc629406d767))
* **test:** surface cleanup failures in withPendingWebhookChange ([#716](https://github.com/dicode-ayo/dicode-core/issues/716)) ([096f1a2](https://github.com/dicode-ayo/dicode-core/commit/096f1a24db99cddc39c4120bf2e12f4187ec3cd6))

## [0.5.0](https://github.com/dicode-ayo/dicode-core/compare/v0.4.0...v0.5.0) (2026-08-18)


### ⚠ BREAKING CHANGES

* **relay:** relay.server_url must now be wss:// and point at the broker's mTLS port; relay.broker_url is effectively required (separate public-listener port). A ws:// server_url is rejected at config load. Requires dicode-relay 0.2.0 (protocol v4) — v3 daemons/brokers are incompatible. The mTLS port needs end-to-end TLS passthrough (never behind a TLS-terminating proxy). 'dicode relay trust-broker' is removed.

### Features

* **ai-agent-claude-cli:** dual-mode auth — token optional, fall back to login ([#563](https://github.com/dicode-ayo/dicode-core/issues/563)) ([e7f0fe8](https://github.com/dicode-ayo/dicode-core/commit/e7f0fe83a5dc8d2819b770ef93174c4c6153c062))
* **ai:** ai-agent-claude-cli suspend/resume chat loop (first slice of [#571](https://github.com/dicode-ayo/dicode-core/issues/571)) ([#578](https://github.com/dicode-ayo/dicode-core/issues/578)) ([93dc9cc](https://github.com/dicode-ayo/dicode-core/commit/93dc9cc0390bb9a344af7acbeb3a080acc5766c1))
* **ai:** relay-reachable HMAC auth on the AI-chat webhook ([#611](https://github.com/dicode-ayo/dicode-core/issues/611)) ([#615](https://github.com/dicode-ayo/dicode-core/issues/615)) ([2dec976](https://github.com/dicode-ayo/dicode-core/commit/2dec976b0b219d121db7cbd00c36169efacd0d73))
* **ai:** retire KV session-threading from both agent tasks (slice of [#571](https://github.com/dicode-ayo/dicode-core/issues/571)b) ([#598](https://github.com/dicode-ayo/dicode-core/issues/598)) ([f4b617d](https://github.com/dicode-ayo/dicode-core/commit/f4b617ddd6d7ee96f679e142ce706f5b401c5fa6))
* **ai:** shared suspend/resume chat envelope for both agent tasks ([#571](https://github.com/dicode-ayo/dicode-core/issues/571), first slice) ([#594](https://github.com/dicode-ayo/dicode-core/issues/594)) ([d13b434](https://github.com/dicode-ayo/dicode-core/commit/d13b434db718e2d1545c3e91759648dc169257b6))
* **approval:** record the approved commit in dicode.lock ([#676](https://github.com/dicode-ayo/dicode-core/issues/676)) ([6cd7557](https://github.com/dicode-ayo/dicode-core/commit/6cd75573f1f821b0d389c1b2182c95730a5681e2))
* **approval:** show the diff of the pending change in the Approve action ([#636](https://github.com/dicode-ayo/dicode-core/issues/636)) ([a02ef60](https://github.com/dicode-ayo/dicode-core/commit/a02ef60fbdca57002f4aa55f003f5ce52404fea9))
* buildin/blob-storage — generic per-task blob store for user data ([#452](https://github.com/dicode-ayo/dicode-core/issues/452)) ([3176bf4](https://github.com/dicode-ayo/dicode-core/commit/3176bf4edce5757ffd7ec13fb4eda597ab8a440a))
* **cli:** dicode ai with no prompt opens an interactive suspend/resume chat ([#596](https://github.com/dicode-ayo/dicode-core/issues/596)) ([6235985](https://github.com/dicode-ayo/dicode-core/commit/62359856acd42b1ac919c0b6dc8c21be9a00e0ba))
* **cli:** dicode deno relock — pinned-Deno lockfile guard + CI check ([#463](https://github.com/dicode-ayo/dicode-core/issues/463)) ([c2442ad](https://github.com/dicode-ayo/dicode-core/commit/c2442ad3e9657416cd7fe418b9450b6075b3ec74))
* **cli:** dicode init — scaffold a git-versionable root taskset directory ([#658](https://github.com/dicode-ayo/dicode-core/issues/658)) ([91f0f41](https://github.com/dicode-ayo/dicode-core/commit/91f0f4108b6a7406cd2174da16cffe30524dad20))
* **cli:** dicode webhook sign — HMAC signing helper for protected webhooks ([#633](https://github.com/dicode-ayo/dicode-core/issues/633)) ([275d667](https://github.com/dicode-ayo/dicode-core/commit/275d667efbb946cea7d073844748ff7bd18fb0f1))
* **cli:** interactive follow mode for run/resume suspend wizards ([#534](https://github.com/dicode-ayo/dicode-core/issues/534)) ([2fa5d51](https://github.com/dicode-ayo/dicode-core/commit/2fa5d515e1607fd1204c7b11a062875aa3760059))
* **cli:** pre-supply suspend-wizard answers with --field to auto-advance run/resume ([#538](https://github.com/dicode-ayo/dicode-core/issues/538)) ([2ae0dec](https://github.com/dicode-ayo/dicode-core/commit/2ae0dec1939773e72e0c7a39c848e60e5ebe8954))
* **cli:** progress spinner, input-guard, and Ctrl+C-cancels-the-run for interactive tasks ([#579](https://github.com/dicode-ayo/dicode-core/issues/579)) ([4f86842](https://github.com/dicode-ayo/dicode-core/commit/4f868427b915961751b232351d2abc64743418c1))
* **cli:** surface tasks pending approval via `dicode task pending` and `list` ([#546](https://github.com/dicode-ayo/dicode-core/issues/546)) ([5ac0fda](https://github.com/dicode-ayo/dicode-core/commit/5ac0fdaaf3c2efacee22a951e1cf4488d412e4d2))
* **dev:** make dev-relay-config — render a local relay.yaml without Doppler ([#461](https://github.com/dicode-ayo/dicode-core/issues/461)) ([bd69011](https://github.com/dicode-ayo/dicode-core/commit/bd69011d8b8d61266c023256e090d68e75d96ef1))
* **e2e:** relay-buildin spec + port param + DICODE_E2E_MOCK_PROVIDER ([#287](https://github.com/dicode-ayo/dicode-core/issues/287)) ([#445](https://github.com/dicode-ayo/dicode-core/issues/445)) ([4bdaed1](https://github.com/dicode-ayo/dicode-core/commit/4bdaed13ee032db8e7aa584f881fbbfea3ae1091))
* **examples:** repo-prune — webhook-triggered, suspend-to-approve git pruning ([#548](https://github.com/dicode-ayo/dicode-core/issues/548)) ([6c38260](https://github.com/dicode-ayo/dicode-core/commit/6c382608401a6aa9caa456617097a8f062f57a8a))
* **mcp:** capability-scope the ephemeral per-run MCP token ([#589](https://github.com/dicode-ayo/dicode-core/issues/589)) ([855931b](https://github.com/dicode-ayo/dicode-core/commit/855931b7f93a40946585551aa14de581c369ad88))
* **mcp:** ephemeral per-run dicode MCP API tokens ([#564](https://github.com/dicode-ayo/dicode-core/issues/564)) ([deeb3f1](https://github.com/dicode-ayo/dicode-core/commit/deeb3f14f15a4bf7a0db968ed63475de4b13e63c))
* **notify:** actionable error when no notification daemon is running + README ([#558](https://github.com/dicode-ayo/dicode-core/issues/558)) ([0204cda](https://github.com/dicode-ayo/dicode-core/commit/0204cda7cf5cf43c04f690643539a1390e10874a))
* **notify:** ping the operator when a run suspends awaiting input ([#597](https://github.com/dicode-ayo/dicode-core/issues/597)) ([a934bef](https://github.com/dicode-ayo/dicode-core/commit/a934bef727230d36896273933c1d87ea437215ef))
* **ops:** cloudflared sidecar task + relay-edge ingress_host; fix dual-trigger ([#631](https://github.com/dicode-ayo/dicode-core/issues/631)) ([7c3e1b0](https://github.com/dicode-ayo/dicode-core/commit/7c3e1b0621c7c01257a858408b89f7975bbcce0d))
* **ops:** relay-edge task — provision the free-bootstrap Cloudflare relay edge ([#628](https://github.com/dicode-ayo/dicode-core/issues/628)) ([8aec29d](https://github.com/dicode-ayo/dicode-core/commit/8aec29dfb441f252a7fbf96972420cab323183af))
* **python:** env-read guardrail — restrict os.environ to declared vars ([#418](https://github.com/dicode-ayo/dicode-core/issues/418)) ([#448](https://github.com/dicode-ayo/dicode-core/issues/448)) ([49c1d79](https://github.com/dicode-ayo/dicode-core/commit/49c1d79a368f3494741aabd1ce56c8888e5c8edc))
* **python:** lock inline PEP 723 deps for reproducibility ([#465](https://github.com/dicode-ayo/dicode-core/issues/465)) ([#466](https://github.com/dicode-ayo/dicode-core/issues/466)) ([38e5776](https://github.com/dicode-ayo/dicode-core/commit/38e577602ae9df248f33224c36d7edc4d562eed1))
* **registry:** collapse a run's descendant tree under its root ([#569](https://github.com/dicode-ayo/dicode-core/issues/569)) ([#573](https://github.com/dicode-ayo/dicode-core/issues/573)) ([aabe236](https://github.com/dicode-ayo/dicode-core/commit/aabe236bfc278d4657e18f56b45a0149ad7c5e5a))
* **relay:** adopt mTLS control channel (dicode-relay 0.2.0, protocol v4) ([#617](https://github.com/dicode-ayo/dicode-core/issues/617)) ([f24ab8b](https://github.com/dicode-ayo/dicode-core/commit/f24ab8b9c22a3dddb062473374a085dde659bba7))
* **relay:** connect to all broker instances (server_urls fan-out consumer) ([#625](https://github.com/dicode-ayo/dicode-core/issues/625)) ([f5e0666](https://github.com/dicode-ayo/dicode-core/commit/f5e066631743def28e4f72fe43baa6d8c1630991))
* **relay:** render relay.yaml locally without Doppler + omit empty OAuth providers ([#493](https://github.com/dicode-ayo/dicode-core/issues/493)) ([5e66e91](https://github.com/dicode-ayo/dicode-core/commit/5e66e9149d006cce110e482b53f08e0252004880))
* **runtime:** deterministic stale-lock auto-recovery for deno + python ([#494](https://github.com/dicode-ayo/dicode-core/issues/494)) ([e7f2530](https://github.com/dicode-ayo/dicode-core/commit/e7f25301969c5f3f195357b9fe968031d2fb8300))
* **runtime:** inject resolved secret env into docker/podman containers ([#629](https://github.com/dicode-ayo/dicode-core/issues/629)) ([4f5cb47](https://github.com/dicode-ayo/dicode-core/commit/4f5cb47546295ed9cd7711d3755bb0d4b5415f1c))
* **runtime:** pattern-scoped host-env forwarding in permissions.env ([#492](https://github.com/dicode-ayo/dicode-core/issues/492)) ([ada06dc](https://github.com/dicode-ayo/dicode-core/commit/ada06dc7b944bc559502b687ba6a97b191201cc4))
* **source:** operator allowlist for git remotes on internal hosts ([#537](https://github.com/dicode-ayo/dicode-core/issues/537)) ([#545](https://github.com/dicode-ayo/dicode-core/issues/545)) ([e19eeb3](https://github.com/dicode-ayo/dicode-core/commit/e19eeb3bfc72fd5cce625d5c2aa61bfeb3e9227c))
* **suspend:** auto-dispatch main/resume/steps (v2 phase B) ([#515](https://github.com/dicode-ayo/dicode-core/issues/515)) ([d959b88](https://github.com/dicode-ayo/dicode-core/commit/d959b882e919cf65ef0062e7af9334595c085c4d))
* **suspend:** Deno runtime + SDK suspend mechanism ([#498](https://github.com/dicode-ayo/dicode-core/issues/498)) ([45b749e](https://github.com/dicode-ayo/dicode-core/commit/45b749e81e03ac058375786453684cc710d94f26))
* **suspend:** dicode resume CLI ([#504](https://github.com/dicode-ayo/dicode-core/issues/504)) ([80552ee](https://github.com/dicode-ayo/dicode-core/commit/80552ee10ff75659c9e43edfdc40634bf1f19dc2))
* **suspend:** JSON-Schema forms + server-side validation (v2 phase A) ([#514](https://github.com/dicode-ayo/dicode-core/issues/514)) ([9dd126b](https://github.com/dicode-ayo/dicode-core/commit/9dd126b18b59a00b8fc9f9c4ba92f69ec2531b8e))
* **suspend:** Python runtime parity for the suspend mechanism ([#501](https://github.com/dicode-ayo/dicode-core/issues/501)) ([a96fefc](https://github.com/dicode-ayo/dicode-core/commit/a96fefc99a2adcc3012487973ff3ed1895dd349e))
* **suspend:** resume orchestration + deadline sweep ([#499](https://github.com/dicode-ayo/dicode-core/issues/499)) ([2b3d93a](https://github.com/dicode-ayo/dicode-core/commit/2b3d93a056e8168501b584b4847604ff6399d7de))
* **suspend:** run status + resume persistence foundation ([#497](https://github.com/dicode-ayo/dicode-core/issues/497)) ([171dfd0](https://github.com/dicode-ayo/dicode-core/commit/171dfd005a714f9f2eea10153445f6215d919d0c))
* **suspend:** webui form-render + resume submit for suspended runs ([#500](https://github.com/dicode-ayo/dicode-core/issues/500)) ([23623cb](https://github.com/dicode-ayo/dicode-core/commit/23623cbe717df74784442d1f5fe09ddd87ab8064))
* **taskset:** allow multiple concurrent dev-mode clone sessions per source ([#283](https://github.com/dicode-ayo/dicode-core/issues/283)) ([#444](https://github.com/dicode-ayo/dicode-core/issues/444)) ([df57a2d](https://github.com/dicode-ayo/dicode-core/commit/df57a2d3e6a5e0500813e82726fe70e5b9b777b7))
* **tasktest:** --format=junit|gh-summary + test-tasks-cli CI job ([#162](https://github.com/dicode-ayo/dicode-core/issues/162)) ([#447](https://github.com/dicode-ayo/dicode-core/issues/447)) ([90449f3](https://github.com/dicode-ayo/dicode-core/commit/90449f315ce3726cb9df0c595178897397cb564c))
* **tasktest:** Python + pytest parity for dicode task test ([#159](https://github.com/dicode-ayo/dicode-core/issues/159) Phase 2) ([#647](https://github.com/dicode-ayo/dicode-core/issues/647)) ([0de345d](https://github.com/dicode-ayo/dicode-core/commit/0de345df1e63b4986374f36de31bf705fd0c8ba0))
* **tray:** warn when no SNI host will show the icon + README ([#557](https://github.com/dicode-ayo/dicode-core/issues/557)) ([fe8f410](https://github.com/dicode-ayo/dicode-core/commit/fe8f41022478179ef08c3c0fe7ec2721c735327b))
* **trigger:** detect daemon crash-loops and surface a crashlooping status ([#458](https://github.com/dicode-ayo/dicode-core/issues/458)) ([#468](https://github.com/dicode-ayo/dicode-core/issues/468)) ([76361f9](https://github.com/dicode-ayo/dicode-core/commit/76361f97d4e50136eb6f1043390f1d6286cf229a))
* **webui:** add shared Lit primitives (Stage 1 of [#93](https://github.com/dicode-ayo/dicode-core/issues/93)) ([#666](https://github.com/dicode-ayo/dicode-core/issues/666)) ([f30598a](https://github.com/dicode-ayo/dicode-core/commit/f30598a9915c7170efadf95feab151ae097399d8))
* **webui:** adopt --dicode-* prefixed design tokens and the new border/focus/control scales ([#713](https://github.com/dicode-ayo/dicode-core/issues/713)) ([fe146a7](https://github.com/dicode-ayo/dicode-core/commit/fe146a75f7ad148d2e91141d8e22c95af525e082))
* **webui:** generic task-contributed nav mechanism ([#637](https://github.com/dicode-ayo/dicode-core/issues/637)) ([c8ec37d](https://github.com/dicode-ayo/dicode-core/commit/c8ec37d03b111d9ae9c7b84338b67d9569e20c97))
* **webui:** trigger.auth "any" — session OR HMAC webhook auth ([#610](https://github.com/dicode-ayo/dicode-core/issues/610)) ([#613](https://github.com/dicode-ayo/dicode-core/issues/613)) ([9f939ef](https://github.com/dicode-ayo/dicode-core/commit/9f939ef726f0c71f590100264ba85a9953b40986))


### Bug Fixes

* **ai-agent-claude-cli:** actually mount the dicode MCP server ([#562](https://github.com/dicode-ayo/dicode-core/issues/562)) ([bf36de2](https://github.com/dicode-ayo/dicode-core/commit/bf36de238dac299441825105c7d43c942bc8bd4a))
* **ai-agent-claude-cli:** make the Claude MCP path work end-to-end ([#574](https://github.com/dicode-ayo/dicode-core/issues/574)) ([cb3ad93](https://github.com/dicode-ayo/dicode-core/commit/cb3ad93193c99284caa70456ee917baeb6cd31ea))
* **approval:** bind dashboard approval to the hash the operator reviewed ([#654](https://github.com/dicode-ayo/dicode-core/issues/654)) ([7a3ea5a](https://github.com/dicode-ayo/dicode-core/commit/7a3ea5a40461734e7cb1501fc3ad9b00a1087ab7))
* **approval:** re-hash live task dir at fire time so edited code re-arms the gate ([#539](https://github.com/dicode-ayo/dicode-core/issues/539)) ([3f78dd0](https://github.com/dicode-ayo/dicode-core/commit/3f78dd02e8e97e64cf171e7852208ad7e6817952))
* **auth:** revoke trusted-device cookie by token_hash on logout ([#474](https://github.com/dicode-ayo/dicode-core/issues/474)) ([#479](https://github.com/dicode-ayo/dicode-core/issues/479)) ([1c6f28a](https://github.com/dicode-ayo/dicode-core/commit/1c6f28a235d66c6d3129130540827542a59c054a))
* builtin task data-dir env resolution + tray lockfile ([#456](https://github.com/dicode-ayo/dicode-core/issues/456)) ([825202a](https://github.com/dicode-ayo/dicode-core/commit/825202af26e1e591fdc27ce9607b00bf6816ef8e))
* **cli:** stop `dicode run` hanging on a task that suspends ([#552](https://github.com/dicode-ayo/dicode-core/issues/552)) ([4de8acd](https://github.com/dicode-ayo/dicode-core/commit/4de8acd56528452bc8245316d770abf43f5ae161))
* **daemon:** gate CLI task ops on a first-reconcile readiness barrier ([#464](https://github.com/dicode-ayo/dicode-core/issues/464)) ([#483](https://github.com/dicode-ayo/dicode-core/issues/483)) ([081f6d2](https://github.com/dicode-ayo/dicode-core/commit/081f6d2551104b595b32203f397af6887a559f31))
* **db,python:** SQLite file mode 0600, stop swallowing migration errors, unify timeout semantics ([#389](https://github.com/dicode-ayo/dicode-core/issues/389)) ([#446](https://github.com/dicode-ayo/dicode-core/issues/446)) ([b7d53cb](https://github.com/dicode-ayo/dicode-core/commit/b7d53cb8e5ceceee4f0ca336d8729848d852c38c))
* **db:** return error instead of panicking on unimplemented postgres/mysql backend ([#453](https://github.com/dicode-ayo/dicode-core/issues/453)) ([cd16004](https://github.com/dicode-ayo/dicode-core/commit/cd16004f48aa0348d9a0d2ca15142b4f07964e4f))
* **deps:** force uuid &gt;=11.1.1 to clear CVE-2026-41907 ([#440](https://github.com/dicode-ayo/dicode-core/issues/440)) ([2b618c4](https://github.com/dicode-ayo/dicode-core/commit/2b618c412ebe1578a99569d1cba2151accc26984))
* **dev-clones-cleanup:** protect suspended runs' clones from sweep ([#587](https://github.com/dicode-ayo/dicode-core/issues/587)) ([f8a4070](https://github.com/dicode-ayo/dicode-core/commit/f8a4070fbf7d4d5cf1f485a3c3c6a57ca6fd1e51))
* **e2e/taskset:** isolate add-source's fixture and name-qualify Source.ID() to stop cron.spec.ts cross-contamination ([#630](https://github.com/dicode-ayo/dicode-core/issues/630)) ([811a8e7](https://github.com/dicode-ayo/dicode-core/commit/811a8e78a1644d8fc8cd8557c465e5fe7297a5a3))
* **ipc:** write IPC frames fully in the Deno shim (large task→daemon messages deadlocked) ([#593](https://github.com/dicode-ayo/dicode-core/issues/593)) ([223d0fa](https://github.com/dicode-ayo/dicode-core/commit/223d0faabc6a6353b39231471a620889aa16b504))
* **notify:** default notify_task is buildin/notifications, not buildin/notify ([#599](https://github.com/dicode-ayo/dicode-core/issues/599)) ([f0e5975](https://github.com/dicode-ayo/dicode-core/commit/f0e5975e6f439941a8fdc7ba0c93a1b2347d21c1))
* **relay-client:** survive ws abortHandshake null-deref and reconnect ([#460](https://github.com/dicode-ayo/dicode-core/issues/460)) ([#467](https://github.com/dicode-ayo/dicode-core/issues/467)) ([d5c8614](https://github.com/dicode-ayo/dicode-core/commit/d5c861472bbc3c40d3b7673f71b68b5c00b57a3b))
* **relay:** pin dicode-relay to 0.1.9 (WebSocket tunnel flap fix) ([#602](https://github.com/dicode-ayo/dicode-core/issues/602)) ([3419a7b](https://github.com/dicode-ayo/dicode-core/commit/3419a7b8d14d9911b9d2fd3fec33f78ad3248acf))
* **runtime/python:** populate ReturnValue, OutputContentType, OutputContent ([#702](https://github.com/dicode-ayo/dicode-core/issues/702)) ([bbe057c](https://github.com/dicode-ayo/dicode-core/commit/bbe057c7ac8966f7994567d2035928e4bb70c044))
* **runtime:** fresh retry deadline + serialize deno relock for stale-lock recovery ([#496](https://github.com/dicode-ayo/dicode-core/issues/496)) ([53a6dd7](https://github.com/dicode-ayo/dicode-core/commit/53a6dd7f0445e3715d4caaa58d787755b34e985b))
* **security:** close ephemeral MCP token redactor leak and self-approval inversion ([#582](https://github.com/dicode-ayo/dicode-core/issues/582)) ([9cfe993](https://github.com/dicode-ayo/dicode-core/commit/9cfe993dfd1e55d667dc79a559342b76247bab08))
* **skills:** reference only real MCP tools in the shared skill library ([#588](https://github.com/dicode-ayo/dicode-core/issues/588)) ([4b2fae1](https://github.com/dicode-ayo/dicode-core/commit/4b2fae1c44e35658c0dca0c5f787406b4d4088ea))
* **source/git:** validate remote host in CloneOrPull, not just ListBranches ([#510](https://github.com/dicode-ayo/dicode-core/issues/510)) ([8f7e8f9](https://github.com/dicode-ayo/dicode-core/commit/8f7e8f93bb0a0f86c00e27a7bcafb0068b16cc8b))
* **startup:** silence three startup log-noise sources ([#524](https://github.com/dicode-ayo/dicode-core/issues/524)) ([fb197b6](https://github.com/dicode-ayo/dicode-core/commit/fb197b63ae4968a450f4a2b8f73ec4f0643312bf))
* **suspend:** [#516](https://github.com/dicode-ayo/dicode-core/issues/516) low-priority hardening follow-ups ([#543](https://github.com/dicode-ayo/dicode-core/issues/543)) ([e1added](https://github.com/dicode-ayo/dicode-core/commit/e1addedd9f9247d13d1cb5bd4803bcfc5c9c2271))
* **suspend:** don't grant CapSuspend to runtimes that can't suspend (docker/podman) ([#506](https://github.com/dicode-ayo/dicode-core/issues/506)) ([09f3c27](https://github.com/dicode-ayo/dicode-core/commit/09f3c27cd9fdd3e4a32a04da39b1a88b12cd1eef))
* **suspend:** fire finish hooks + chains when a suspension is swept on timeout ([#509](https://github.com/dicode-ayo/dicode-core/issues/509)) ([6518c14](https://github.com/dicode-ayo/dicode-core/commit/6518c1432334d51144c9ecfee5f39354c1b7b676))
* **suspend:** handle a suspended pipeline stage without wedging the parent ([#507](https://github.com/dicode-ayo/dicode-core/issues/507)) ([5240b31](https://github.com/dicode-ayo/dicode-core/commit/5240b31050f657289a89cd6b16ec44bd97941088))
* **suspend:** keep the daemon body-in-flight invariant across suspend/resume ([#508](https://github.com/dicode-ayo/dicode-core/issues/508)) ([54f03f5](https://github.com/dicode-ayo/dicode-core/commit/54f03f53d027b6dfea83036817488ffc38581038))
* **suspend:** preserve run params and chain depth across resume ([#505](https://github.com/dicode-ayo/dicode-core/issues/505)) ([ddd76bf](https://github.com/dicode-ayo/dicode-core/commit/ddd76bf662aa5c8d849614305eaaf7ab108d258b))
* **suspend:** probe fire guard before consuming the resume token ([#503](https://github.com/dicode-ayo/dicode-core/issues/503)) ([c917cf9](https://github.com/dicode-ayo/dicode-core/commit/c917cf9ae391d17b4055cd8e362f44e6cf8e662e))
* **suspend:** release the daemon slot + restart when a suspended daemon is swept on timeout ([#511](https://github.com/dicode-ayo/dicode-core/issues/511)) ([6fb1995](https://github.com/dicode-ayo/dicode-core/commit/6fb1995af039a05ee41a97a38f8c449afd43cf59))
* **suspend:** robustness hardening from v2 integration audit ([#517](https://github.com/dicode-ayo/dicode-core/issues/517)) ([#519](https://github.com/dicode-ayo/dicode-core/issues/519)) ([14f6d90](https://github.com/dicode-ayo/dicode-core/commit/14f6d90de0da7115a6b66c4ee5a0690ad00605c9))
* **taskset:** log and fold task.Hash errors into snapHash instead of swallowing them ([#703](https://github.com/dicode-ayo/dicode-core/issues/703)) ([26e9a8d](https://github.com/dicode-ayo/dicode-core/commit/26e9a8d084b36441ab1ad2a3ec0a1c9289e5d559))
* **temp-cleanup:** route the /tmp sweep through the readDir guard ([#457](https://github.com/dicode-ayo/dicode-core/issues/457)) ([ce4f57b](https://github.com/dicode-ayo/dicode-core/commit/ce4f57be67d75ac1b3478465f8822bdab90ea945))
* **trigger:** a Pending pipeline still fires on a chain edge and on its own webhook ([#704](https://github.com/dicode-ayo/dicode-core/issues/704)) ([caf2943](https://github.com/dicode-ayo/dicode-core/commit/caf294359a708b91087133c17146497846568a3e))
* **trigger:** avoid dropping a cron tick during a no-op re-registration ([#565](https://github.com/dicode-ayo/dicode-core/issues/565)) ([25f0dd1](https://github.com/dicode-ayo/dicode-core/commit/25f0dd197e485a9b3923fa50c18354c3d4de3fb9))
* **trigger:** bind webhook replay cache to timestamp, add require_timestamp gate ([#612](https://github.com/dicode-ayo/dicode-core/issues/612)) ([40b0ace](https://github.com/dicode-ayo/dicode-core/commit/40b0ace7dacadc01d8c330f29e82d781c6e4d42f))
* **trigger:** close daemon lifecycle races — slot reservation, gated backoff restart ([#470](https://github.com/dicode-ayo/dicode-core/issues/470)) ([#471](https://github.com/dicode-ayo/dicode-core/issues/471)) ([f4af65b](https://github.com/dicode-ayo/dicode-core/commit/f4af65b7773af2cfd049d8c1dd52df5d56f7193c))
* **trigger:** drain apiReplayRun's storage-task fireSync on shutdown ([#540](https://github.com/dicode-ayo/dicode-core/issues/540)) ([801c37f](https://github.com/dicode-ayo/dicode-core/commit/801c37fc73384880ffd4344eeafb23be53c9ecc3))
* **trigger:** drain in-flight runs before shutdown closes the DB ([#525](https://github.com/dicode-ayo/dicode-core/issues/525)) ([6faede1](https://github.com/dicode-ayo/dicode-core/commit/6faede1aa4e43b8f0d8cb2190e5f81457e29d322))
* **trigger:** drain the sync-webhook fireSync path on shutdown ([#532](https://github.com/dicode-ayo/dicode-core/issues/532)) ([8d312df](https://github.com/dicode-ayo/dicode-core/commit/8d312df39bf18f6e97aacfd7fc5eba948dd63664))
* **trigger:** lifecycle correctness — chain cycles, prereq timeout, crash backoff, SIGHUP, DB logging ([#438](https://github.com/dicode-ayo/dicode-core/issues/438)) ([b925918](https://github.com/dicode-ayo/dicode-core/commit/b9259183afeedda0cdad923e5d262baf8edbb4da))
* **trigger:** poll ActivePIDs instead of single-shot read in daemon-reap test ([#619](https://github.com/dicode-ayo/dicode-core/issues/619)) ([cbc1112](https://github.com/dicode-ayo/dicode-core/commit/cbc111251a02fb632ba1ddbb94ba1b27a6784ef8))
* **trigger:** stop Register from deleting the webhook path it re-claims ([#549](https://github.com/dicode-ayo/dicode-core/issues/549)) ([9d1e34c](https://github.com/dicode-ayo/dicode-core/commit/9d1e34cfe28ba55defc8b29965e5e563eddec58c))
* **webui:** make suspend/resume navigable and self-refreshing in the UI ([#527](https://github.com/dicode-ayo/dicode-core/issues/527)) ([5cabaee](https://github.com/dicode-ayo/dicode-core/commit/5cabaeef56108f5abba193dce89ba7db38c4db4a))
* **webui:** map stale var() tokens to --dicode-* prefixed tokens ([#715](https://github.com/dicode-ayo/dicode-core/issues/715)) ([27660c7](https://github.com/dicode-ayo/dicode-core/commit/27660c748c3b06d043d40e0d6224a4d2ec0503be))
* **webui:** re-resolve taskset overrides on raw-config save ([#408](https://github.com/dicode-ayo/dicode-core/issues/408)) ([#430](https://github.com/dicode-ayo/dicode-core/issues/430)) ([4b47568](https://github.com/dicode-ayo/dicode-core/commit/4b475685b4925b7d77c1b43e01b904cb7172187d))
* **webui:** rotate device token in one place so every auth path gets the cookie ([#697](https://github.com/dicode-ayo/dicode-core/issues/697)) ([3e608ab](https://github.com/dicode-ayo/dicode-core/commit/3e608ab3fa63886f6a0dd70b64d64a2078801d97))
* **webui:** send a suspended run's result page to the resume form ([#547](https://github.com/dicode-ayo/dicode-core/issues/547)) ([6e4562e](https://github.com/dicode-ayo/dicode-core/commit/6e4562ea4812e1386d3449dbed0e2c45a62fee68))
* **webui:** send and validate source name in add-source form ([#601](https://github.com/dicode-ayo/dicode-core/issues/601)) ([2d89865](https://github.com/dicode-ayo/dicode-core/commit/2d898657e9fdf874768d66fb04a2681dfed07ff7))
* **webui:** stop /login demanding a passphrase when none is configured ([#655](https://github.com/dicode-ayo/dicode-core/issues/655)) ([7f5cbfd](https://github.com/dicode-ayo/dicode-core/commit/7f5cbfd87799624c54a0a39dd08183d4a03b3b31))
* **webui:** stop bouncing relay-reached auth webhooks to an unreachable login ([#609](https://github.com/dicode-ayo/dicode-core/issues/609)) ([5b6a6bc](https://github.com/dicode-ayo/dicode-core/commit/5b6a6bc7ccac0ad5a023e9b57489a0ffac26090d))
* **webui:** stop the task list from presenting pending tasks as live ([#663](https://github.com/dicode-ayo/dicode-core/issues/663)) ([94c676c](https://github.com/dicode-ayo/dicode-core/commit/94c676c631eb12a5526523908e39f85e5bf8a6d4))
* **webui:** surface task load failures instead of silently vanishing ([#656](https://github.com/dicode-ayo/dicode-core/issues/656)) ([0a7d7f7](https://github.com/dicode-ayo/dicode-core/commit/0a7d7f735b6aa9fc2d9cf26059b7803bc9f2782a))
* **webui:** validate runtime versions and canonicalize task-file paths; clear the CodeQL backlog ([#709](https://github.com/dicode-ayo/dicode-core/issues/709)) ([3935738](https://github.com/dicode-ayo/dicode-core/commit/39357381cbada4cb25b3650cc08e8c14adc8c136))


### Performance Improvements

* **task:** cap per-file bytes in task.Hash and skip heavy dirs ([#401](https://github.com/dicode-ayo/dicode-core/issues/401)) ([#428](https://github.com/dicode-ayo/dicode-core/issues/428)) ([32f2a24](https://github.com/dicode-ayo/dicode-core/commit/32f2a245cb80341bfc49f893ad13ad79f645a083))


### Documentation

* correct claude-cli tool claim + adopt capability-scoped MCP as the agent surface ([#561](https://github.com/dicode-ayo/dicode-core/issues/561)) ([144faff](https://github.com/dicode-ayo/dicode-core/commit/144faff126f45acb93419065676d70936ebc50a2))
* **design:** AI task authoring — adopted direction + phased plan ([#559](https://github.com/dicode-ayo/dicode-core/issues/559)) ([5b8247c](https://github.com/dicode-ayo/dicode-core/commit/5b8247c49ed73836f085bde97e26ead3e8e1bef1))
* **design:** AI tasks as suspend/resume conversations ([#572](https://github.com/dicode-ayo/dicode-core/issues/572)) ([c5121d3](https://github.com/dicode-ayo/dicode-core/commit/c5121d3088dc1cbed458d08205b7648e06ba465b))
* **design:** approval review renders end state, not a diff ([#673](https://github.com/dicode-ayo/dicode-core/issues/673)) ([efd3c46](https://github.com/dicode-ayo/dicode-core/commit/efd3c46b839d76d65c832f5dc21e99b1e373a427))
* **design:** record the verified AI-loop convergence design ([#586](https://github.com/dicode-ayo/dicode-core/issues/586)) ([5eb4390](https://github.com/dicode-ayo/dicode-core/commit/5eb4390cd6756be60fa140804adc1abd69904616))
* **examples:** branching deploy-wizard suspend example ([#535](https://github.com/dicode-ayo/dicode-core/issues/535)) ([6b07592](https://github.com/dicode-ayo/dicode-core/commit/6b07592264d0206ba2b31617ced49cf7c8dd202f))
* fix stale deployment claims in business-model.md ([#556](https://github.com/dicode-ayo/dicode-core/issues/556)) ([043008d](https://github.com/dicode-ayo/dicode-core/commit/043008d52f626de0620294448836bdae08cc4529))
* reconcile documentation with current code (post relay Go→TS migration) ([#555](https://github.com/dicode-ayo/dicode-core/issues/555)) ([9ca2df3](https://github.com/dicode-ayo/dicode-core/commit/9ca2df303200c290e46e943344e575b4737275be))
* **suspend:** document suspendable tasks (dicode.suspend / resume) ([#513](https://github.com/dicode-ayo/dicode-core/issues/513)) ([2d29c1a](https://github.com/dicode-ayo/dicode-core/commit/2d29c1a5dda55857a585ea090d438d8ba7b47a1a))
* **suspend:** shipped example wizard + comment cleanup ([#518](https://github.com/dicode-ayo/dicode-core/issues/518)) ([7008cb9](https://github.com/dicode-ayo/dicode-core/commit/7008cb9bbbd9de316bf404c6861b5975fb081629))
* upgrade notes for 0.4.1 behaviour changes ([#544](https://github.com/dicode-ayo/dicode-core/issues/544)) ([b9022d4](https://github.com/dicode-ayo/dicode-core/commit/b9022d4c0cae44760f415000b4e8606c0f616d15))

## [0.4.0](https://github.com/dicode-ayo/dicode-core/compare/v0.3.0...v0.4.0) (2026-06-18)


### ⚠ BREAKING CHANGES

* **permissions:** default net permission flips to deny ([#379](https://github.com/dicode-ayo/dicode-core/issues/379)) (#394)

### security

* **permissions:** default net permission flips to deny ([#379](https://github.com/dicode-ayo/dicode-core/issues/379)) ([#394](https://github.com/dicode-ayo/dicode-core/issues/394)) ([1013cf3](https://github.com/dicode-ayo/dicode-core/commit/1013cf3e69b2c794559524765c0d2460804c7a55))


### Features

* **approval:** approve surfaces — WebUI, CLI, tokenized link ([#392](https://github.com/dicode-ayo/dicode-core/issues/392) phase 2) ([#403](https://github.com/dicode-ayo/dicode-core/issues/403)) ([a2e1eac](https://github.com/dicode-ayo/dicode-core/commit/a2e1eacac554b204ceed95c3d247c5cd7fc48c23))
* **approval:** operator notification on pending task ([#399](https://github.com/dicode-ayo/dicode-core/issues/399)) ([#409](https://github.com/dicode-ayo/dicode-core/issues/409)) ([34c80da](https://github.com/dicode-ayo/dicode-core/commit/34c80da79f85d55df96ca9041522ac3ed39df4e4))
* **approval:** trust-on-change task approval gate — core gate ([#392](https://github.com/dicode-ayo/dicode-core/issues/392), phase 1) ([#393](https://github.com/dicode-ayo/dicode-core/issues/393)) ([090b8d8](https://github.com/dicode-ayo/dicode-core/commit/090b8d862fcc73156ffabadb14f55be97f6eb331))
* **audit:** cursor + SDK audit.query() + builtin Grafana/Loki exporter ([#415](https://github.com/dicode-ayo/dicode-core/issues/415)) ([#416](https://github.com/dicode-ayo/dicode-core/issues/416)) ([f7b48c1](https://github.com/dicode-ayo/dicode-core/commit/f7b48c1569270bb600619debd3d20e6a9a7f4216))
* **audit:** structured audit log for security-sensitive operations ([#45](https://github.com/dicode-ayo/dicode-core/issues/45)) ([#395](https://github.com/dicode-ayo/dicode-core/issues/395)) ([755906d](https://github.com/dicode-ayo/dicode-core/commit/755906d069412c7c7a7f1f81afe32c0581d7c7ba))
* **authoring:** IPC + CLI verbs for AI task create/edit/save/cancel ([#288](https://github.com/dicode-ayo/dicode-core/issues/288)) ([#376](https://github.com/dicode-ayo/dicode-core/issues/376)) ([cea475f](https://github.com/dicode-ayo/dicode-core/commit/cea475f7d8d2afc3c78a2f75ed5c13e71cb07256))
* **authoring:** sessions table + REST handlers for AI task create/edit ([#288](https://github.com/dicode-ayo/dicode-core/issues/288)) ([#369](https://github.com/dicode-ayo/dicode-core/issues/369)) ([a17dc6e](https://github.com/dicode-ayo/dicode-core/commit/a17dc6ef4752406209e3d36c3a79e897eaf06904))
* **cli:** dicode task delete &lt;id&gt; ([#285](https://github.com/dicode-ayo/dicode-core/issues/285)) ([#377](https://github.com/dicode-ayo/dicode-core/issues/377)) ([63321d5](https://github.com/dicode-ayo/dicode-core/commit/63321d5d76d125f34a8c648b163396e91e09e1f2))
* **deno:** env_read_exposed flag to close --allow-env gap for npm tasks ([#413](https://github.com/dicode-ayo/dicode-core/issues/413)) ([1cce241](https://github.com/dicode-ayo/dicode-core/commit/1cce24100c0b14c0e994cf7f0f7c37c4ad2093d9))
* **pipeline:** parallel stage execution with DAG dependencies ([#355](https://github.com/dicode-ayo/dicode-core/issues/355)) ([#368](https://github.com/dicode-ayo/dicode-core/issues/368)) ([a01642b](https://github.com/dicode-ayo/dicode-core/commit/a01642b262bcbafc839a28021af3d1b1534a16dd))
* **python-runtime:** enforce permissions.{fs,net,run} via PEP 578 audit hook ([#390](https://github.com/dicode-ayo/dicode-core/issues/390)) ([bcf5db2](https://github.com/dicode-ayo/dicode-core/commit/bcf5db2bac39567932ed50b36cc1820ea93b2b25))
* **runtime:** enforce container security floor for docker/podman ([#380](https://github.com/dicode-ayo/dicode-core/issues/380)) ([#397](https://github.com/dicode-ayo/dicode-core/issues/397)) ([920ff92](https://github.com/dicode-ayo/dicode-core/commit/920ff921b33c41581fd5e9b496cba5f6ffd74f57))


### Bug Fixes

* **auth-providers:** restore connected-providers indication via secrets.has ([#255](https://github.com/dicode-ayo/dicode-core/issues/255)) ([#374](https://github.com/dicode-ayo/dicode-core/issues/374)) ([897726e](https://github.com/dicode-ayo/dicode-core/commit/897726e6b1f6b7f5e03de2c9e321b3ace7e6179c))
* **auth:** align slack-oauth token key with dashboard convention ([#223](https://github.com/dicode-ayo/dicode-core/issues/223)) ([#373](https://github.com/dicode-ayo/dicode-core/issues/373)) ([73b8011](https://github.com/dicode-ayo/dicode-core/commit/73b8011e8723370c86e19184c23177d4f2e3c92b))
* **envresolve:** share resolver across launches for cross-fire cache TTL ([#242](https://github.com/dicode-ayo/dicode-core/issues/242)) ([#372](https://github.com/dicode-ayo/dicode-core/issues/372)) ([3eea744](https://github.com/dicode-ayo/dicode-core/commit/3eea744c9e166eecad00a027b7d47a3e46172052))
* **hot-reload:** LocalSource Sync delivery, new-dir watching, provider retry, Docker template expansion ([#425](https://github.com/dicode-ayo/dicode-core/issues/425)) ([97ba564](https://github.com/dicode-ayo/dicode-core/commit/97ba56491575223fe68ef552273a13f805d9995a))
* post-alpha correctness — scanner limits, FD leak, WS origin ([#194](https://github.com/dicode-ayo/dicode-core/issues/194)) ([#367](https://github.com/dicode-ayo/dicode-core/issues/367)) ([6ed5a37](https://github.com/dicode-ayo/dicode-core/commit/6ed5a3764d6404d25526606c09a84d42bfb9651c))
* **python:** make TestExecute_HelloPythonSucceeds hermetic (Closes [#422](https://github.com/dicode-ayo/dicode-core/issues/422)) ([#427](https://github.com/dicode-ayo/dicode-core/issues/427)) ([e53aa05](https://github.com/dicode-ayo/dicode-core/commit/e53aa05cf2e13b883c3cbf003b66c0cb76ed5e40))
* **python:** run async main() with no args and always log run failures ([#405](https://github.com/dicode-ayo/dicode-core/issues/405)) ([#410](https://github.com/dicode-ayo/dicode-core/issues/410)) ([017a166](https://github.com/dicode-ayo/dicode-core/commit/017a16602a698854b64fbc26cdb1b1d1430e4565))
* **relay:** gate relay-server-body daemon on relay config ([#406](https://github.com/dicode-ayo/dicode-core/issues/406)) ([#411](https://github.com/dicode-ayo/dicode-core/issues/411)) ([b452c29](https://github.com/dicode-ayo/dicode-core/commit/b452c29108bccb2702904e473e362d60d0f7861a))


### Documentation

* audit log ([#45](https://github.com/dicode-ayo/dicode-core/issues/45)) and container security floor ([#380](https://github.com/dicode-ayo/dicode-core/issues/380)) ([#404](https://github.com/dicode-ayo/dicode-core/issues/404)) ([8cf53d1](https://github.com/dicode-ayo/dicode-core/commit/8cf53d1645ab64cb881f5b6ec86dab0fbce569a9))
* update security, MCP, pipeline, and API docs after PRs [#362](https://github.com/dicode-ayo/dicode-core/issues/362)–[#369](https://github.com/dicode-ayo/dicode-core/issues/369) ([#371](https://github.com/dicode-ayo/dicode-core/issues/371)) ([30daec5](https://github.com/dicode-ayo/dicode-core/commit/30daec5e76262048866dba06e0594d80a6f723ad))

## [0.3.0](https://github.com/dicode-ayo/dicode-core/compare/v0.2.1...v0.3.0) (2026-06-10)


### ⚠ BREAKING CHANGES

* document kind: PipelineTask; remove all legacy trigger.before references (PR7/8) ([#349](https://github.com/dicode-ayo/dicode-core/issues/349))
* remove trigger.before; pipelines replace preflight orchestration (PR6/8) ([#347](https://github.com/dicode-ayo/dicode-core/issues/347))
* drop legacy notification subsystem ([#281](https://github.com/dicode-ayo/dicode-core/issues/281))

### docs+chore

* document kind: PipelineTask; remove all legacy trigger.before references (PR7/8) ([#349](https://github.com/dicode-ayo/dicode-core/issues/349)) ([82f48f3](https://github.com/dicode-ayo/dicode-core/commit/82f48f3d06d54f7616f8f8aea801e8e9da220ee7))


### Features

* **auth-providers:** auto-discover BYO OAuth tasks via template marker ([#278](https://github.com/dicode-ayo/dicode-core/issues/278)) ([b0eecf9](https://github.com/dicode-ayo/dicode-core/commit/b0eecf952a974aaaf50cb4bd8871124810b7ffd8))
* **buildin/relay-server:** Doppler-fed relay supervisor ([#284](https://github.com/dicode-ayo/dicode-core/issues/284)) ([3992f9c](https://github.com/dicode-ayo/dicode-core/commit/3992f9c9a8fd6f6d4ed316338d748e5acf785acd))
* **buildin/template:** ${VAR} substitution library task ([#298](https://github.com/dicode-ayo/dicode-core/issues/298)) ([154a3b5](https://github.com/dicode-ayo/dicode-core/commit/154a3b5bb05485cd424cde5b6a886321b49828a9))
* **buildin/template:** template_path param to load body from a file ([#323](https://github.com/dicode-ayo/dicode-core/issues/323)) ([8616b88](https://github.com/dicode-ayo/dicode-core/commit/8616b8880704838fa76ad56e212a0f9172677df6))
* **buildin/write-local:** generic file-write library task ([#309](https://github.com/dicode-ayo/dicode-core/issues/309)) ([16bb4af](https://github.com/dicode-ayo/dicode-core/commit/16bb4affd57c277e77f2a73da3fe0a45e28ca1e6))
* **config:** AI task-authoring fields + virtual ai-scratch source (epic [#288](https://github.com/dicode-ayo/dicode-core/issues/288)) ([#293](https://github.com/dicode-ayo/dicode-core/issues/293)) ([0fec424](https://github.com/dicode-ayo/dicode-core/commit/0fec424e7f14902c1e0a494b2930a5af4e223ca1))
* drop legacy notification subsystem ([#281](https://github.com/dicode-ayo/dicode-core/issues/281)) ([019cb5e](https://github.com/dicode-ayo/dicode-core/commit/019cb5e651d61b681fa7f860a9be84e53b09269b))
* **task,trigger:** allow trigger.before on one-shot tasks ([#331](https://github.com/dicode-ayo/dicode-core/issues/331)) ([f39781a](https://github.com/dicode-ayo/dicode-core/commit/f39781a9cea027ea57ab3fe582d80661c21e82fe))
* **task,trigger:** richer ${input.*} interpolation grammar ([#330](https://github.com/dicode-ayo/dicode-core/issues/330)) ([96c3461](https://github.com/dicode-ayo/dicode-core/commit/96c3461fe78857e67fa2390637d993c1d0488eaf))
* **task/docker:** add network + hardening fields to docker spec ([#296](https://github.com/dicode-ayo/dicode-core/issues/296)) ([643c2f4](https://github.com/dicode-ayo/dicode-core/commit/643c2f4ee5770338128f043e59a62cadebb1cabd))
* **task:** expand template vars in docker.volumes ([#297](https://github.com/dicode-ayo/dicode-core/issues/297)) ([555354c](https://github.com/dicode-ayo/dicode-core/commit/555354c1269a925506caf59365d47e7ad69e1ba8))
* **task:** PipelineTask spec-layer foundation (kind: PipelineTask, PR1/8) ([#339](https://github.com/dicode-ayo/dicode-core/issues/339)) ([4a6131a](https://github.com/dicode-ayo/dicode-core/commit/4a6131aff5659a0bc138392ea92c28c9dfdeff44))
* **task:** run_result flag to suppress return-value persistence ([#302](https://github.com/dicode-ayo/dicode-core/issues/302)) ([67f04c9](https://github.com/dicode-ayo/dicode-core/commit/67f04c92e5c5d61ab14aca176c3a2df27e37eb3a))
* **taskset:** path templating variables in ref entries — REPO_DIR, TASKSET_DIR ([#360](https://github.com/dicode-ayo/dicode-core/issues/360)) ([0dd9667](https://github.com/dicode-ayo/dicode-core/commit/0dd966728766ac44a9bab002ed0a33f01823c844))
* **transport:** PipelineTask polymorphic transport + registration (PR2/8) ([#340](https://github.com/dicode-ayo/dicode-core/issues/340)) ([ee93bfd](https://github.com/dicode-ayo/dicode-core/commit/ee93bfd060437c6b383239bed6729d58f828757c))
* **trigger,task:** ${input.output} interpolation in chain.params + before.overrides.params ([#310](https://github.com/dicode-ayo/dicode-core/issues/310)) ([6c4a8da](https://github.com/dicode-ayo/dicode-core/commit/6c4a8da427a6522ed580a83b766ea822e3337184))
* **trigger,webui:** distinguish crashed daemon from cleanly-stopped ([#329](https://github.com/dicode-ayo/dicode-core/issues/329)) ([257ae3c](https://github.com/dicode-ayo/dicode-core/commit/257ae3c41617a691d4388bda93b31b304383874b))
* **trigger,webui:** distinguish failed-after-preflight from stopped ([#324](https://github.com/dicode-ayo/dicode-core/issues/324)) ([60b6bb6](https://github.com/dicode-ayo/dicode-core/commit/60b6bb6eb272efbedc9702a4ec318861fb6203aa))
* **trigger:** chain trigger params (symmetry with failure chain) ([#299](https://github.com/dicode-ayo/dicode-core/issues/299)) ([046c8e9](https://github.com/dicode-ayo/dicode-core/commit/046c8e95784053b40c11f6b2f3495d7b427ff44f))
* **trigger:** PipelineRunner — sequential execution + dispatch (PR3/8) ([#342](https://github.com/dicode-ayo/dicode-core/issues/342)) ([f9a5323](https://github.com/dicode-ayo/dicode-core/commit/f9a53237e24ff7c1e5f445d9935ad4cb4af26d7d))
* **trigger:** re-run propagation + daemon-terminal-stage lifetime (PR4/8) ([#343](https://github.com/dicode-ayo/dicode-core/issues/343)) ([c57ed5a](https://github.com/dicode-ayo/dicode-core/commit/c57ed5aeab89bc0d1b5528fa7344a8ac1c8344e1))
* **trigger:** seed PipelineTask stage 0 input from the firing trigger ([#351](https://github.com/dicode-ayo/dicode-core/issues/351)) ([56e34a9](https://github.com/dicode-ayo/dicode-core/commit/56e34a97494550be4dfdc8e488b5bf0b1682c24a))
* **webui:** surface pipelines + stages in the WebUI (PR5/8) ([#345](https://github.com/dicode-ayo/dicode-core/issues/345)) ([74ddb33](https://github.com/dicode-ayo/dicode-core/commit/74ddb33865532d6ba6d5a7b12a39b746b0a2a35e))


### Bug Fixes

* **buildin/auth-providers:** land [#256](https://github.com/dicode-ayo/dicode-core/issues/256) on main + broker-less fallback ([#275](https://github.com/dicode-ayo/dicode-core/issues/275)) ([77efdc8](https://github.com/dicode-ayo/dicode-core/commit/77efdc86cf5839c39782a65696b1eee13525f4be))
* **buildin/relay-server:** pre-create broker signing key on first run ([#289](https://github.com/dicode-ayo/dicode-core/issues/289)) ([96ff725](https://github.com/dicode-ayo/dicode-core/commit/96ff725b52f5b37a7d2c2dc701c28719bb883935))
* **ipc:** close log-batch silent-loss windows on Stop() and flush errors ([#357](https://github.com/dicode-ayo/dicode-core/issues/357)) ([975495b](https://github.com/dicode-ayo/dicode-core/commit/975495b80d7a08160dcb12313d9002e2f5a32423))
* **task:** unify empty-string handling across ${input.*} forms ([#336](https://github.com/dicode-ayo/dicode-core/issues/336)) ([ffdc51a](https://github.com/dicode-ayo/dicode-core/commit/ffdc51a85034b2668611c50c225e5894f9ef3f39))
* **trigger,daemon:** pipeline daemon trigger fixes — restart orphan, webhook gateway, cold-start ordering ([#346](https://github.com/dicode-ayo/dicode-core/issues/346)) ([bfbadae](https://github.com/dicode-ayo/dicode-core/commit/bfbadae1d54a75141e3d3e0b7027fbc507fbc74f))
* **trigger:** atomic FinishRunWithResult eliminates status/return_value race ([#354](https://github.com/dicode-ayo/dicode-core/issues/354)) ([daeb1f7](https://github.com/dicode-ayo/dicode-core/commit/daeb1f7c7a38912a0b8b7eb9cd270b8ac51ee02f))
* **trigger:** cancelled daemon run transitions to DaemonStopped ([#335](https://github.com/dicode-ayo/dicode-core/issues/335)) ([19a3a9f](https://github.com/dicode-ayo/dicode-core/commit/19a3a9f1f786054c8f67650d60413ddc78e0e5ed))
* **trigger:** runTask preflight-env FireChain is now synchronous ([#337](https://github.com/dicode-ayo/dicode-core/issues/337)) ([bbb3fe0](https://github.com/dicode-ayo/dicode-core/commit/bbb3fe036bbf4cd6df38463c564daf8798e8227f))
* **webui,taskset:** hardening follow-ups — redact RefAuth secrets + source API tests ([#359](https://github.com/dicode-ayo/dicode-core/issues/359)) ([c8ae687](https://github.com/dicode-ayo/dicode-core/commit/c8ae687dc05632956c909c0fb4402f60b1277768))
* **webui:** sync SourceManager.cfg on raw config save ([#356](https://github.com/dicode-ayo/dicode-core/issues/356)) ([a193a54](https://github.com/dicode-ayo/dicode-core/commit/a193a54116a612a2fae00767b422e97c0b31dec7))


### Documentation

* document template_path + daemon states ([#327](https://github.com/dicode-ayo/dicode-core/issues/327)) ([6710763](https://github.com/dicode-ayo/dicode-core/commit/671076312ccb08eb9031275b82274cfebd3bc334))
* **examples:** cloudflared uses buildin/template + buildin/write-local ([#321](https://github.com/dicode-ayo/dicode-core/issues/321)) ([9eccc92](https://github.com/dicode-ayo/dicode-core/commit/9eccc92d53e23730ed5cebd9afa74f16f043d701))
* **examples:** cloudflared via template-resolver + preflight + per-edge overrides ([#305](https://github.com/dicode-ayo/dicode-core/issues/305)) ([cdbe3d5](https://github.com/dicode-ayo/dicode-core/commit/cdbe3d58e48dd467b0a1789a275ede573b17b4df))


### Miscellaneous

* remove trigger.before; pipelines replace preflight orchestration (PR6/8) ([#347](https://github.com/dicode-ayo/dicode-core/issues/347)) ([f9e3edc](https://github.com/dicode-ayo/dicode-core/commit/f9e3edcaefb8dfe2a82729207a0adc8b47a9bf48))

## [Unreleased]

### Breaking

- **Legacy notification subsystem removed.** The daemon-level `notifications:`
  config block (provider/on_failure/on_success), the per-task `notify:` field,
  the `notify` taskset override key, and the `pkg/notify` package are all
  gone. Notifications are now delivered exclusively by tasks: point
  `defaults.on_failure_chain` (or per-task `on_failure_chain`) at
  `buildin/alert`, `buildin/notifications`, or any task you write yourself
  for ntfy / Slack / Discord / email / etc. WebSocket `run:finished` payloads
  no longer carry `notifyOnSuccess` / `notifyOnFailure` fields. **The daemon
  refuses to start if `dicode.yaml` still has a `notifications:` block** — you
  will lose all alerts otherwise, since the keys would be silently ignored.
  Strip the block (and any `notify:` blocks under task.yaml) to migrate.

## [0.2.1](https://github.com/dicode-ayo/dicode-core/compare/v0.2.0...v0.2.1) (2026-05-06)


### Bug Fixes

* **release:** revert draft-mode now that immutable releases is off ([#274](https://github.com/dicode-ayo/dicode-core/issues/274)) ([3ef96a5](https://github.com/dicode-ayo/dicode-core/commit/3ef96a5e69f5cde951edd5c146b678fec5607301))

## [0.2.0](https://github.com/dicode-ayo/dicode-core/compare/v0.1.2...v0.2.0) (2026-05-06)


### ⚠ BREAKING CHANGES

* remove direct-AI Go code, port dev skill to task-based skills ([#134](https://github.com/dicode-ayo/dicode-core/issues/134))

### Features

* **#48:** split monolith into dicoded daemon + dicode CLI ([#57](https://github.com/dicode-ayo/dicode-core/issues/57)) ([257d590](https://github.com/dicode-ayo/dicode-core/commit/257d5901994fe571565a5d62373fb9b62daddac3))
* add github-stars example task ([55ed356](https://github.com/dicode-ayo/dicode-core/commit/55ed356c7a8ee734bd7847b8372c089c4cb9eddf))
* add max concurrent tasks semaphore in fireAsync() ([#74](https://github.com/dicode-ayo/dicode-core/issues/74)) ([ec8677c](https://github.com/dicode-ayo/dicode-core/commit/ec8677cbd2cc3d0caaed93abbbebaa2613d0f81f))
* ai ([80c8ce1](https://github.com/dicode-ayo/dicode-core/commit/80c8ce1d60f74ff99040c7c00d3db0c73e0d7bd9))
* **ai-agent:** set_group so chat conversations collapse in the run list ([#112](https://github.com/dicode-ayo/dicode-core/issues/112), [#113](https://github.com/dicode-ayo/dicode-core/issues/113)) ([#251](https://github.com/dicode-ayo/dicode-core/issues/251)) ([21dd4a9](https://github.com/dicode-ayo/dicode-core/commit/21dd4a98ffa045a57d63a3608145425764c12db3))
* auto-fix loop — engine guardrails + git-pr + dicode-auto-fix skill + auto-fix taskset entry ([#238](https://github.com/dicode-ayo/dicode-core/issues/238)) ([#247](https://github.com/dicode-ayo/dicode-core/issues/247)) ([9044612](https://github.com/dicode-ayo/dicode-core/commit/90446125cc92ffe96fb89a393f1223d159eb885f))
* auto-fix SDK surface (replay / tasks.test / sources.set_dev_mode / git.commit_push) ([#234](https://github.com/dicode-ayo/dicode-core/issues/234)) ([#245](https://github.com/dicode-ayo/dicode-core/issues/245)) ([92b43a7](https://github.com/dicode-ayo/dicode-core/commit/92b43a7a9b1c4abf15a0a74d04a8a93e907ec247))
* browser notifications, run events SSE, return value storage ([868cb1d](https://github.com/dicode-ayo/dicode-core/commit/868cb1da5cd7d355ade7e25c7a60ad29bac8c812))
* **buildin:** ai-agent chat task + task.yaml template vars ([#98](https://github.com/dicode-ayo/dicode-core/issues/98)) ([3d8c5ec](https://github.com/dicode-ayo/dicode-core/commit/3d8c5ec3b34e4c759a8a718e8f10cc553519538a))
* **buildin:** ai-agent-claude-cli — drive Claude via subscription, not API ([#248](https://github.com/dicode-ayo/dicode-core/issues/248)) ([6c63723](https://github.com/dicode-ayo/dicode-core/commit/6c63723a62146c02e105be1674e4b4aba8bc2e73))
* **buildin:** auth-providers dashboard + dicode.oauth.list_status ([#221](https://github.com/dicode-ayo/dicode-core/issues/221)) ([27cef46](https://github.com/dicode-ayo/dicode-core/commit/27cef46f378145e5fb32b258e5a14139a214a618))
* clean up before going public ([ee2a149](https://github.com/dicode-ayo/dicode-core/commit/ee2a1491b0de271b1ae73adca0901d1663aa1d4e))
* **config,runtime:** add RelayConfig.BrokerURL + inject DICODE_BROKER_URL ([#84](https://github.com/dicode-ayo/dicode-core/issues/84)) ([#149](https://github.com/dicode-ayo/dicode-core/issues/149)) ([0b83b27](https://github.com/dicode-ayo/dicode-core/commit/0b83b27278cea36edbd5cfeb654ad371804ca5ff))
* **config,webui,cli:** add ai.task pointer, /api/ai/chat, and dicode ai CLI ([#140](https://github.com/dicode-ayo/dicode-core/issues/140)) ([41fe089](https://github.com/dicode-ayo/dicode-core/commit/41fe089d3f9bc593fe33b7677f0f4b7ae7c8647b))
* Deno SDK cleanup — stdio logging, Deno.env, TypeScript shim, Monaco IntelliSense ([#70](https://github.com/dicode-ayo/dicode-core/issues/70)) ([15acc3a](https://github.com/dicode-ayo/dicode-core/commit/15acc3a205754c83617bf4c476350177e38a1a99))
* **deploy:** production-ready Helm chart for dicode-core ([#230](https://github.com/dicode-ayo/dicode-core/issues/230)) ([0f5a841](https://github.com/dicode-ayo/dicode-core/commit/0f5a8414aa8735b62d63d053632ec7f39434d1ee))
* dev-mode branch lifecycle + on_failure_chain params + branch validator ([#236](https://github.com/dicode-ayo/dicode-core/issues/236)) ([#241](https://github.com/dicode-ayo/dicode-core/issues/241)) ([724e6c2](https://github.com/dicode-ayo/dicode-core/commit/724e6c2ce8c3c0760344f191308a34785299acba))
* dicode shim global — run_task, list_tasks, get_runs, get_config + security.allowed_tasks ([#33](https://github.com/dicode-ayo/dicode-core/issues/33)) ([9328ab7](https://github.com/dicode-ayo/dicode-core/commit/9328ab7fde0c26424babd237f8dbf4136afd03df))
* dicode.yaml as inline-root TaskSet (hard cut from sources array) ([#262](https://github.com/dicode-ayo/dicode-core/issues/262)) ([541ebdf](https://github.com/dicode-ayo/dicode-core/commit/541ebdf4b2d25175a45ca561ca3c0968b5f71391))
* docker executor ([288dcc3](https://github.com/dicode-ayo/dicode-core/commit/288dcc358ba433c44c7af3ff5514e19e67737959))
* doker engine ([7f3ee46](https://github.com/dicode-ayo/dicode-core/commit/7f3ee46fac97bd9434ab3f5a5e65b1d64698675c))
* **e2e:** revive Playwright stack — replaces [#18](https://github.com/dicode-ayo/dicode-core/issues/18)/[#19](https://github.com/dicode-ayo/dicode-core/issues/19)/[#20](https://github.com/dicode-ayo/dicode-core/issues/20) ([#150](https://github.com/dicode-ayo/dicode-core/issues/150)) ([92a53fd](https://github.com/dicode-ayo/dicode-core/commit/92a53fddaeb81d3c20581f966f3dbcc6a3172c19))
* enhanced config ([99e2727](https://github.com/dicode-ayo/dicode-core/commit/99e2727e6ae031954ec9ac2da701178e3937bdea))
* expose concurrency metrics (active tasks, memory, CPU) ([#75](https://github.com/dicode-ayo/dicode-core/issues/75)) ([91b32fd](https://github.com/dicode-ayo/dicode-core/commit/91b32fd6b7a1112bdbdf98dbbecf3986a9005074))
* init commit ([f3d1be9](https://github.com/dicode-ayo/dicode-core/commit/f3d1be93c3aa18964b2828fd48c38f5e01a13cb4))
* **ipc:** HTTP gateway — delete relay, route webhooks and daemon handlers through gateway ([#56](https://github.com/dicode-ayo/dicode-core/issues/56)) ([b5a235e](https://github.com/dicode-ayo/dicode-core/commit/b5a235eac9286ddca482bd03b072cd2676088674))
* **ipc:** unified IPC protocol with capability-based access control ([#55](https://github.com/dicode-ayo/dicode-core/issues/55)) ([d10c57c](https://github.com/dicode-ayo/dicode-core/commit/d10c57ccbef00e195ef639e489cdd470cb6c2742))
* **oauth:** relay broker flow — daemon plumbing, builtins, AAD binding, docs ([#100](https://github.com/dicode-ayo/dicode-core/issues/100)) ([c17f376](https://github.com/dicode-ayo/dicode-core/commit/c17f3765647ab3c232737c7778232204a6905a1d))
* **onboarding:** first-run wizard — CLI + browser, curated tasksets (closes [#85](https://github.com/dicode-ayo/dicode-core/issues/85)) ([#170](https://github.com/dicode-ayo/dicode-core/issues/170)) ([2b58c79](https://github.com/dicode-ayo/dicode-core/commit/2b58c792a383bb2e13ebd027d590c35738f6b1a4))
* PATCH /api/tasks/{id}/overrides + enable/disable toggle UI ([#265](https://github.com/dicode-ayo/dicode-core/issues/265)) ([b6a5f1f](https://github.com/dicode-ayo/dicode-core/commit/b6a5f1fc9590f90166a045dcf8e71685439cc185))
* persist and display structured output (output.html/text) ([32785bd](https://github.com/dicode-ayo/dicode-core/commit/32785bd8b2ca1975cfce79547a3639af8400ab9f))
* Python socket-bridge runtime, Podman executor, Dockerfile builds, examples ([#1](https://github.com/dicode-ayo/dicode-core/issues/1)) ([22b91ae](https://github.com/dicode-ayo/dicode-core/commit/22b91ae1d09700404dd8566c5d4003bce2f0d844))
* relay client with cryptographic identity ([#79](https://github.com/dicode-ayo/dicode-core/issues/79)) ([46c2097](https://github.com/dicode-ayo/dicode-core/commit/46c20974fc1df61a26c457f03e5aa62a5c637fb5))
* **relay:** rotate-identity CLI — split-key aware (replaces [#101](https://github.com/dicode-ayo/dicode-core/issues/101)) ([#141](https://github.com/dicode-ayo/dicode-core/issues/141)) ([e156f45](https://github.com/dicode-ayo/dicode-core/commit/e156f45c0001853cdf63bba5b3286edd50ad39d6))
* **release:** Dockerfile + dual-publish to Docker Hub & GHCR ([#227](https://github.com/dicode-ayo/dicode-core/issues/227)) ([8512401](https://github.com/dicode-ayo/dicode-core/commit/85124017a742a779cb23e16ff78bee4a880d445d))
* replace SSE+templates with WebSocket SPA architecture ([1ebcfd1](https://github.com/dicode-ayo/dicode-core/commit/1ebcfd1e888f0ffadc9e9ad15e5ae25d8a642b07))
* run grouping ([#116](https://github.com/dicode-ayo/dicode-core/issues/116)) — parent_run_id + group + set_group SDK + REST filters ([#250](https://github.com/dicode-ayo/dicode-core/issues/250)) ([9804dec](https://github.com/dicode-ayo/dicode-core/commit/9804dec41307f7d76b149cff6036cae9b2d70b4b))
* run-input persistence + encrypted storage + retention sweep ([#233](https://github.com/dicode-ayo/dicode-core/issues/233)) ([#243](https://github.com/dicode-ayo/dicode-core/issues/243)) ([ef66469](https://github.com/dicode-ayo/dicode-core/commit/ef66469ddf3a5d1d6287b106c69228e54b8b24da))
* secrets ([48079f2](https://github.com/dicode-ayo/dicode-core/commit/48079f2e70c8a0f31ceabf75ab4b4628fc744f47))
* **secrets:** task-based secret providers (Doppler reference) — closes [#119](https://github.com/dicode-ayo/dicode-core/issues/119) ([#232](https://github.com/dicode-ayo/dicode-core/issues/232)) ([3b6ecf1](https://github.com/dicode-ayo/dicode-core/commit/3b6ecf157ca0fa164f94c9cfae5bc93dc7bf792f))
* **security:** collapse two-tier auth — single login, secrets write-only ([#16](https://github.com/dicode-ayo/dicode-core/issues/16)) ([ade9e54](https://github.com/dicode-ayo/dicode-core/commit/ade9e545b6a2b3b1fa34738fdb3ae63d758c2168))
* **security:** global auth wall, trusted browser, webhook HMAC, MCP API keys ([#11](https://github.com/dicode-ayo/dicode-core/issues/11)) ([f458dd9](https://github.com/dicode-ayo/dicode-core/commit/f458dd9c8d18c24169bb7c774358afed3c7fbb1d))
* **security:** passphrase bootstrap — DB storage, auto-gen, change API ([#15](https://github.com/dicode-ayo/dicode-core/issues/15)) ([a176639](https://github.com/dicode-ayo/dicode-core/commit/a176639c464c3055fd60f25ac5ef03daf1d70374))
* **security:** webhook optional auth + dicode.js 401 handling ([#17](https://github.com/dicode-ayo/dicode-core/issues/17)) ([3cdc30d](https://github.com/dicode-ayo/dicode-core/commit/3cdc30d0df94c7bdae38da24036ecc84323c89cc))
* simple task runs ([7143d2b](https://github.com/dicode-ayo/dicode-core/commit/7143d2b84f00abbe6432af97b007e7803552a02d))
* some fixes ([1b6ffe6](https://github.com/dicode-ayo/dicode-core/commit/1b6ffe64baefd34860844c993ed45dce838b654c))
* TaskSet architecture — hierarchical task composition with dev mode & MCP ([#3](https://github.com/dicode-ayo/dicode-core/issues/3)) ([33fd7f4](https://github.com/dicode-ayo/dicode-core/commit/33fd7f43664b5d177db88da417dc23e4cdbdb3b4))
* **tasktest:** CLI + IPC + MCP surface for running task tests (Phase 1, Deno) ([#160](https://github.com/dicode-ayo/dicode-core/issues/160)) ([919998e](https://github.com/dicode-ayo/dicode-core/commit/919998e0f67ff5eba47dea8fc2a2e38cdbe9fbc2))
* temp file cleanup via builtin task ([#91](https://github.com/dicode-ayo/dicode-core/issues/91)) ([ae8902b](https://github.com/dicode-ayo/dicode-core/commit/ae8902bd06ba1df4421f6a5e971cbb62800dad36))
* transparent relay proxy + comprehensive docs update ([#80](https://github.com/dicode-ayo/dicode-core/issues/80)) ([87559b5](https://github.com/dicode-ayo/dicode-core/commit/87559b507266e2b64442dd44bffed0935814a8cb))
* tray icon ([5c80c77](https://github.com/dicode-ayo/dicode-core/commit/5c80c7774c192be76174286d5c3c84d4bb1997fa))
* triggers edit ([5b5c215](https://github.com/dicode-ayo/dicode-core/commit/5b5c215afd5527aebd41a994ec7711c6d4612708))
* **ui:** settings ([2e49b6e](https://github.com/dicode-ayo/dicode-core/commit/2e49b6ef4742b602f710f1581cc11f8189cb5438))
* webhook return the result ([b5ee783](https://github.com/dicode-ayo/dicode-core/commit/b5ee78384bfcfe9ef913bc0dd75ad16125a0489b))
* webhook task UIs — serve index.html + dicode.js client SDK ([#9](https://github.com/dicode-ayo/dicode-core/issues/9)) ([7acf4fc](https://github.com/dicode-ayo/dicode-core/commit/7acf4fc3de81582cbdead697272fbb639a293d1f))
* **webui,relay,taskset:** relay + per-source status in task list (closes [#87](https://github.com/dicode-ayo/dicode-core/issues/87)) ([#181](https://github.com/dicode-ayo/dicode-core/issues/181)) ([f58e72d](https://github.com/dicode-ayo/dicode-core/commit/f58e72dfb90c3afdbc42592b6afa678e8f57bbe0))
* **webui/auth:** bcrypt passphrase + configurable cost + race-safe migration ([#209](https://github.com/dicode-ayo/dicode-core/issues/209)) ([#219](https://github.com/dicode-ayo/dicode-core/issues/219)) ([c98e4df](https://github.com/dicode-ayo/dicode-core/commit/c98e4dfcaf110c0ac94d715ddb933db478129d5f))
* **webui:** adopt dicode design system via theme.css ([#92](https://github.com/dicode-ayo/dicode-core/issues/92)) ([3499b60](https://github.com/dicode-ayo/dicode-core/commit/3499b60349d304dc0d972ec5996e85488fbb4832))
* **webui:** collapse run list + parent/sub-runs in run detail ([#114](https://github.com/dicode-ayo/dicode-core/issues/114), [#115](https://github.com/dicode-ayo/dicode-core/issues/115)) ([#252](https://github.com/dicode-ayo/dicode-core/issues/252)) ([e11104e](https://github.com/dicode-ayo/dicode-core/commit/e11104e2e3e0ef9f4f2b282ecb365789b62bdbc4))
* **webui:** dc-toast listener wires up the missing toast sink ([#266](https://github.com/dicode-ayo/dicode-core/issues/266)) ([0cf2370](https://github.com/dicode-ayo/dicode-core/commit/0cf23707c5844aa8668197d5df0814808b8ba381))
* **webui:** migrate SPA to standalone webhook task ([#22](https://github.com/dicode-ayo/dicode-core/issues/22)) ([126fa11](https://github.com/dicode-ayo/dicode-core/commit/126fa11448086fc5e5e00d9e23d9afe2f9890f98))
* **webui:** POST /api/tasks/{id}/test REST endpoint ([#218](https://github.com/dicode-ayo/dicode-core/issues/218)) ([34145a0](https://github.com/dicode-ayo/dicode-core/commit/34145a0caff0c4a7bc223fbc0142761034fd154f))
* **webui:** zap-based HTTP request logger (closes [#23](https://github.com/dicode-ayo/dicode-core/issues/23)) ([#167](https://github.com/dicode-ayo/dicode-core/issues/167)) ([2e28c39](https://github.com/dicode-ayo/dicode-core/commit/2e28c39c7d6333f344c396f71a5afad235c706e5))
* zero-paste OAuth onboarding via env.if_missing + OpenRouter provider ([#117](https://github.com/dicode-ayo/dicode-core/issues/117)) ([6db1b30](https://github.com/dicode-ayo/dicode-core/commit/6db1b303b8b6f1ea7acabf12ed3938e22bacee65))


### Bug Fixes

* **buildin:** cleanup tasks null-tolerance + relay-client restart backoff ([#260](https://github.com/dicode-ayo/dicode-core/issues/260)) ([ecf9197](https://github.com/dicode-ayo/dicode-core/commit/ecf9197a2f2c3b45c32cda4c5db300370cf1183a))
* **buildin:** restore auth-start + auth-relay to working order ([#147](https://github.com/dicode-ayo/dicode-core/issues/147)) ([#148](https://github.com/dicode-ayo/dicode-core/issues/148)) ([0fc087f](https://github.com/dicode-ayo/dicode-core/commit/0fc087f90d42d47913e47a8a41fcbc532078c962))
* **ci:** gofmt violations, release tag format, add dicoded to goreleaser ([f64b9f7](https://github.com/dicode-ayo/dicode-core/commit/f64b9f70a91572d6ad01d9818772928a1771c909))
* **config:** honor watch:false and mcp:false in YAML ([#182](https://github.com/dicode-ayo/dicode-core/issues/182)) ([09cf2d7](https://github.com/dicode-ayo/dicode-core/commit/09cf2d7098709c03063641e1ded07491b0c17b40))
* only cap SQLite connections to 1 for :memory: databases ([eb5a1b8](https://github.com/dicode-ayo/dicode-core/commit/eb5a1b84934fcb2ee63e6007a69aea8f8a55a42c))
* persist cron next-run time to detect missed jobs on restart ([#51](https://github.com/dicode-ayo/dicode-core/issues/51)) ([21b12a1](https://github.com/dicode-ayo/dicode-core/commit/21b12a19c49580a63e0652e5b3c5464259670666))
* **relay:** refuse OAuth IPC during rotation + doc cleanup ([#144](https://github.com/dicode-ayo/dicode-core/issues/144) + [#143](https://github.com/dicode-ayo/dicode-core/issues/143) + [#145](https://github.com/dicode-ayo/dicode-core/issues/145)) ([#146](https://github.com/dicode-ayo/dicode-core/issues/146)) ([0c2bd65](https://github.com/dicode-ayo/dicode-core/commit/0c2bd654a37d276510d883a00c9c0a8adf49b832))
* **relay:** split Identity into SignKey + DecryptKey + require broker protocol 2 ([#104](https://github.com/dicode-ayo/dicode-core/issues/104)) ([#135](https://github.com/dicode-ayo/dicode-core/issues/135)) ([d801230](https://github.com/dicode-ayo/dicode-core/commit/d80123049e47fdeef400331829066e110b73dd27))
* **relay:** VerifyBrokerSig — match Node's double-hash signing shape ([#151](https://github.com/dicode-ayo/dicode-core/issues/151)) ([#152](https://github.com/dicode-ayo/dicode-core/issues/152)) ([fb6f713](https://github.com/dicode-ayo/dicode-core/commit/fb6f7137e3214726f286a87e8c585fc655347b9a))
* **release:** create GitHub releases as drafts + fix chain-params test flake ([#269](https://github.com/dicode-ayo/dicode-core/issues/269)) ([bb4bf8b](https://github.com/dicode-ayo/dicode-core/commit/bb4bf8b04114db2f908f41617ebefc06042f9aa4))
* **source:** block-with-ctx instead of dropping events on full channel ([#183](https://github.com/dicode-ayo/dicode-core/issues/183)) ([237a7b3](https://github.com/dicode-ayo/dicode-core/commit/237a7b325d9221aa8b2ea3a37282078b02220536))
* **taskset:** full clone instead of shallow (closes [#175](https://github.com/dicode-ayo/dicode-core/issues/175)) ([#176](https://github.com/dicode-ayo/dicode-core/issues/176)) ([3c392b0](https://github.com/dicode-ayo/dicode-core/commit/3c392b02fc16265916a334bd45ad65349e4e3f55))
* trayicon exit ([3a9df66](https://github.com/dicode-ayo/dicode-core/commit/3a9df66632f5ab81a052430ce5b09c6f30198210))
* ui aftet taskset implementation ([dfa9f63](https://github.com/dicode-ayo/dicode-core/commit/dfa9f633629f941f39082f6466408ec27d7beeca))
* web ui a bit ([cbd5009](https://github.com/dicode-ayo/dicode-core/commit/cbd50095f3991ae7e21e1c37c828e80228517cc0))
* **webui:** protect Server.cfg with sync.RWMutex (closes [#264](https://github.com/dicode-ayo/dicode-core/issues/264)) ([#267](https://github.com/dicode-ayo/dicode-core/issues/267)) ([18cc222](https://github.com/dicode-ayo/dicode-core/commit/18cc222d58426340ec9f3c749be83b1355b817f0))
* **webui:** redirect unauth webhook access to /login with return-to-origin ([#96](https://github.com/dicode-ayo/dicode-core/issues/96)) ([#131](https://github.com/dicode-ayo/dicode-core/issues/131)) ([fae727a](https://github.com/dicode-ayo/dicode-core/commit/fae727abf474d4254f03cd712236d0ac73a9d2b8))


### Performance Improvements

* batch log writes to SQLite instead of per-line inserts ([#76](https://github.com/dicode-ayo/dicode-core/issues/76)) ([2c81e21](https://github.com/dicode-ayo/dicode-core/commit/2c81e216bead26d3024651c2aaa13ff3d6bdbbb0))
* replace WaitRun() polling loop with channel notification ([#73](https://github.com/dicode-ayo/dicode-core/issues/73)) ([057d884](https://github.com/dicode-ayo/dicode-core/commit/057d884adc36cfb9fe0fc0657c4185d5ea0a47b3))


### Documentation

* **claude:** correct runtime architecture description ([#185](https://github.com/dicode-ayo/dicode-core/issues/185)) ([9aa4fd8](https://github.com/dicode-ayo/dicode-core/commit/9aa4fd87dc46d4f49a49e18aec6d7a1539fae449))
* latest status ([6467285](https://github.com/dicode-ayo/dicode-core/commit/6467285a49bc8955c3ac5a946f4c352909a09345))
* move back ([4c5ebb4](https://github.com/dicode-ayo/dicode-core/commit/4c5ebb444e6f387288be36ca723d7706d4b3faa7))
* move pages ([22cad0e](https://github.com/dicode-ayo/dicode-core/commit/22cad0e2e54fe79698c4ddd9f0617117de2736cb))
* **proto:** add proto/README explaining the dual-side regen workflow ([#205](https://github.com/dicode-ayo/dicode-core/issues/205)) ([da4ce5a](https://github.com/dicode-ayo/dicode-core/commit/da4ce5a01c426903621a729e2bd75a977e2f8757))
* **relay:** point self-host sections at dicode-relay repo ([#184](https://github.com/dicode-ayo/dicode-core/issues/184)) ([e4755be](https://github.com/dicode-ayo/dicode-core/commit/e4755be5f30c22fecdd3ddd654d5202592a5334f))
* **spec:** on-failure AI auto-fix loop design ([#228](https://github.com/dicode-ayo/dicode-core/issues/228)) ([#229](https://github.com/dicode-ayo/dicode-core/issues/229)) ([1418ee7](https://github.com/dicode-ayo/dicode-core/commit/1418ee76e98f748b120008c6ae7cbf3d3ce23148))
* **testing/e2e:** [#137](https://github.com/dicode-ayo/dicode-core/issues/137) Phase B coverage audit — close out all 7 scenarios ([#173](https://github.com/dicode-ayo/dicode-core/issues/173)) ([46b56aa](https://github.com/dicode-ayo/dicode-core/commit/46b56aab21c5f649b52b03a72917ccf2bba5d30f))
* update implementation-plan with current milestone statuses ([6f275db](https://github.com/dicode-ayo/dicode-core/commit/6f275dbc953ba2145210ae1d912657287e8da9cb))
* update readme ([db53ab7](https://github.com/dicode-ayo/dicode-core/commit/db53ab7a808ebc23e067c77a4487ee302d713f55))
* update status ([fdecf3b](https://github.com/dicode-ayo/dicode-core/commit/fdecf3b1abda17a965a8dd8807ada981aadb3988))
* update taskset ([fab20ed](https://github.com/dicode-ayo/dicode-core/commit/fab20ed31467ec52cf690df5bc82c818fe1563b2))


### Miscellaneous

* remove direct-AI Go code, port dev skill to task-based skills ([#134](https://github.com/dicode-ayo/dicode-core/issues/134)) ([32dd0e6](https://github.com/dicode-ayo/dicode-core/commit/32dd0e65393a1a8f2dd2c34276f932d1b116d2a8))

## [0.1.2](https://github.com/dicode-ayo/dicode-core/compare/v0.1.1...v0.1.2) (2026-05-06)


### Bug Fixes

* **release:** create GitHub releases as drafts + fix chain-params test flake ([#269](https://github.com/dicode-ayo/dicode-core/issues/269)) ([bb4bf8b](https://github.com/dicode-ayo/dicode-core/commit/bb4bf8b04114db2f908f41617ebefc06042f9aa4))

## [0.1.1](https://github.com/dicode-ayo/dicode-core/compare/v0.1.0...v0.1.1) (2026-05-06)


### Features

* **ai-agent:** set_group so chat conversations collapse in the run list ([#112](https://github.com/dicode-ayo/dicode-core/issues/112), [#113](https://github.com/dicode-ayo/dicode-core/issues/113)) ([#251](https://github.com/dicode-ayo/dicode-core/issues/251)) ([21dd4a9](https://github.com/dicode-ayo/dicode-core/commit/21dd4a98ffa045a57d63a3608145425764c12db3))
* auto-fix loop — engine guardrails + git-pr + dicode-auto-fix skill + auto-fix taskset entry ([#238](https://github.com/dicode-ayo/dicode-core/issues/238)) ([#247](https://github.com/dicode-ayo/dicode-core/issues/247)) ([9044612](https://github.com/dicode-ayo/dicode-core/commit/90446125cc92ffe96fb89a393f1223d159eb885f))
* auto-fix SDK surface (replay / tasks.test / sources.set_dev_mode / git.commit_push) ([#234](https://github.com/dicode-ayo/dicode-core/issues/234)) ([#245](https://github.com/dicode-ayo/dicode-core/issues/245)) ([92b43a7](https://github.com/dicode-ayo/dicode-core/commit/92b43a7a9b1c4abf15a0a74d04a8a93e907ec247))
* **buildin:** ai-agent-claude-cli — drive Claude via subscription, not API ([#248](https://github.com/dicode-ayo/dicode-core/issues/248)) ([6c63723](https://github.com/dicode-ayo/dicode-core/commit/6c63723a62146c02e105be1674e4b4aba8bc2e73))
* **buildin:** auth-providers dashboard + dicode.oauth.list_status ([#221](https://github.com/dicode-ayo/dicode-core/issues/221)) ([27cef46](https://github.com/dicode-ayo/dicode-core/commit/27cef46f378145e5fb32b258e5a14139a214a618))
* **deploy:** production-ready Helm chart for dicode-core ([#230](https://github.com/dicode-ayo/dicode-core/issues/230)) ([0f5a841](https://github.com/dicode-ayo/dicode-core/commit/0f5a8414aa8735b62d63d053632ec7f39434d1ee))
* dev-mode branch lifecycle + on_failure_chain params + branch validator ([#236](https://github.com/dicode-ayo/dicode-core/issues/236)) ([#241](https://github.com/dicode-ayo/dicode-core/issues/241)) ([724e6c2](https://github.com/dicode-ayo/dicode-core/commit/724e6c2ce8c3c0760344f191308a34785299acba))
* dicode.yaml as inline-root TaskSet (hard cut from sources array) ([#262](https://github.com/dicode-ayo/dicode-core/issues/262)) ([541ebdf](https://github.com/dicode-ayo/dicode-core/commit/541ebdf4b2d25175a45ca561ca3c0968b5f71391))
* PATCH /api/tasks/{id}/overrides + enable/disable toggle UI ([#265](https://github.com/dicode-ayo/dicode-core/issues/265)) ([b6a5f1f](https://github.com/dicode-ayo/dicode-core/commit/b6a5f1fc9590f90166a045dcf8e71685439cc185))
* **release:** Dockerfile + dual-publish to Docker Hub & GHCR ([#227](https://github.com/dicode-ayo/dicode-core/issues/227)) ([8512401](https://github.com/dicode-ayo/dicode-core/commit/85124017a742a779cb23e16ff78bee4a880d445d))
* run grouping ([#116](https://github.com/dicode-ayo/dicode-core/issues/116)) — parent_run_id + group + set_group SDK + REST filters ([#250](https://github.com/dicode-ayo/dicode-core/issues/250)) ([9804dec](https://github.com/dicode-ayo/dicode-core/commit/9804dec41307f7d76b149cff6036cae9b2d70b4b))
* run-input persistence + encrypted storage + retention sweep ([#233](https://github.com/dicode-ayo/dicode-core/issues/233)) ([#243](https://github.com/dicode-ayo/dicode-core/issues/243)) ([ef66469](https://github.com/dicode-ayo/dicode-core/commit/ef66469ddf3a5d1d6287b106c69228e54b8b24da))
* **secrets:** task-based secret providers (Doppler reference) — closes [#119](https://github.com/dicode-ayo/dicode-core/issues/119) ([#232](https://github.com/dicode-ayo/dicode-core/issues/232)) ([3b6ecf1](https://github.com/dicode-ayo/dicode-core/commit/3b6ecf157ca0fa164f94c9cfae5bc93dc7bf792f))
* **webui/auth:** bcrypt passphrase + configurable cost + race-safe migration ([#209](https://github.com/dicode-ayo/dicode-core/issues/209)) ([#219](https://github.com/dicode-ayo/dicode-core/issues/219)) ([c98e4df](https://github.com/dicode-ayo/dicode-core/commit/c98e4dfcaf110c0ac94d715ddb933db478129d5f))
* **webui:** collapse run list + parent/sub-runs in run detail ([#114](https://github.com/dicode-ayo/dicode-core/issues/114), [#115](https://github.com/dicode-ayo/dicode-core/issues/115)) ([#252](https://github.com/dicode-ayo/dicode-core/issues/252)) ([e11104e](https://github.com/dicode-ayo/dicode-core/commit/e11104e2e3e0ef9f4f2b282ecb365789b62bdbc4))
* **webui:** dc-toast listener wires up the missing toast sink ([#266](https://github.com/dicode-ayo/dicode-core/issues/266)) ([0cf2370](https://github.com/dicode-ayo/dicode-core/commit/0cf23707c5844aa8668197d5df0814808b8ba381))
* **webui:** POST /api/tasks/{id}/test REST endpoint ([#218](https://github.com/dicode-ayo/dicode-core/issues/218)) ([34145a0](https://github.com/dicode-ayo/dicode-core/commit/34145a0caff0c4a7bc223fbc0142761034fd154f))


### Bug Fixes

* **buildin:** cleanup tasks null-tolerance + relay-client restart backoff ([#260](https://github.com/dicode-ayo/dicode-core/issues/260)) ([ecf9197](https://github.com/dicode-ayo/dicode-core/commit/ecf9197a2f2c3b45c32cda4c5db300370cf1183a))
* **webui:** protect Server.cfg with sync.RWMutex (closes [#264](https://github.com/dicode-ayo/dicode-core/issues/264)) ([#267](https://github.com/dicode-ayo/dicode-core/issues/267)) ([18cc222](https://github.com/dicode-ayo/dicode-core/commit/18cc222d58426340ec9f3c749be83b1355b817f0))


### Documentation

* **spec:** on-failure AI auto-fix loop design ([#228](https://github.com/dicode-ayo/dicode-core/issues/228)) ([#229](https://github.com/dicode-ayo/dicode-core/issues/229)) ([1418ee7](https://github.com/dicode-ayo/dicode-core/commit/1418ee76e98f748b120008c6ae7cbf3d3ce23148))

## [Unreleased]

### Fixed

- **`auth-providers` UI now correctly shows connected providers** via the new
  `dicode.secrets.has` IPC verb (closes #255). Previously the `has_token`
  field was hardcoded `false` for every provider; tokens written by
  `auth-relay` are now reflected immediately.

### Added

- **`dicode.secrets.has(key)` IPC verb** — cap-gated boolean presence check;
  never returns the secret value. Enable with
  `permissions.dicode.secrets_has: true` in `task.yaml`. Symmetric with
  `secrets_write` so tasks can check presence without write rights.
- **Provider list now sourced dynamically from the dicode-relay broker's
  `GET /providers` endpoint** (requires dicode-relay >= 0.1.5). Eliminates
  the hardcoded provider list maintenance burden — adding or removing a
  provider in `relay.yaml` is immediately reflected without a dicode-core
  release.

### Breaking

- **`tasks/auth/taskset.yaml` per-provider entries removed.** All 15
  `_oauth-app` inheritor entries (github, slack, google, spotify, linear,
  discord, gitlab, airtable, notion, confluence, salesforce, stripe,
  office365, azure, looker) were deleted. The dicode-relay broker (>= 0.1.5)
  handles the first 14 end-to-end via `buildin/auth-{providers,start,relay}`;
  for any provider the broker doesn't carry (looker, plus anything BYO),
  operators instantiate `_oauth-app/task.yaml` from their own taskset and
  the dashboard's auth-providers panel auto-discovers it via the new
  `template: dicode.io/oauth-app` marker. The only entry that remains in
  this taskset is `openrouter-oauth` — a standalone non-broker PKCE flow
  that uses a callback URL request param, so the broker can't proxy it.
  Operators with `/hooks/<provider>-oauth` callback URLs registered at
  GitHub/Google/Slack/etc. for their own OAuth apps have two migration
  paths: (a) switch to the broker flow — no callback re-registration
  needed; (b) instantiate `auth/_oauth-app/task.yaml` from their own
  taskset with a fresh `/hooks/<their-name>-oauth` route and re-register
  the callback. See `docs/oauth.md` and the header of
  `tasks/auth/taskset.yaml` for the BYO walkthrough.
- **`buildin/auth-providers` `providers` param default changed** from a
  hardcoded 16-key list to `""` (= "all"). With the new broker-backed
  catalogue, an empty `providers` parameter now returns every provider the
  broker reports plus the `STANDALONE` entries (currently `openrouter`).
  Callers that depended on the previous fixed list must pass the explicit
  comma-separated subset they want.
- **Relay client migrated from Go to TypeScript task.** Existing users with
  relay enabled will see a new daemon UUID on first boot of this version.
  Existing webhook URLs (`https://relay.dicode.app/u/<old-uuid>/hooks/...`)
  stop working; reissue them as needed.

  The legacy SQLite kv row at `relay.private_key` is no longer used; the
  new identity blob lives at `<DATADIR>/relay-store/relay/identity/v1.bin`,
  encrypted at rest via `dicode.crypto`.

- **`dicode relay rotate-identity` CLI removed.** New rotation procedure:
  stop the daemon, delete `<DATADIR>/relay-store/relay/identity/v1.bin`,
  restart. The relay-client task regenerates the identity on next boot.

### Removed

- `pkg/relay/` package (Go WSS client).
- `pkg/ipc/oauth_*.go` IPC verbs (`build_auth_url`, `store_token`,
  `list_status`). Replaced by `dicode.crypto.{encrypt, decrypt}` +
  task-side composition.
- `proto/relay.proto` and generated protobuf code.
- `tests/e2e/relay/`.
- `dicode.oauth.*` SDK methods (build_auth_url, store_token, list_status).
- `permissions.dicode.{oauth_init, oauth_store, oauth_status}`.

### Added

- `dicode.crypto.{encrypt, decrypt}` IPC verb pair (context-scoped sub-key
  derivation, AES-GCM with AAD-bound context).
- `silent: true` task.yaml flag (detaches stdout/stderr from log capture
  for tasks handling plaintext credentials).
- `tasks/buildin/relay-client/` daemon task.
- Configurable `prefix` param on `tasks/buildin/local-storage/`.

## [0.1.0](https://github.com/dicode-ayo/dicode-core/compare/v0.0.4...v0.1.0) (2026-04-25)


### ⚠ BREAKING CHANGES

* remove direct-AI Go code, port dev skill to task-based skills ([#134](https://github.com/dicode-ayo/dicode-core/issues/134))

### Features

* **config,runtime:** add RelayConfig.BrokerURL + inject DICODE_BROKER_URL ([#84](https://github.com/dicode-ayo/dicode-core/issues/84)) ([#149](https://github.com/dicode-ayo/dicode-core/issues/149)) ([0b83b27](https://github.com/dicode-ayo/dicode-core/commit/0b83b27278cea36edbd5cfeb654ad371804ca5ff))
* **config,webui,cli:** add ai.task pointer, /api/ai/chat, and dicode ai CLI ([#140](https://github.com/dicode-ayo/dicode-core/issues/140)) ([41fe089](https://github.com/dicode-ayo/dicode-core/commit/41fe089d3f9bc593fe33b7677f0f4b7ae7c8647b))
* **e2e:** revive Playwright stack — replaces [#18](https://github.com/dicode-ayo/dicode-core/issues/18)/[#19](https://github.com/dicode-ayo/dicode-core/issues/19)/[#20](https://github.com/dicode-ayo/dicode-core/issues/20) ([#150](https://github.com/dicode-ayo/dicode-core/issues/150)) ([92a53fd](https://github.com/dicode-ayo/dicode-core/commit/92a53fddaeb81d3c20581f966f3dbcc6a3172c19))
* **onboarding:** first-run wizard — CLI + browser, curated tasksets (closes [#85](https://github.com/dicode-ayo/dicode-core/issues/85)) ([#170](https://github.com/dicode-ayo/dicode-core/issues/170)) ([2b58c79](https://github.com/dicode-ayo/dicode-core/commit/2b58c792a383bb2e13ebd027d590c35738f6b1a4))
* **relay:** rotate-identity CLI — split-key aware (replaces [#101](https://github.com/dicode-ayo/dicode-core/issues/101)) ([#141](https://github.com/dicode-ayo/dicode-core/issues/141)) ([e156f45](https://github.com/dicode-ayo/dicode-core/commit/e156f45c0001853cdf63bba5b3286edd50ad39d6))
* **tasktest:** CLI + IPC + MCP surface for running task tests (Phase 1, Deno) ([#160](https://github.com/dicode-ayo/dicode-core/issues/160)) ([919998e](https://github.com/dicode-ayo/dicode-core/commit/919998e0f67ff5eba47dea8fc2a2e38cdbe9fbc2))
* **webui,relay,taskset:** relay + per-source status in task list (closes [#87](https://github.com/dicode-ayo/dicode-core/issues/87)) ([#181](https://github.com/dicode-ayo/dicode-core/issues/181)) ([f58e72d](https://github.com/dicode-ayo/dicode-core/commit/f58e72dfb90c3afdbc42592b6afa678e8f57bbe0))
* **webui:** zap-based HTTP request logger (closes [#23](https://github.com/dicode-ayo/dicode-core/issues/23)) ([#167](https://github.com/dicode-ayo/dicode-core/issues/167)) ([2e28c39](https://github.com/dicode-ayo/dicode-core/commit/2e28c39c7d6333f344c396f71a5afad235c706e5))
* zero-paste OAuth onboarding via env.if_missing + OpenRouter provider ([#117](https://github.com/dicode-ayo/dicode-core/issues/117)) ([6db1b30](https://github.com/dicode-ayo/dicode-core/commit/6db1b303b8b6f1ea7acabf12ed3938e22bacee65))


### Bug Fixes

* **buildin:** restore auth-start + auth-relay to working order ([#147](https://github.com/dicode-ayo/dicode-core/issues/147)) ([#148](https://github.com/dicode-ayo/dicode-core/issues/148)) ([0fc087f](https://github.com/dicode-ayo/dicode-core/commit/0fc087f90d42d47913e47a8a41fcbc532078c962))
* **config:** honor watch:false and mcp:false in YAML ([#182](https://github.com/dicode-ayo/dicode-core/issues/182)) ([09cf2d7](https://github.com/dicode-ayo/dicode-core/commit/09cf2d7098709c03063641e1ded07491b0c17b40))
* **relay:** refuse OAuth IPC during rotation + doc cleanup ([#144](https://github.com/dicode-ayo/dicode-core/issues/144) + [#143](https://github.com/dicode-ayo/dicode-core/issues/143) + [#145](https://github.com/dicode-ayo/dicode-core/issues/145)) ([#146](https://github.com/dicode-ayo/dicode-core/issues/146)) ([0c2bd65](https://github.com/dicode-ayo/dicode-core/commit/0c2bd654a37d276510d883a00c9c0a8adf49b832))
* **relay:** split Identity into SignKey + DecryptKey + require broker protocol 2 ([#104](https://github.com/dicode-ayo/dicode-core/issues/104)) ([#135](https://github.com/dicode-ayo/dicode-core/issues/135)) ([d801230](https://github.com/dicode-ayo/dicode-core/commit/d80123049e47fdeef400331829066e110b73dd27))
* **relay:** VerifyBrokerSig — match Node's double-hash signing shape ([#151](https://github.com/dicode-ayo/dicode-core/issues/151)) ([#152](https://github.com/dicode-ayo/dicode-core/issues/152)) ([fb6f713](https://github.com/dicode-ayo/dicode-core/commit/fb6f7137e3214726f286a87e8c585fc655347b9a))
* **source:** block-with-ctx instead of dropping events on full channel ([#183](https://github.com/dicode-ayo/dicode-core/issues/183)) ([237a7b3](https://github.com/dicode-ayo/dicode-core/commit/237a7b325d9221aa8b2ea3a37282078b02220536))
* **taskset:** full clone instead of shallow (closes [#175](https://github.com/dicode-ayo/dicode-core/issues/175)) ([#176](https://github.com/dicode-ayo/dicode-core/issues/176)) ([3c392b0](https://github.com/dicode-ayo/dicode-core/commit/3c392b02fc16265916a334bd45ad65349e4e3f55))
* **webui:** redirect unauth webhook access to /login with return-to-origin ([#96](https://github.com/dicode-ayo/dicode-core/issues/96)) ([#131](https://github.com/dicode-ayo/dicode-core/issues/131)) ([fae727a](https://github.com/dicode-ayo/dicode-core/commit/fae727abf474d4254f03cd712236d0ac73a9d2b8))


### Performance Improvements

* batch log writes to SQLite instead of per-line inserts ([#76](https://github.com/dicode-ayo/dicode-core/issues/76)) ([2c81e21](https://github.com/dicode-ayo/dicode-core/commit/2c81e216bead26d3024651c2aaa13ff3d6bdbbb0))


### Documentation

* **claude:** correct runtime architecture description ([#185](https://github.com/dicode-ayo/dicode-core/issues/185)) ([9aa4fd8](https://github.com/dicode-ayo/dicode-core/commit/9aa4fd87dc46d4f49a49e18aec6d7a1539fae449))
* **proto:** add proto/README explaining the dual-side regen workflow ([#205](https://github.com/dicode-ayo/dicode-core/issues/205)) ([da4ce5a](https://github.com/dicode-ayo/dicode-core/commit/da4ce5a01c426903621a729e2bd75a977e2f8757))
* **relay:** point self-host sections at dicode-relay repo ([#184](https://github.com/dicode-ayo/dicode-core/issues/184)) ([e4755be](https://github.com/dicode-ayo/dicode-core/commit/e4755be5f30c22fecdd3ddd654d5202592a5334f))
* **testing/e2e:** [#137](https://github.com/dicode-ayo/dicode-core/issues/137) Phase B coverage audit — close out all 7 scenarios ([#173](https://github.com/dicode-ayo/dicode-core/issues/173)) ([46b56aa](https://github.com/dicode-ayo/dicode-core/commit/46b56aab21c5f649b52b03a72917ccf2bba5d30f))
* update readme ([db53ab7](https://github.com/dicode-ayo/dicode-core/commit/db53ab7a808ebc23e067c77a4487ee302d713f55))


### Miscellaneous

* remove direct-AI Go code, port dev skill to task-based skills ([#134](https://github.com/dicode-ayo/dicode-core/issues/134)) ([32dd0e6](https://github.com/dicode-ayo/dicode-core/commit/32dd0e65393a1a8f2dd2c34276f932d1b116d2a8))

## [0.0.4](https://github.com/dicode-ayo/dicode-core/compare/v0.0.3...v0.0.4) (2026-04-17)


### Features

* **#48:** split monolith into dicoded daemon + dicode CLI ([#57](https://github.com/dicode-ayo/dicode-core/issues/57)) ([257d590](https://github.com/dicode-ayo/dicode-core/commit/257d5901994fe571565a5d62373fb9b62daddac3))
* add github-stars example task ([55ed356](https://github.com/dicode-ayo/dicode-core/commit/55ed356c7a8ee734bd7847b8372c089c4cb9eddf))
* add max concurrent tasks semaphore in fireAsync() ([#74](https://github.com/dicode-ayo/dicode-core/issues/74)) ([ec8677c](https://github.com/dicode-ayo/dicode-core/commit/ec8677cbd2cc3d0caaed93abbbebaa2613d0f81f))
* ai ([80c8ce1](https://github.com/dicode-ayo/dicode-core/commit/80c8ce1d60f74ff99040c7c00d3db0c73e0d7bd9))
* browser notifications, run events SSE, return value storage ([868cb1d](https://github.com/dicode-ayo/dicode-core/commit/868cb1da5cd7d355ade7e25c7a60ad29bac8c812))
* **buildin:** ai-agent chat task + task.yaml template vars ([#98](https://github.com/dicode-ayo/dicode-core/issues/98)) ([3d8c5ec](https://github.com/dicode-ayo/dicode-core/commit/3d8c5ec3b34e4c759a8a718e8f10cc553519538a))
* clean up before going public ([ee2a149](https://github.com/dicode-ayo/dicode-core/commit/ee2a1491b0de271b1ae73adca0901d1663aa1d4e))
* Deno SDK cleanup — stdio logging, Deno.env, TypeScript shim, Monaco IntelliSense ([#70](https://github.com/dicode-ayo/dicode-core/issues/70)) ([15acc3a](https://github.com/dicode-ayo/dicode-core/commit/15acc3a205754c83617bf4c476350177e38a1a99))
* dicode shim global — run_task, list_tasks, get_runs, get_config + security.allowed_tasks ([#33](https://github.com/dicode-ayo/dicode-core/issues/33)) ([9328ab7](https://github.com/dicode-ayo/dicode-core/commit/9328ab7fde0c26424babd237f8dbf4136afd03df))
* docker executor ([288dcc3](https://github.com/dicode-ayo/dicode-core/commit/288dcc358ba433c44c7af3ff5514e19e67737959))
* doker engine ([7f3ee46](https://github.com/dicode-ayo/dicode-core/commit/7f3ee46fac97bd9434ab3f5a5e65b1d64698675c))
* enhanced config ([99e2727](https://github.com/dicode-ayo/dicode-core/commit/99e2727e6ae031954ec9ac2da701178e3937bdea))
* expose concurrency metrics (active tasks, memory, CPU) ([#75](https://github.com/dicode-ayo/dicode-core/issues/75)) ([91b32fd](https://github.com/dicode-ayo/dicode-core/commit/91b32fd6b7a1112bdbdf98dbbecf3986a9005074))
* init commit ([f3d1be9](https://github.com/dicode-ayo/dicode-core/commit/f3d1be93c3aa18964b2828fd48c38f5e01a13cb4))
* **ipc:** HTTP gateway — delete relay, route webhooks and daemon handlers through gateway ([#56](https://github.com/dicode-ayo/dicode-core/issues/56)) ([b5a235e](https://github.com/dicode-ayo/dicode-core/commit/b5a235eac9286ddca482bd03b072cd2676088674))
* **ipc:** unified IPC protocol with capability-based access control ([#55](https://github.com/dicode-ayo/dicode-core/issues/55)) ([d10c57c](https://github.com/dicode-ayo/dicode-core/commit/d10c57ccbef00e195ef639e489cdd470cb6c2742))
* **oauth:** relay broker flow — daemon plumbing, builtins, AAD binding, docs ([#100](https://github.com/dicode-ayo/dicode-core/issues/100)) ([c17f376](https://github.com/dicode-ayo/dicode-core/commit/c17f3765647ab3c232737c7778232204a6905a1d))
* persist and display structured output (output.html/text) ([32785bd](https://github.com/dicode-ayo/dicode-core/commit/32785bd8b2ca1975cfce79547a3639af8400ab9f))
* Python socket-bridge runtime, Podman executor, Dockerfile builds, examples ([#1](https://github.com/dicode-ayo/dicode-core/issues/1)) ([22b91ae](https://github.com/dicode-ayo/dicode-core/commit/22b91ae1d09700404dd8566c5d4003bce2f0d844))
* relay client with cryptographic identity ([#79](https://github.com/dicode-ayo/dicode-core/issues/79)) ([46c2097](https://github.com/dicode-ayo/dicode-core/commit/46c20974fc1df61a26c457f03e5aa62a5c637fb5))
* replace SSE+templates with WebSocket SPA architecture ([1ebcfd1](https://github.com/dicode-ayo/dicode-core/commit/1ebcfd1e888f0ffadc9e9ad15e5ae25d8a642b07))
* secrets ([48079f2](https://github.com/dicode-ayo/dicode-core/commit/48079f2e70c8a0f31ceabf75ab4b4628fc744f47))
* **security:** collapse two-tier auth — single login, secrets write-only ([#16](https://github.com/dicode-ayo/dicode-core/issues/16)) ([ade9e54](https://github.com/dicode-ayo/dicode-core/commit/ade9e545b6a2b3b1fa34738fdb3ae63d758c2168))
* **security:** global auth wall, trusted browser, webhook HMAC, MCP API keys ([#11](https://github.com/dicode-ayo/dicode-core/issues/11)) ([f458dd9](https://github.com/dicode-ayo/dicode-core/commit/f458dd9c8d18c24169bb7c774358afed3c7fbb1d))
* **security:** passphrase bootstrap — DB storage, auto-gen, change API ([#15](https://github.com/dicode-ayo/dicode-core/issues/15)) ([a176639](https://github.com/dicode-ayo/dicode-core/commit/a176639c464c3055fd60f25ac5ef03daf1d70374))
* **security:** webhook optional auth + dicode.js 401 handling ([#17](https://github.com/dicode-ayo/dicode-core/issues/17)) ([3cdc30d](https://github.com/dicode-ayo/dicode-core/commit/3cdc30d0df94c7bdae38da24036ecc84323c89cc))
* simple task runs ([7143d2b](https://github.com/dicode-ayo/dicode-core/commit/7143d2b84f00abbe6432af97b007e7803552a02d))
* some fixes ([1b6ffe6](https://github.com/dicode-ayo/dicode-core/commit/1b6ffe64baefd34860844c993ed45dce838b654c))
* TaskSet architecture — hierarchical task composition with dev mode & MCP ([#3](https://github.com/dicode-ayo/dicode-core/issues/3)) ([33fd7f4](https://github.com/dicode-ayo/dicode-core/commit/33fd7f43664b5d177db88da417dc23e4cdbdb3b4))
* temp file cleanup via builtin task ([#91](https://github.com/dicode-ayo/dicode-core/issues/91)) ([ae8902b](https://github.com/dicode-ayo/dicode-core/commit/ae8902bd06ba1df4421f6a5e971cbb62800dad36))
* transparent relay proxy + comprehensive docs update ([#80](https://github.com/dicode-ayo/dicode-core/issues/80)) ([87559b5](https://github.com/dicode-ayo/dicode-core/commit/87559b507266e2b64442dd44bffed0935814a8cb))
* tray icon ([5c80c77](https://github.com/dicode-ayo/dicode-core/commit/5c80c7774c192be76174286d5c3c84d4bb1997fa))
* triggers edit ([5b5c215](https://github.com/dicode-ayo/dicode-core/commit/5b5c215afd5527aebd41a994ec7711c6d4612708))
* **ui:** settings ([2e49b6e](https://github.com/dicode-ayo/dicode-core/commit/2e49b6ef4742b602f710f1581cc11f8189cb5438))
* webhook return the result ([b5ee783](https://github.com/dicode-ayo/dicode-core/commit/b5ee78384bfcfe9ef913bc0dd75ad16125a0489b))
* webhook task UIs — serve index.html + dicode.js client SDK ([#9](https://github.com/dicode-ayo/dicode-core/issues/9)) ([7acf4fc](https://github.com/dicode-ayo/dicode-core/commit/7acf4fc3de81582cbdead697272fbb639a293d1f))
* **webui:** adopt dicode design system via theme.css ([#92](https://github.com/dicode-ayo/dicode-core/issues/92)) ([3499b60](https://github.com/dicode-ayo/dicode-core/commit/3499b60349d304dc0d972ec5996e85488fbb4832))
* **webui:** migrate SPA to standalone webhook task ([#22](https://github.com/dicode-ayo/dicode-core/issues/22)) ([126fa11](https://github.com/dicode-ayo/dicode-core/commit/126fa11448086fc5e5e00d9e23d9afe2f9890f98))


### Bug Fixes

* **ci:** gofmt violations, release tag format, add dicoded to goreleaser ([f64b9f7](https://github.com/dicode-ayo/dicode-core/commit/f64b9f70a91572d6ad01d9818772928a1771c909))
* only cap SQLite connections to 1 for :memory: databases ([eb5a1b8](https://github.com/dicode-ayo/dicode-core/commit/eb5a1b84934fcb2ee63e6007a69aea8f8a55a42c))
* persist cron next-run time to detect missed jobs on restart ([#51](https://github.com/dicode-ayo/dicode-core/issues/51)) ([21b12a1](https://github.com/dicode-ayo/dicode-core/commit/21b12a19c49580a63e0652e5b3c5464259670666))
* trayicon exit ([3a9df66](https://github.com/dicode-ayo/dicode-core/commit/3a9df66632f5ab81a052430ce5b09c6f30198210))
* ui aftet taskset implementation ([dfa9f63](https://github.com/dicode-ayo/dicode-core/commit/dfa9f633629f941f39082f6466408ec27d7beeca))
* web ui a bit ([cbd5009](https://github.com/dicode-ayo/dicode-core/commit/cbd50095f3991ae7e21e1c37c828e80228517cc0))


### Performance Improvements

* replace WaitRun() polling loop with channel notification ([#73](https://github.com/dicode-ayo/dicode-core/issues/73)) ([057d884](https://github.com/dicode-ayo/dicode-core/commit/057d884adc36cfb9fe0fc0657c4185d5ea0a47b3))


### Documentation

* latest status ([6467285](https://github.com/dicode-ayo/dicode-core/commit/6467285a49bc8955c3ac5a946f4c352909a09345))
* move back ([4c5ebb4](https://github.com/dicode-ayo/dicode-core/commit/4c5ebb444e6f387288be36ca723d7706d4b3faa7))
* move pages ([22cad0e](https://github.com/dicode-ayo/dicode-core/commit/22cad0e2e54fe79698c4ddd9f0617117de2736cb))
* update implementation-plan with current milestone statuses ([6f275db](https://github.com/dicode-ayo/dicode-core/commit/6f275dbc953ba2145210ae1d912657287e8da9cb))
* update status ([fdecf3b](https://github.com/dicode-ayo/dicode-core/commit/fdecf3b1abda17a965a8dd8807ada981aadb3988))
* update taskset ([fab20ed](https://github.com/dicode-ayo/dicode-core/commit/fab20ed31467ec52cf690df5bc82c818fe1563b2))

## [0.0.3](https://github.com/dicode-ayo/dicode-core/compare/dicode-v0.0.2...dicode-v0.0.3) (2026-04-17)


### Features

* **#48:** split monolith into dicoded daemon + dicode CLI ([#57](https://github.com/dicode-ayo/dicode-core/issues/57)) ([257d590](https://github.com/dicode-ayo/dicode-core/commit/257d5901994fe571565a5d62373fb9b62daddac3))
* add github-stars example task ([55ed356](https://github.com/dicode-ayo/dicode-core/commit/55ed356c7a8ee734bd7847b8372c089c4cb9eddf))
* add max concurrent tasks semaphore in fireAsync() ([#74](https://github.com/dicode-ayo/dicode-core/issues/74)) ([ec8677c](https://github.com/dicode-ayo/dicode-core/commit/ec8677cbd2cc3d0caaed93abbbebaa2613d0f81f))
* ai ([80c8ce1](https://github.com/dicode-ayo/dicode-core/commit/80c8ce1d60f74ff99040c7c00d3db0c73e0d7bd9))
* browser notifications, run events SSE, return value storage ([868cb1d](https://github.com/dicode-ayo/dicode-core/commit/868cb1da5cd7d355ade7e25c7a60ad29bac8c812))
* **buildin:** ai-agent chat task + task.yaml template vars ([#98](https://github.com/dicode-ayo/dicode-core/issues/98)) ([3d8c5ec](https://github.com/dicode-ayo/dicode-core/commit/3d8c5ec3b34e4c759a8a718e8f10cc553519538a))
* clean up before going public ([ee2a149](https://github.com/dicode-ayo/dicode-core/commit/ee2a1491b0de271b1ae73adca0901d1663aa1d4e))
* Deno SDK cleanup — stdio logging, Deno.env, TypeScript shim, Monaco IntelliSense ([#70](https://github.com/dicode-ayo/dicode-core/issues/70)) ([15acc3a](https://github.com/dicode-ayo/dicode-core/commit/15acc3a205754c83617bf4c476350177e38a1a99))
* dicode shim global — run_task, list_tasks, get_runs, get_config + security.allowed_tasks ([#33](https://github.com/dicode-ayo/dicode-core/issues/33)) ([9328ab7](https://github.com/dicode-ayo/dicode-core/commit/9328ab7fde0c26424babd237f8dbf4136afd03df))
* docker executor ([288dcc3](https://github.com/dicode-ayo/dicode-core/commit/288dcc358ba433c44c7af3ff5514e19e67737959))
* doker engine ([7f3ee46](https://github.com/dicode-ayo/dicode-core/commit/7f3ee46fac97bd9434ab3f5a5e65b1d64698675c))
* enhanced config ([99e2727](https://github.com/dicode-ayo/dicode-core/commit/99e2727e6ae031954ec9ac2da701178e3937bdea))
* expose concurrency metrics (active tasks, memory, CPU) ([#75](https://github.com/dicode-ayo/dicode-core/issues/75)) ([91b32fd](https://github.com/dicode-ayo/dicode-core/commit/91b32fd6b7a1112bdbdf98dbbecf3986a9005074))
* init commit ([f3d1be9](https://github.com/dicode-ayo/dicode-core/commit/f3d1be93c3aa18964b2828fd48c38f5e01a13cb4))
* **ipc:** HTTP gateway — delete relay, route webhooks and daemon handlers through gateway ([#56](https://github.com/dicode-ayo/dicode-core/issues/56)) ([b5a235e](https://github.com/dicode-ayo/dicode-core/commit/b5a235eac9286ddca482bd03b072cd2676088674))
* **ipc:** unified IPC protocol with capability-based access control ([#55](https://github.com/dicode-ayo/dicode-core/issues/55)) ([d10c57c](https://github.com/dicode-ayo/dicode-core/commit/d10c57ccbef00e195ef639e489cdd470cb6c2742))
* **oauth:** relay broker flow — daemon plumbing, builtins, AAD binding, docs ([#100](https://github.com/dicode-ayo/dicode-core/issues/100)) ([c17f376](https://github.com/dicode-ayo/dicode-core/commit/c17f3765647ab3c232737c7778232204a6905a1d))
* persist and display structured output (output.html/text) ([32785bd](https://github.com/dicode-ayo/dicode-core/commit/32785bd8b2ca1975cfce79547a3639af8400ab9f))
* Python socket-bridge runtime, Podman executor, Dockerfile builds, examples ([#1](https://github.com/dicode-ayo/dicode-core/issues/1)) ([22b91ae](https://github.com/dicode-ayo/dicode-core/commit/22b91ae1d09700404dd8566c5d4003bce2f0d844))
* relay client with cryptographic identity ([#79](https://github.com/dicode-ayo/dicode-core/issues/79)) ([46c2097](https://github.com/dicode-ayo/dicode-core/commit/46c20974fc1df61a26c457f03e5aa62a5c637fb5))
* replace SSE+templates with WebSocket SPA architecture ([1ebcfd1](https://github.com/dicode-ayo/dicode-core/commit/1ebcfd1e888f0ffadc9e9ad15e5ae25d8a642b07))
* secrets ([48079f2](https://github.com/dicode-ayo/dicode-core/commit/48079f2e70c8a0f31ceabf75ab4b4628fc744f47))
* **security:** collapse two-tier auth — single login, secrets write-only ([#16](https://github.com/dicode-ayo/dicode-core/issues/16)) ([ade9e54](https://github.com/dicode-ayo/dicode-core/commit/ade9e545b6a2b3b1fa34738fdb3ae63d758c2168))
* **security:** global auth wall, trusted browser, webhook HMAC, MCP API keys ([#11](https://github.com/dicode-ayo/dicode-core/issues/11)) ([f458dd9](https://github.com/dicode-ayo/dicode-core/commit/f458dd9c8d18c24169bb7c774358afed3c7fbb1d))
* **security:** passphrase bootstrap — DB storage, auto-gen, change API ([#15](https://github.com/dicode-ayo/dicode-core/issues/15)) ([a176639](https://github.com/dicode-ayo/dicode-core/commit/a176639c464c3055fd60f25ac5ef03daf1d70374))
* **security:** webhook optional auth + dicode.js 401 handling ([#17](https://github.com/dicode-ayo/dicode-core/issues/17)) ([3cdc30d](https://github.com/dicode-ayo/dicode-core/commit/3cdc30d0df94c7bdae38da24036ecc84323c89cc))
* simple task runs ([7143d2b](https://github.com/dicode-ayo/dicode-core/commit/7143d2b84f00abbe6432af97b007e7803552a02d))
* some fixes ([1b6ffe6](https://github.com/dicode-ayo/dicode-core/commit/1b6ffe64baefd34860844c993ed45dce838b654c))
* TaskSet architecture — hierarchical task composition with dev mode & MCP ([#3](https://github.com/dicode-ayo/dicode-core/issues/3)) ([33fd7f4](https://github.com/dicode-ayo/dicode-core/commit/33fd7f43664b5d177db88da417dc23e4cdbdb3b4))
* temp file cleanup via builtin task ([#91](https://github.com/dicode-ayo/dicode-core/issues/91)) ([ae8902b](https://github.com/dicode-ayo/dicode-core/commit/ae8902bd06ba1df4421f6a5e971cbb62800dad36))
* transparent relay proxy + comprehensive docs update ([#80](https://github.com/dicode-ayo/dicode-core/issues/80)) ([87559b5](https://github.com/dicode-ayo/dicode-core/commit/87559b507266e2b64442dd44bffed0935814a8cb))
* tray icon ([5c80c77](https://github.com/dicode-ayo/dicode-core/commit/5c80c7774c192be76174286d5c3c84d4bb1997fa))
* triggers edit ([5b5c215](https://github.com/dicode-ayo/dicode-core/commit/5b5c215afd5527aebd41a994ec7711c6d4612708))
* **ui:** settings ([2e49b6e](https://github.com/dicode-ayo/dicode-core/commit/2e49b6ef4742b602f710f1581cc11f8189cb5438))
* webhook return the result ([b5ee783](https://github.com/dicode-ayo/dicode-core/commit/b5ee78384bfcfe9ef913bc0dd75ad16125a0489b))
* webhook task UIs — serve index.html + dicode.js client SDK ([#9](https://github.com/dicode-ayo/dicode-core/issues/9)) ([7acf4fc](https://github.com/dicode-ayo/dicode-core/commit/7acf4fc3de81582cbdead697272fbb639a293d1f))
* **webui:** adopt dicode design system via theme.css ([#92](https://github.com/dicode-ayo/dicode-core/issues/92)) ([3499b60](https://github.com/dicode-ayo/dicode-core/commit/3499b60349d304dc0d972ec5996e85488fbb4832))
* **webui:** migrate SPA to standalone webhook task ([#22](https://github.com/dicode-ayo/dicode-core/issues/22)) ([126fa11](https://github.com/dicode-ayo/dicode-core/commit/126fa11448086fc5e5e00d9e23d9afe2f9890f98))


### Bug Fixes

* only cap SQLite connections to 1 for :memory: databases ([eb5a1b8](https://github.com/dicode-ayo/dicode-core/commit/eb5a1b84934fcb2ee63e6007a69aea8f8a55a42c))
* persist cron next-run time to detect missed jobs on restart ([#51](https://github.com/dicode-ayo/dicode-core/issues/51)) ([21b12a1](https://github.com/dicode-ayo/dicode-core/commit/21b12a19c49580a63e0652e5b3c5464259670666))
* trayicon exit ([3a9df66](https://github.com/dicode-ayo/dicode-core/commit/3a9df66632f5ab81a052430ce5b09c6f30198210))
* ui aftet taskset implementation ([dfa9f63](https://github.com/dicode-ayo/dicode-core/commit/dfa9f633629f941f39082f6466408ec27d7beeca))
* web ui a bit ([cbd5009](https://github.com/dicode-ayo/dicode-core/commit/cbd50095f3991ae7e21e1c37c828e80228517cc0))


### Performance Improvements

* replace WaitRun() polling loop with channel notification ([#73](https://github.com/dicode-ayo/dicode-core/issues/73)) ([057d884](https://github.com/dicode-ayo/dicode-core/commit/057d884adc36cfb9fe0fc0657c4185d5ea0a47b3))


### Documentation

* latest status ([6467285](https://github.com/dicode-ayo/dicode-core/commit/6467285a49bc8955c3ac5a946f4c352909a09345))
* move back ([4c5ebb4](https://github.com/dicode-ayo/dicode-core/commit/4c5ebb444e6f387288be36ca723d7706d4b3faa7))
* move pages ([22cad0e](https://github.com/dicode-ayo/dicode-core/commit/22cad0e2e54fe79698c4ddd9f0617117de2736cb))
* update implementation-plan with current milestone statuses ([6f275db](https://github.com/dicode-ayo/dicode-core/commit/6f275dbc953ba2145210ae1d912657287e8da9cb))
* update status ([fdecf3b](https://github.com/dicode-ayo/dicode-core/commit/fdecf3b1abda17a965a8dd8807ada981aadb3988))
* update taskset ([fab20ed](https://github.com/dicode-ayo/dicode-core/commit/fab20ed31467ec52cf690df5bc82c818fe1563b2))

## [0.0.2](https://github.com/dicode-ayo/dicode-core/compare/dicode-v0.0.1...dicode-v0.0.2) (2026-03-29)


### Features

* add github-stars example task ([3ed92e0](https://github.com/dicode-ayo/dicode-core/commit/3ed92e00c067ad2ce8e0abed1284cf02c4d663db))
* ai ([de07baf](https://github.com/dicode-ayo/dicode-core/commit/de07baf7b6c9ed9c793a0cf2fef61d9c3c0a6dfd))
* browser notifications, run events SSE, return value storage ([69a11d5](https://github.com/dicode-ayo/dicode-core/commit/69a11d586e1449a1f6ed9b674545b8ebe2c38290))
* docker executor ([fd7a01a](https://github.com/dicode-ayo/dicode-core/commit/fd7a01a13eb946d977a5dd12cc86941f88c338b7))
* doker engine ([6ce9b7a](https://github.com/dicode-ayo/dicode-core/commit/6ce9b7a47c526189f8bccebf80a0cf6dfb9bd06f))
* enhanced config ([238f983](https://github.com/dicode-ayo/dicode-core/commit/238f98339ecc9e9beff05aad0233788057d2cbd3))
* init commit ([1399d59](https://github.com/dicode-ayo/dicode-core/commit/1399d5957a3516f4cf42039dbc55409fa16b1b1e))
* persist and display structured output (output.html/text) ([51c1ff4](https://github.com/dicode-ayo/dicode-core/commit/51c1ff4eb44e7025b4c938ad2e4f8aad520360e6))
* Python socket-bridge runtime, Podman executor, Dockerfile builds, examples ([#1](https://github.com/dicode-ayo/dicode-core/issues/1)) ([33021da](https://github.com/dicode-ayo/dicode-core/commit/33021da6e2ec00a9adf2cca499f75fc804672be0))
* replace SSE+templates with WebSocket SPA architecture ([3cb1132](https://github.com/dicode-ayo/dicode-core/commit/3cb1132494e8543809e4b7a2d4c66035b68c156a))
* secrets ([042a26d](https://github.com/dicode-ayo/dicode-core/commit/042a26d032566ad357492063c169f686274c6542))
* simple task runs ([81cfec0](https://github.com/dicode-ayo/dicode-core/commit/81cfec07b6ad97eefddcd02f2191bfa27168b84e))
* some fixes ([ffdd1eb](https://github.com/dicode-ayo/dicode-core/commit/ffdd1eb77dff75acf169d0b5cb9636462dc21fdc))
* tray icon ([870ff2f](https://github.com/dicode-ayo/dicode-core/commit/870ff2fa93edabb473bc18ac3b91045d39cb3bbb))
* triggers edit ([d6a4c2d](https://github.com/dicode-ayo/dicode-core/commit/d6a4c2dbfe6f74e1cf7b103d8025daa41253e0d6))
* **ui:** settings ([0f7279c](https://github.com/dicode-ayo/dicode-core/commit/0f7279cc696d09dd04ab356e239f5d9a4f33f709))


### Bug Fixes

* only cap SQLite connections to 1 for :memory: databases ([eb1a182](https://github.com/dicode-ayo/dicode-core/commit/eb1a18288bf4a8024a416fbd560e7db7a2dbfe91))
* trayicon exit ([2f166b8](https://github.com/dicode-ayo/dicode-core/commit/2f166b86f1c123ec7d5cf1b665c8058ef6a78ee4))
* web ui a bit ([cebaebb](https://github.com/dicode-ayo/dicode-core/commit/cebaebbe2173c071b6e9a8b3b886f75e7042e0fe))


### Documentation

* latest status ([5918310](https://github.com/dicode-ayo/dicode-core/commit/591831013abe5aa385c3d5234eab2d1912378a8f))
* move back ([aa812e0](https://github.com/dicode-ayo/dicode-core/commit/aa812e0e1301350cb9bb2db153a4f08a595f023e))
* move pages ([4e49300](https://github.com/dicode-ayo/dicode-core/commit/4e49300d81621ed6045e2d9d62ee9357a59d20e7))
* update status ([bf8baa2](https://github.com/dicode-ayo/dicode-core/commit/bf8baa25baa58b95fe5f5d280f3cb6be774fa20e))
