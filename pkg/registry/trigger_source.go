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
	// TriggerResume marks the continuation run spawned when a suspended run is
	// resumed (#95). Its parent_run_id points at the original suspended run.
	TriggerResume TriggerSource = "resume"
	// TriggerPipelineStage marks a run fired as a stage of a kind: PipelineTask.
	// The WebUI groups pipeline stage runs under their parent pipeline run.
	TriggerPipelineStage TriggerSource = "pipeline-stage"
)
