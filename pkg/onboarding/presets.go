package onboarding

// TaskSetPreset describes one curated git-backed taskset that the first-run
// wizard offers. Each preset maps to a single entry under `sources:` in the
// generated dicode.yaml.
type TaskSetPreset struct {
	Name      string // unique namespace segment; used as the source's `name`
	Label     string // shown in the UI
	Desc      string // one-line description for the UI
	URL       string // git URL
	Branch    string
	EntryPath string // path within repo to taskset.yaml
	DefaultOn bool   // pre-checked in the wizard
}

// TaskSetPresets is the single edit-point when the three tasksets split into
// standalone repos. Today all three point at dicode-core with different
// entry paths.
var TaskSetPresets = []TaskSetPreset{
	{
		Name:      "buildin",
		Label:     "Built-in tasks",
		Desc:      "Tray icon, notifications, web UI, dicodai chat, alert — the daemon's standard inventory.",
		URL:       "https://github.com/dicode-ayo/dicode-core",
		Branch:    "main",
		EntryPath: "tasks/buildin/taskset.yaml",
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
