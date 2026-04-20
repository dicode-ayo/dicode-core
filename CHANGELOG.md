# Changelog

## Unreleased

### ⚠ BREAKING CHANGES

- The direct-AI Go implementation has been removed. All AI functionality now runs through the `buildin/ai-agent` task and its presets.
  - Removed the SSE streaming endpoint `POST /api/tasks/{id}/ai/stream` (`pkg/webui/ai.go`).
  - Removed the embedded `pkg/agent/skill.md` package — moved to `tasks/skills/dicode-task-dev.md` so it can be loaded as an ai-agent skill.
  - Removed the `ai:` block from `dicode.yaml`. Extra AI config in your YAML is silently ignored; drop it on your next edit.
  - Removed the `dicode.get_config("ai")` IPC method and the `permissions.dicode.get_config` flag. Tasks needing provider config should declare their own params or env.
  - Removed the `/api/settings/ai` REST endpoint and the AI tab from the WebUI configuration page.

### Features

- Added the `buildin/dicodai` preset: an `ai-agent` override preloaded with the `dicode-task-dev` and `dicode-basics` skills and tuned for task development. Defaults to OpenAI's `gpt-4o`; only `OPENAI_API_KEY` is required.
- The task-detail AI chat panel now routes to `/hooks/ai/dicodai`, returning the agent's text reply. File edits are no longer written automatically — copy code back to the editor manually.
- Added `tasks/skills/dicode-task-dev.md` alongside `dicode-basics.md`; the ai-agent taskset discovers both via `${TASK_SET_DIR}/../skills` by default.

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
