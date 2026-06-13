package podman

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/dicode/dicode/pkg/task"
)

// envNameRe matches a sane environment variable name. Anything else —
// in particular names containing "=" — would let a task smuggle extra
// KEY=VALUE pairs through the single "-e", k+"="+v argv token.
var envNameRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// validateArgvSafety rejects task-controlled values that could corrupt the
// podman CLI invocation built by buildArgs (issue #380).
//
// exec.Command passes each slice element as one argv entry, so classic shell
// injection is impossible. The remaining hazards are targeted here:
//
//   - the image reference is the first positional argument after the flags;
//     a value starting with "-" would be parsed as a podman flag,
//   - NUL bytes make execve fail with a cryptic error (and signal tampering),
//   - CR/LF in single-token flag values (ports, volumes, hosts, caps,
//     security opts, network, workdir, user) have no legitimate use and
//     make injected values hard to spot in logs,
//   - env var names must be well-formed so "-e NAME=VALUE" stays one pair.
func validateArgvSafety(cfg *task.DockerConfig, imageRef string) error {
	if imageRef == "" {
		return fmt.Errorf("podman argv: image reference is empty")
	}
	if strings.HasPrefix(imageRef, "-") {
		return fmt.Errorf("podman argv: image reference %q starts with '-' and would be parsed as a flag", imageRef)
	}
	if strings.ContainsAny(imageRef, "\x00\n\r\t ") {
		return fmt.Errorf("podman argv: image reference %q contains whitespace or control characters", imageRef)
	}

	checkToken := func(field, v string) error {
		if strings.ContainsAny(v, "\x00\n\r") {
			return fmt.Errorf("podman argv: docker.%s value %q contains control characters", field, v)
		}
		return nil
	}
	for _, v := range cfg.Ports {
		if err := checkToken("ports", v); err != nil {
			return err
		}
	}
	for _, v := range cfg.Volumes {
		if err := checkToken("volumes", v); err != nil {
			return err
		}
	}
	for _, v := range cfg.ExtraHosts {
		if err := checkToken("extra_hosts", v); err != nil {
			return err
		}
	}
	for _, v := range cfg.CapAdd {
		if err := checkToken("cap_add", v); err != nil {
			return err
		}
	}
	for _, v := range cfg.CapDrop {
		if err := checkToken("cap_drop", v); err != nil {
			return err
		}
	}
	for _, v := range cfg.SecurityOpt {
		if err := checkToken("security_opt", v); err != nil {
			return err
		}
	}
	for _, fv := range []struct{ field, value string }{
		{"network_mode", cfg.NetworkMode},
		{"working_dir", cfg.WorkingDir},
		{"user", cfg.User},
	} {
		if err := checkToken(fv.field, fv.value); err != nil {
			return err
		}
	}

	for k, v := range cfg.EnvVars {
		if !envNameRe.MatchString(k) {
			return fmt.Errorf("podman argv: docker.env_vars key %q is not a valid environment variable name", k)
		}
		if strings.ContainsRune(v, '\x00') {
			return fmt.Errorf("podman argv: docker.env_vars[%s] value contains a NUL byte", k)
		}
	}

	// Command and entrypoint tokens land after the image reference, where
	// podman hands them to the container verbatim — only NUL is fatal.
	for _, v := range cfg.Command {
		if strings.ContainsRune(v, '\x00') {
			return fmt.Errorf("podman argv: docker.command token %q contains a NUL byte", v)
		}
	}
	for _, v := range cfg.Entrypoint {
		if strings.ContainsRune(v, '\x00') {
			return fmt.Errorf("podman argv: docker.entrypoint token %q contains a NUL byte", v)
		}
	}
	return nil
}
