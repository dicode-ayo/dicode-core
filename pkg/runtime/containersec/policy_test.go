package containersec

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dicode/dicode/pkg/task"
)

func TestValidate_NilConfig(t *testing.T) {
	if err := Validate(nil, Policy{}); err != nil {
		t.Fatalf("nil config must validate, got %v", err)
	}
}

// TestValidate_DefaultDeny pins the security floor: every dangerous config
// is rejected by the zero-value (default) policy.
func TestValidate_DefaultDeny(t *testing.T) {
	tests := []struct {
		name    string
		cfg     *task.DockerConfig
		wantSub string // substring expected in the error
	}{
		{
			name:    "host network",
			cfg:     &task.DockerConfig{NetworkMode: "host"},
			wantSub: "network_mode",
		},
		{
			name:    "host network case-insensitive",
			cfg:     &task.DockerConfig{NetworkMode: "HOST"},
			wantSub: "network_mode",
		},
		{
			name:    "container network namespace",
			cfg:     &task.DockerConfig{NetworkMode: "container:abc"},
			wantSub: "network_mode",
		},
		{
			name:    "ns path network namespace",
			cfg:     &task.DockerConfig{NetworkMode: "ns:/proc/1/ns/net"},
			wantSub: "network_mode",
		},
		{
			name:    "cap_add SYS_ADMIN",
			cfg:     &task.DockerConfig{CapAdd: []string{"SYS_ADMIN"}},
			wantSub: "cap_add",
		},
		{
			name:    "cap_add CAP_ prefixed lowercase",
			cfg:     &task.DockerConfig{CapAdd: []string{"cap_sys_admin"}},
			wantSub: "cap_add",
		},
		{
			name:    "cap_add SYS_PTRACE",
			cfg:     &task.DockerConfig{CapAdd: []string{"SYS_PTRACE"}},
			wantSub: "cap_add",
		},
		{
			name:    "cap_add NET_ADMIN",
			cfg:     &task.DockerConfig{CapAdd: []string{"NET_ADMIN"}},
			wantSub: "cap_add",
		},
		{
			name:    "cap_add ALL",
			cfg:     &task.DockerConfig{CapAdd: []string{"ALL"}},
			wantSub: "cap_add",
		},
		{
			name:    "seccomp unconfined",
			cfg:     &task.DockerConfig{SecurityOpt: []string{"seccomp=unconfined"}},
			wantSub: "security_opt",
		},
		{
			name:    "seccomp unconfined legacy colon separator",
			cfg:     &task.DockerConfig{SecurityOpt: []string{"seccomp:unconfined"}},
			wantSub: "security_opt",
		},
		{
			name:    "apparmor unconfined",
			cfg:     &task.DockerConfig{SecurityOpt: []string{"apparmor=unconfined"}},
			wantSub: "security_opt",
		},
		{
			name:    "selinux label disable",
			cfg:     &task.DockerConfig{SecurityOpt: []string{"label=disable"}},
			wantSub: "security_opt",
		},
		{
			name:    "systempaths unconfined",
			cfg:     &task.DockerConfig{SecurityOpt: []string{"systempaths=unconfined"}},
			wantSub: "security_opt",
		},
		{
			name:    "podman unmask",
			cfg:     &task.DockerConfig{SecurityOpt: []string{"unmask=ALL"}},
			wantSub: "security_opt",
		},
		{
			name:    "bind mount host root",
			cfg:     &task.DockerConfig{Volumes: []string{"/:/host"}},
			wantSub: "volumes",
		},
		{
			name:    "bind mount docker socket",
			cfg:     &task.DockerConfig{Volumes: []string{"/var/run/docker.sock:/var/run/docker.sock"}},
			wantSub: "volumes",
		},
		{
			name:    "bind mount podman socket",
			cfg:     &task.DockerConfig{Volumes: []string{"/run/podman/podman.sock:/sock"}},
			wantSub: "volumes",
		},
		{
			name:    "bind mount rootless podman socket",
			cfg:     &task.DockerConfig{Volumes: []string{"/run/user/1000/podman/podman.sock:/sock"}},
			wantSub: "volumes",
		},
		{
			name:    "bind mount relocated docker socket by name",
			cfg:     &task.DockerConfig{Volumes: []string{"/srv/sockets/docker.sock:/sock"}},
			wantSub: "control socket",
		},
		{
			name:    "bind mount /proc",
			cfg:     &task.DockerConfig{Volumes: []string{"/proc:/host-proc"}},
			wantSub: "volumes",
		},
		{
			name:    "bind mount /sys",
			cfg:     &task.DockerConfig{Volumes: []string{"/sys:/host-sys"}},
			wantSub: "volumes",
		},
		{
			name:    "bind mount /etc",
			cfg:     &task.DockerConfig{Volumes: []string{"/etc:/host-etc:ro"}},
			wantSub: "volumes",
		},
		{
			name:    "bind mount under /etc",
			cfg:     &task.DockerConfig{Volumes: []string{"/etc/shadow:/x:ro"}},
			wantSub: "volumes",
		},
		{
			name:    "bind mount docker state dir",
			cfg:     &task.DockerConfig{Volumes: []string{"/var/lib/docker:/d"}},
			wantSub: "volumes",
		},
		{
			name:    "traversal escapes into /etc",
			cfg:     &task.DockerConfig{Volumes: []string{"/srv/data/../../etc:/x"}},
			wantSub: "volumes",
		},
		{
			name:    "traversal to host root",
			cfg:     &task.DockerConfig{Volumes: []string{"/srv/..:/x"}},
			wantSub: "volumes",
		},
		{
			name:    "relative bind mount source",
			cfg:     &task.DockerConfig{Volumes: []string{"../secrets:/x"}},
			wantSub: "relative",
		},
		{
			name: "multiple violations all reported",
			cfg: &task.DockerConfig{
				NetworkMode: "host",
				CapAdd:      []string{"SYS_ADMIN"},
				Volumes:     []string{"/:/host"},
			},
			wantSub: "network_mode",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := Validate(tt.cfg, Policy{})
			if err == nil {
				t.Fatalf("expected rejection, got nil error")
			}
			if !strings.Contains(err.Error(), tt.wantSub) {
				t.Errorf("error %q does not mention %q", err.Error(), tt.wantSub)
			}
		})
	}
}

