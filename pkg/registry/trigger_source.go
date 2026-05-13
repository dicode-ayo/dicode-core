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
	// TriggerPreflight tags a run fired by the daemon-preflight gating
	// mechanism (`trigger.before`). Distinct from TriggerChain so the
	// WebUI can group preflight runs under the daemon they're gating
	// rather than under their own chain-trigger history.
	TriggerPreflight TriggerSource = "preflight"
)
