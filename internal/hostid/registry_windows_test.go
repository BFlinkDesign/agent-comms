//go:build windows

package hostid

import (
	"context"
	"os/exec"
	"strings"
	"testing"
)

// The in-process read must give exactly what reg.exe reports, or every machine
// would change identity, and be filed as a new host, when fleetd was upgraded.
func TestTheRegistryReadMatchesRegExe(t *testing.T) {
	guid, err := machineGUID()
	if err != nil {
		t.Fatalf("machineGUID: %v", err)
	}
	out, err := exec.Command("reg", "query", `HKLM\SOFTWARE\Microsoft\Cryptography`, "/v", "MachineGuid").Output()
	if err != nil {
		t.Fatalf("reg query: %v", err)
	}
	var want string
	for _, line := range strings.Split(string(out), "\n") {
		if f := strings.Fields(line); len(f) >= 3 && f[0] == "MachineGuid" {
			want = f[len(f)-1]
		}
	}
	if want == "" || guid != want {
		t.Fatalf("registry read %q, reg.exe reports %q", guid, want)
	}
}

// With no Runner, as in production, Derive reads the registry itself: the
// identity is stable, and the same one reg.exe's output derives.
func TestDeriveOnWindowsNeedsNoChildProcess(t *testing.T) {
	id := Derive(Options{Salt: "s"})
	if id.Source != "windows:MachineGuid" || !id.Stable {
		t.Fatalf("Source=%q Stable=%v, want windows:MachineGuid/true", id.Source, id.Stable)
	}
	viaReg := Derive(Options{Salt: "s", GOOS: "windows", Runner: func(_ context.Context, name string, args ...string) ([]byte, error) {
		return exec.Command(name, args...).Output()
	}})
	if viaReg.ID != id.ID {
		t.Fatalf("in-process identity %s differs from reg.exe's %s", id.ID, viaReg.ID)
	}
}