// TestValidate_OperatorOptIn pins that each dangerous config passes when the
// operator explicitly allows it.
func TestValidate_OperatorOptIn(t *testing.T) {
	tests := []struct {
		name   string
		cfg    *task.DockerConfig
		policy Policy
	}{
		{
			name:   "host network allowed",
			cfg:    &task.DockerConfig{NetworkMode: "host"},
			policy: Policy{AllowHostNetwork: true},
		},
		{
			name:   "cap_add allowed by list",
			cfg:    &task.DockerConfig{CapAdd: []string{"SYS_PTRACE"}},
			policy: Policy{AllowedCapAdd: []string{"sys_ptrace"}},
		},
		{
			name:   "cap_add allowed with CAP_ prefix in policy",
			cfg:    &task.DockerConfig{CapAdd: []string{"NET_ADMIN"}},
			policy: Policy{AllowedCapAdd: []string{"CAP_NET_ADMIN"}},
		},
		{
			name:   "cap_add ALL allowed when policy allows ALL",
			cfg:    &task.DockerConfig{CapAdd: []string{"ALL", "SYS_ADMIN"}},
			policy: Policy{AllowedCapAdd: []string{"ALL"}},
		},
		{
			name:   "insecure security_opt allowed",
			cfg:    &task.DockerConfig{SecurityOpt: []string{"seccomp=unconfined"}},
			policy: Policy{AllowInsecureSecurityOpt: true},
		},
		{
			name:   "bind mount inside allowed root",
			cfg:    &task.DockerConfig{Volumes: []string{"/srv/shared/data:/data"}},
			policy: Policy{AllowedVolumeRoots: []string{"/srv/shared"}},
		},
		{
			name:   "allowed root overrides built-in denylist",
			cfg:    &task.DockerConfig{Volumes: []string{"/etc/myapp:/cfg:ro"}},
			policy: Policy{AllowedVolumeRoots: []string{"/etc/myapp"}},
		},
		{
			name:   "allowed root slash allows everything",
			cfg:    &task.DockerConfig{Volumes: []string{"/var/lib/docker:/d"}},
			policy: Policy{AllowedVolumeRoots: []string{"/"}},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := Validate(tt.cfg, tt.policy); err != nil {
				t.Errorf("expected pass with opt-in, got %v", err)
			}
		})
	}
}

// TestValidate_PartialOptInStillDeniesOthers ensures opting in to one escape
// does not loosen the rest of the floor.
func TestValidate_PartialOptInStillDeniesOthers(t *testing.T) {
	cfg := &task.DockerConfig{
		NetworkMode: "host",
		CapAdd:      []string{"SYS_ADMIN"},
	}
	err := Validate(cfg, Policy{AllowHostNetwork: true})
	if err == nil {
		t.Fatalf("cap_add SYS_ADMIN must still be rejected when only host network is allowed")
	}
	if strings.Contains(err.Error(), "network_mode") {
		t.Errorf("allowed host network must not appear as a violation: %v", err)
	}
	if !strings.Contains(err.Error(), "cap_add") {
		t.Errorf("expected cap_add violation in %v", err)
	}
}

