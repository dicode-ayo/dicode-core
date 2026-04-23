package taskset

// SetPullStatusForTest seeds a Source's pull status without running a
// live git pull. Exported for cross-package tests (e.g. pkg/webui) that
// need to exercise code paths consuming PullStatus() without standing
// up a bare repo fixture. Not for production use.
func (s *Source) SetPullStatusForTest(err error) {
	s.recordPull(err)
}
