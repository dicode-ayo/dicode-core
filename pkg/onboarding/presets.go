package onboarding

// TaskSetPreset describes one curated git-backed taskset that the first-run
// wizard offers. Each preset maps to a single entry under `spec.entries` in
// the generated dicode.yaml.
type TaskSetPreset struct {
	Name  string // unique namespace segment; used as the source's `name`
	Label string // shown in the UI
	Desc  string // one-line description for the UI
	URL   string // git URL
	// Branch tracks a moving head; Tag pins the preset to one release. Set
	// exactly one — a ref carrying both is a config-load error.
	Branch    string
	Tag       string
	EntryPath string // path within repo to taskset.yaml
	DefaultOn bool   // pre-checked in the wizard
}

// TaskSetPresets is the single edit-point for where each taskset lives.
// buildin has a repo of its own; examples and auth are still entry paths
// within dicode-core.
//
// The buildin entry's Name is load-bearing beyond the UI: task IDs are
// namespaced by it, and pkg/approval keys its auto-approve rule and
// dicode-core's own task references ("buildin/dicodai", …) off that exact
// string.
var TaskSetPresets = []TaskSetPreset{
	{
		Name:      "buildin",
		Label:     "Built-in tasks",
		Desc:      "Tray icon, notifications, web UI, dicodai chat, alert — the daemon's standard inventory.",
		URL:       "https://github.com/dicode-ayo/dicode-buildin",
		Branch:    "main",
		EntryPath: "taskset.yaml",
		DefaultOn: true,
	},
	{
		Name:      "examples",
		Label:     "Examples",
		Desc:      "Copy-friendly samples: hello-cron, github-stars, webhook-form, nginx-start, and more.",
		URL:       "https://github.com/dicode-ayo/dicode-core",
		Branch:    "main",
		EntryPath: "tasks/examples/taskset.yaml",
		DefaultOn: true,
	},
	{
		Name:      "auth",
		Label:     "OAuth providers",
		Desc:      "Zero-paste OAuth for Google, GitHub, Slack, OpenRouter, Spotify, and more.",
		URL:       "https://github.com/dicode-ayo/dicode-core",
		Branch:    "main",
		EntryPath: "tasks/auth/taskset.yaml",
		DefaultOn: true,
	},
}