// TestValidate_SafeConfigsPass pins that ordinary configs are untouched by
// the floor.
func TestValidate_SafeConfigsPass(t *testing.T) {
	tests := []struct {
		name string
		cfg  *task.DockerConfig
	}{
		{name: "empty config", cfg: &task.DockerConfig{Image: "alpine"}},
		{name: "bridge network", cfg: &task.DockerConfig{NetworkMode: "bridge"}},
		{name: "none network", cfg: &task.DockerConfig{NetworkMode: "none"}},
		{name: "custom network", cfg: &task.DockerConfig{NetworkMode: "my-net"}},
		{name: "benign cap_add", cfg: &task.DockerConfig{CapAdd: []string{"NET_BIND_SERVICE", "CHOWN"}}},
		{name: "cap_drop ALL", cfg: &task.DockerConfig{CapDrop: []string{"ALL"}}},
		{name: "hardening security_opt", cfg: &task.DockerConfig{SecurityOpt: []string{"no-new-privileges:true"}}},
		{name: "named volume", cfg: &task.DockerConfig{Volumes: []string{"mydata:/data"}}},
		{name: "named volume with opts", cfg: &task.DockerConfig{Volumes: []string{"my_vol.2:/data:ro"}}},
		{name: "anonymous volume", cfg: &task.DockerConfig{Volumes: []string{"/data"}}},
		{name: "benign bind mount", cfg: &task.DockerConfig{Volumes: []string{"/srv/app-data:/data:ro"}}},
		{name: "read only and user", cfg: &task.DockerConfig{ReadOnly: true, User: "65532:65532"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := Validate(tt.cfg, Policy{}); err != nil {
				t.Errorf("safe config rejected: %v", err)
			}
		})
	}
}

// TestValidate_SymlinkEscape ensures a symlink inside an allowed root that
// points at a sensitive path is resolved before the allow-root check.
func TestValidate_SymlinkEscape(t *testing.T) {
	root := t.TempDir()
	link := filepath.Join(root, "etc-link")
	if err := os.Symlink("/etc", link); err != nil {
		t.Skipf("cannot create symlink: %v", err)
	}

	cfg := &task.DockerConfig{Volumes: []string{link + ":/x"}}

	// Allowlist mode: the symlink resolves to /etc, outside the allowed root.
	err := Validate(cfg, Policy{AllowedVolumeRoots: []string{root}})
	if err == nil {
		t.Fatalf("symlink escaping the allowed root must be rejected")
	}

	// Denylist mode: the symlink resolves to /etc, which is sensitive.
	err = Validate(cfg, Policy{})
	if err == nil {
		t.Fatalf("symlink pointing at /etc must be rejected by the default denylist")
	}
}

// TestValidate_TraversalOutsideAllowedRoot covers ".." escapes when
// allowed_volume_roots is configured.
func TestValidate_TraversalOutsideAllowedRoot(t *testing.T) {
	cfg := &task.DockerConfig{Volumes: []string{"/srv/shared/../../etc/shadow:/x"}}
	err := Validate(cfg, Policy{AllowedVolumeRoots: []string{"/srv/shared"}})
	if err == nil {
		t.Fatalf("traversal escaping the allowed root must be rejected")
	}

	// A sibling directory that merely shares the root as a string prefix
	// must not pass ("/srv/shared-evil" is not within "/srv/shared").
	cfg = &task.DockerConfig{Volumes: []string{"/srv/shared-evil/data:/x"}}
	if err := Validate(cfg, Policy{AllowedVolumeRoots: []string{"/srv/shared"}}); err == nil {
		t.Fatalf("string-prefix sibling of an allowed root must be rejected")
	}
}

func TestNormalizeCap(t *testing.T) {
	tests := []struct{ in, want string }{
		{"SYS_ADMIN", "SYS_ADMIN"},
		{"sys_admin", "SYS_ADMIN"},
		{"CAP_SYS_ADMIN", "SYS_ADMIN"},
		{"cap_net_admin", "NET_ADMIN"},
		{" all ", "ALL"},
	}
	for _, tt := range tests {
		if got := NormalizeCap(tt.in); got != tt.want {
			t.Errorf("NormalizeCap(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}
