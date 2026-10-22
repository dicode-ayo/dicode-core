package deno

import (
	"reflect"
	"strings"
	"testing"

	"github.com/dicode/dicode/pkg/runtime/perms/permstest"
)

// flag returns the comma-split value of --<name>=..., and whether the flag
// was present bare or valued.
func flag(args []string, name string) (vals []string, present bool) {
	for _, a := range args {
		if a == "--"+name {
			return nil, true
		}
		if v, ok := strings.CutPrefix(a, "--"+name+"="); ok {
			return strings.Split(v, ","), true
		}
	}
	return nil, false
}

func TestRenderersAgree_Deno(t *testing.T) {
	const ipc = "127.0.0.1:1"
	for _, c := range permstest.Cases {
		t.Run(c.Name, func(t *testing.T) {
			args := buildDenoArgs(c.Spec(), ipc, "/shim.ts", "/runner.ts", nil, "2.3.3")

			net, present := flag(args, "allow-net")
			switch {
			case c.NetUnrestricted:
				if !present || net != nil {
					t.Errorf("want bare --allow-net, got %v", net)
				}
			default:
				want := append([]string{ipc}, c.NetHosts...)
				if !reflect.DeepEqual(net, want) {
					t.Errorf("allow-net = %v, want %v", net, want)
				}
			}

			run, present := flag(args, "allow-run")
			switch {
			case c.RunAllow:
				if !present || run != nil {
					t.Errorf("want bare --allow-run, got %v", run)
				}
			case len(c.RunCommands) == 0:
				if present {
					t.Errorf("unexpected --allow-run %v", run)
				}
			case !reflect.DeepEqual(run, c.RunCommands):
				t.Errorf("allow-run = %v, want %v", run, c.RunCommands)
			}

			w, _ := flag(args, "allow-write")
			if len(c.WritePaths) == 0 {
				c.WritePaths = nil
			}
			if !reflect.DeepEqual(w, c.WritePaths) {
				t.Errorf("allow-write = %v, want %v", w, c.WritePaths)
			}
		})
	}
}
