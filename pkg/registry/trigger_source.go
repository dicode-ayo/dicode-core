package registry

// TriggerSource is the typed enum identifying how a run was started.
// JSON wire value is the underlying string (preserves API compatibility
// with the prior string-typed Run.TriggerSource field). DB scan paths
// use a string intermediary and cast on assignment to avoid driver
// type-assertion failures.
type TriggerSource string

const (
	TriggerUnknown     TriggerSource = ""
	TriggerWebhook     TriggerSource = "webhook"
	TriggerCron        TriggerSource = "cron"
	TriggerManual      TriggerSource = "manual"
	TriggerChain       TriggerSource = "chain"
	TriggerDaemon      TriggerSource = "daemon"
	TriggerReplay      TriggerSource = "replay"
	TriggerCronCatchup TriggerSource = "cron-catchup"
	TriggerProvider    TriggerSource = "provider"
	TriggerIfMissing   TriggerSource = "if_missing"
	// TriggerPreflight tagged runs fired by the now-removed daemon-preflight
	// gating mechanism (`trigger.before`, removed in PR6). No code path emits
	// it anymore — it is retained intentionally because existing DB `runs`
	// rows may carry trigger_source="preflight"; removing the const would
	// break scanning those historical rows. Superseded by
	// TriggerPipelineStage for new runs.
	TriggerPreflight TriggerSource = "preflight"
	// TriggerPipelineStage marks a run fired as a stage of a kind: PipelineTask.
	// Distinct from TriggerPreflight so the WebUI can group pipeline stage runs
	// under their parent pipeline run. Supersedes TriggerPreflight, which is
	// kept only to scan historical "preflight" rows (trigger.before removed
	// in PR6).
	TriggerPipelineStage TriggerSource = "pipeline-stage"
)
