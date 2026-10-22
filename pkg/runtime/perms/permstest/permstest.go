// Package permstest holds the spec table that every permission renderer is
// tested against, so each asserts the same effective grants.
package permstest

import "github.com/dicode/dicode/pkg/task"

// TaskDir is the task directory every Case spec uses.
const TaskDir = "/srv/tasks/p"

// Case is one declared permission set and the grants every renderer must
// emit for it.
type Case struct {
	Name string
	Perm task.Permissions

	NetUnrestricted bool
	NetHosts        []string // allowlist; empty with !NetUnrestricted means deny
	RunAllow        bool
	RunCommands     []string // allowlist; empty with !RunAllow means deny
	WritePaths      []string // canonical fs write grants, excluding IPC
}

// Spec builds the task spec for the case.
func (c Case) Spec() *task.Spec {
	return &task.Spec{ID: "t/p", TaskDir: TaskDir, Permissions: c.Perm}
}

// Cases is the shared table.
var Cases = []Case{
	{Name: "zero value"},
	{
		Name: "wildcards", Perm: task.Permissions{Net: []string{"*"}, Run: []string{"*"}},
		NetUnrestricted: true, RunAllow: true,
	},
	{
		Name:     "allowlists",
		Perm:     task.Permissions{Net: []string{"a.example", "b.example:443"}, Run: []string{"git", "ls"}},
		NetHosts: []string{"a.example", "b.example:443"}, RunCommands: []string{"git", "ls"},
	},
	{
		Name: "fs traversal is cleaned",
		Perm: task.Permissions{FS: []task.FSEntry{
			{Path: "../../etc", Permission: "w"},
			{Path: "d//x/", Permission: "rw"},
			{Path: "ro", Permission: "r"},
			{Path: "/abs/./p", Permission: "w"},
		}},
		WritePaths: []string{"/srv/etc", "/srv/tasks/p/d/x", "/abs/p"},
	},
}
