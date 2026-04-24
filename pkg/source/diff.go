package source

// DiffSnapshots compares two keyed snapshots and returns the keys that were
// added, updated, or removed. The hashOf function extracts a comparable hash
// from each value — callers pass an identity function when V is already a
// comparable scalar, or a field accessor when V is a struct and only one
// field defines equality (e.g. a content hash).
//
// Used by pkg/source/git, pkg/source/local, and pkg/taskset/source to avoid
// three hand-rolled copies of the same "build map, compare, emit events"
// pattern.
func DiffSnapshots[K comparable, V any](prev, cur map[K]V, hashOf func(V) string) (added, updated, removed []K) {
	for k, v := range cur {
		pv, ok := prev[k]
		if !ok {
			added = append(added, k)
			continue
		}
		if hashOf(pv) != hashOf(v) {
			updated = append(updated, k)
		}
	}
	for k := range prev {
		if _, ok := cur[k]; !ok {
			removed = append(removed, k)
		}
	}
	return
}
