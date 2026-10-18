package hosts

import (
	"os/exec"
	"strings"
	"testing"
)

// runSh runs a script with /bin/sh -s and HOME set, as a host would.
func runSh(t *testing.T, home, script string) ([]byte, error) {
	t.Helper()
	cmd := exec.Command("/bin/sh", "-s")
	cmd.Env = []string{"HOME=" + home, "PATH=/usr/bin:/bin", "SHELL=/bin/sh", "SSH_CONNECTION=100.64.0.1 5000 100.64.0.7 22"}
	cmd.Stdin = strings.NewReader(script)
	return cmd.CombinedOutput()
}
