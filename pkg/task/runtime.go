package task

// NormalizeRuntime maps the legacy runtime aliases "" and "js" to RuntimeDeno
// and returns every other value unchanged. Matching is exact, mirroring the
// values Validate accepts as deno.
func NormalizeRuntime(r Runtime) Runtime {
	switch r {
	case "", "js":
		return RuntimeDeno
	}
	return r
}
