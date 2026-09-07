//go:build integration

package integration

import (
	"os/exec"
	"testing"
)

// TestRestartSurvival_DisposableUnit verifies the host capability required for
// the lifecycle check. Docker test containers normally have no user manager;
// that is an explicit skip rather than a false pass. make restart-survival-host
// performs the disposable user-unit check on a manager-capable host.
func TestRestartSurvival_DisposableUnit(t *testing.T) {
	if _, err := exec.LookPath("systemd-run"); err != nil {
		t.Skip("systemd-run unavailable in integration container; use make restart-survival-host on a user-manager host")
	}
	if err := exec.Command("systemctl", "--user", "show-environment").Run(); err != nil {
		t.Skip("no reachable systemd user manager in integration container; use make restart-survival-host")
	}
	t.Skip("restart survival is exercised by make restart-survival-host against a disposable user unit")
}
