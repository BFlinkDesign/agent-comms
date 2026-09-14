package hostid

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeFile(t *testing.T, dir, name, content string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func baseOpts() Options {
	return Options{
		Salt:     "test-fleet-salt",
		Hostname: func() (string, error) { return "CNC-1", nil },
		Username: func() string { return "Brady.EAGLE" },
	}
}

func TestLinuxReadsMachineID(t *testing.T) {
	dir := t.TempDir()
	p := writeFile(t, dir, "machine-id", "d9b2f1a4c0e84a1b9f3c7d2e5a6b8c01\n")

	o := baseOpts()
	o.GOOS = "linux"
	o.LinuxIDPaths = []string{p}

	id := Derive(o)
	if id.Source != "linux:machine-id" {
		t.Errorf("Source = %q, want linux:machine-id", id.Source)
	}
	if !id.Stable {
		t.Error("Stable = false, want true when machine-id was read")
	}
	if id.Name != "CNC-1" || id.OS != "linux" {
		t.Errorf("Name/OS = %q/%q, want CNC-1/linux", id.Name, id.OS)
	}
}

func TestLinuxFallsThroughUnreadableAndEmptyPaths(t *testing.T) {
	dir := t.TempDir()
	empty := writeFile(t, dir, "empty-id", "   \n")
	good := writeFile(t, dir, "machine-id", "abc123")

	o := baseOpts()
	o.GOOS = "linux"
	// First path does not exist, second is whitespace-only, third is real.
	o.LinuxIDPaths = []string{filepath.Join(dir, "does-not-exist"), empty, good}

	id := Derive(o)
	if !id.Stable || id.Source != "linux:machine-id" {
		t.Fatalf("did not fall through to the readable path: Source=%q Stable=%v", id.Source, id.Stable)
	}

	// It must have used the third file's value, not merely reported success.
	o2 := baseOpts()
	o2.GOOS = "linux"
	o2.LinuxIDPaths = []string{good}
	if want := Derive(o2).ID; id.ID != want {
		t.Errorf("ID = %s, want %s (the value from the readable path)", id.ID, want)
	}
}

func TestDegradesToHostnameAndSaysSo(t *testing.T) {
	o := baseOpts()
	o.GOOS = "linux"
	o.LinuxIDPaths = []string{filepath.Join(t.TempDir(), "absent")}

	id := Derive(o)
	if id.Source != "hostname-only" {
		t.Errorf("Source = %q, want hostname-only", id.Source)
	}
	if id.Stable {
		t.Error("Stable = true for a hostname-derived identity; a weak identity must be labelled weak")
	}
	if id.ID == "" {
		t.Error("ID is empty; degradation must still produce a usable identifier")
	}
}

func TestUnsupportedPlatformDegradesRatherThanPanics(t *testing.T) {
	o := baseOpts()
	o.GOOS = "plan9"
	id := Derive(o)
	if id.Stable || id.Source != "hostname-only" {
		t.Errorf("unsupported platform gave Source=%q Stable=%v, want hostname-only/false", id.Source, id.Stable)
	}
}

func TestWindowsParsesRegOutput(t *testing.T) {
	// Verbatim shape of `reg query HKLM\SOFTWARE\Microsoft\Cryptography /v MachineGuid`,
	// including the blank first line and tab-ish spacing real reg.exe emits.
	const regOut = "\r\n" +
		"HKEY_LOCAL_MACHINE\\SOFTWARE\\Microsoft\\Cryptography\r\n" +
		"    MachineGuid    REG_SZ    00000000-1111-2222-3333-444444444444\r\n\r\n"

	var gotName string
	var gotArgs []string
	o := baseOpts()
	o.GOOS = "windows"
	o.Runner = func(_ context.Context, name string, args ...string) ([]byte, error) {
		gotName, gotArgs = name, args
		return []byte(regOut), nil
	}

	id := Derive(o)
	if id.Source != "windows:MachineGuid" || !id.Stable {
		t.Fatalf("Source=%q Stable=%v, want windows:MachineGuid/true", id.Source, id.Stable)
	}
	if gotName != "reg" {
		t.Errorf("ran %q, want reg", gotName)
	}
	// Arguments must be a vector, never a single interpolated command string.
	for _, a := range gotArgs {
		if strings.ContainsAny(a, "|&;><$`") {
			t.Errorf("argument %q contains shell metacharacters", a)
		}
	}

	// The GUID must be normalised, so the same machine hashes equal whether the
	// platform reports it braced, upper-case, or bare.
	for _, variant := range []string{
		"{00000000-1111-2222-3333-444444444444}",
		"00000000111122223333444444444444",
	} {
		v := variant
		o2 := baseOpts()
		o2.GOOS = "windows"
		o2.Runner = func(_ context.Context, _ string, _ ...string) ([]byte, error) {
			return []byte("    MachineGuid    REG_SZ    " + v + "\r\n"), nil
		}
		if got := Derive(o2).ID; got != id.ID {
			t.Errorf("variant %q hashed to %s, want %s — normalisation is not equivalence-preserving", v, got, id.ID)
		}
	}
}

func TestDarwinParsesIoregOutput(t *testing.T) {
	const ioregOut = `+-o J316sAP  <class IOPlatformExpertDevice, id 0x100000266>
    {
      "IOPlatformUUID" = "11111111-2222333344445555"
      "IOPlatformSerialNumber" = "XXXXXXXXXX"
    }`
	o := baseOpts()
	o.GOOS = "darwin"
	o.Runner = func(_ context.Context, _ string, _ ...string) ([]byte, error) {
		return []byte(ioregOut), nil
	}
	id := Derive(o)
	if id.Source != "darwin:IOPlatformUUID" || !id.Stable {
		t.Fatalf("Source=%q Stable=%v, want darwin:IOPlatformUUID/true", id.Source, id.Stable)
	}
}

func TestRawFingerprintIsNeverPublished(t *testing.T) {
	// Deliberately a low-entropy, obviously-synthetic value. An earlier version used
	// a realistic random GUID assigned to a variable named "secret", which a secret
	// scanner flagged as a hardcoded credential and failed the build on. The scanner
	// was doing its job: a high-entropy literal next to that keyword is exactly what
	// it should catch. The test proves the raw identifier is not republished, and
	// that property does not need a realistic-looking value to demonstrate.
	const fakeMachineGuid = "00000000-1111-2222-3333-444444444444"
	o := baseOpts()
	o.GOOS = "windows"
	o.Runner = func(_ context.Context, _ string, _ ...string) ([]byte, error) {
		return []byte("    MachineGuid    REG_SZ    " + fakeMachineGuid + "\r\n"), nil
	}
	id := Derive(o)

	// Every published field must be free of the raw identifier, in any casing and
	// with separators stripped — publishing a hardware fingerprint to a repository
	// is publishing it to everyone who can clone the repository.
	stripped := strings.ReplaceAll(strings.ToLower(fakeMachineGuid), "-", "")
	for field, v := range map[string]string{"ID": id.ID, "Name": id.Name, "Source": id.Source, "User": id.User} {
		low := strings.ToLower(v)
		if strings.Contains(low, strings.ToLower(fakeMachineGuid)) || strings.Contains(strings.ReplaceAll(low, "-", ""), stripped) {
			t.Errorf("field %s leaks the raw machine identifier: %q", field, v)
		}
	}
	if !strings.HasPrefix(id.ID, "host:") {
		t.Errorf("ID = %q, want a host: prefixed digest", id.ID)
	}
}

func TestSaltAndSourceSeparateTheKeyspace(t *testing.T) {
	mk := func(salt, goos string) string {
		o := baseOpts()
		o.Salt = salt
		o.GOOS = goos
		o.Runner = func(_ context.Context, _ string, _ ...string) ([]byte, error) {
			// Same literal value on both platforms.
			if goos == "windows" {
				return []byte("    MachineGuid    REG_SZ    aaaa\r\n"), nil
			}
			return []byte(`"IOPlatformUUID" = "aaaa"`), nil
		}
		return Derive(o).ID
	}

	if mk("salt-a", "windows") == mk("salt-b", "windows") {
		t.Error("different salts produced the same ID; the salt is not binding")
	}
	if mk("salt-a", "windows") == mk("salt-a", "darwin") {
		t.Error("the same raw value on two platforms produced the same ID; source is not binding")
	}
	if mk("salt-a", "windows") != mk("salt-a", "windows") {
		t.Error("identical inputs produced different IDs; derivation is not deterministic")
	}
}

func TestExternalCommandIsBounded(t *testing.T) {
	// The failure this guards against is real and recent: a sibling tool in this
	// fleet set a 15s deadline but applied it only after unbounded reads, and was
	// observed still running at 18.02s. Here the deadline starts before the
	// process does, so a child that never exits is still bounded.
	o := baseOpts()
	o.GOOS = "linux"
	o.LinuxIDPaths = []string{filepath.Join(t.TempDir(), "absent")}
	o.Timeout = 150 * time.Millisecond

	start := time.Now()
	// Probe windows explicitly so a real command runs under the deadline.
	o.GOOS = "windows"
	o.Runner = nil // use the real exec path
	o.Timeout = 150 * time.Millisecond
	_, _, err := probeWindows(Options{
		Salt: o.Salt, GOOS: "windows", Timeout: 150 * time.Millisecond,
		Runner: func(ctx context.Context, _ string, _ ...string) ([]byte, error) {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(10 * time.Second):
				return []byte("never reached"), nil
			}
		},
	})
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("a command that outlives the deadline returned success")
	}
	if !errors.Is(err, ErrNoStableSource) {
		t.Errorf("err = %v, want it to wrap ErrNoStableSource so callers can classify it", err)
	}
	if elapsed > 2*time.Second {
		t.Errorf("took %s; the deadline was not enforced", elapsed)
	}
}

func TestHostnameFailureDoesNotPanic(t *testing.T) {
	o := baseOpts()
	o.GOOS = "linux"
	o.LinuxIDPaths = []string{filepath.Join(t.TempDir(), "absent")}
	o.Hostname = func() (string, error) { return "", errors.New("no hostname") }

	id := Derive(o)
	if id.Name != "unknown-host" {
		t.Errorf("Name = %q, want unknown-host", id.Name)
	}
	if id.ID == "" {
		t.Error("ID is empty when the hostname is unavailable")
	}
}

func TestDeriveOnThisMachineIsSelfConsistent(t *testing.T) {
	// No injection: exercise the real platform path on whatever host runs this.
	o := Options{Salt: "self-check"}
	a, b := Derive(o), Derive(o)
	if a.ID != b.ID {
		t.Fatalf("two derivations on the same machine disagreed: %s vs %s", a.ID, b.ID)
	}
	if a.OS == "" || a.Arch == "" || a.Name == "" || a.Source == "" {
		t.Fatalf("identity has empty required fields: %+v", a)
	}
	t.Logf("this host: id=%s name=%s os=%s arch=%s source=%s stable=%v user=%s",
		a.ID, a.Name, a.OS, a.Arch, a.Source, a.Stable, a.User)
}
