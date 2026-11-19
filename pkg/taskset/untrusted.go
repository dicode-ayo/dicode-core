package taskset

// SetUntrustedRoot makes future Resolve calls treat the root taskset.yaml as
// untrusted: its git entries' ref.auth.token_env is stripped exactly as for a
// nested sub-tree. Use it when the root TaskSet is not operator-owned config,
// e.g. a TaskSet named on the command line.
func (r *Resolver) SetUntrustedRoot(untrusted bool) {
	r.mu.Lock()
	r.untrustedRoot = untrusted
	r.mu.Unlock()
}

func (r *Resolver) isUntrustedRoot() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.untrustedRoot
}
