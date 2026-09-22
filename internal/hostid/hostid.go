// Package hostid derives a stable identity for the machine a record was written
// on.
//
// This exists because git records no such thing. A commit carries an author, a
// committer and a timezone offset, and none of them identify a host: one person
// routinely commits under several identities, and the UTC offset tracks daylight
// saving and the location of whoever happened to run the command. Measuring the
// history of this fleet found ten distinct commit identities for one person and
// no signal at all that separated one workstation from another. So "which machine
// did I do that on" is not a question git was ever able to answer, and the only
// fix is to record it at the time, deliberately.
//
// Two principles govern what this package emits:
//
//   - The raw platform identifier is never published. On every supported OS the
//     stable identifier is a hardware or install fingerprint, and a fingerprint
//     that lands in a repository is a fingerprint that has left the machine. What
//     is published is a salted digest of it, which is stable across reboots and
//     useless to anyone without the salt.
//
//   - A weak identity is labelled weak. When no stable source is readable, Derive
//     degrades to the hostname and says so in Source, and Stable reports false.
//     Presenting a hostname-derived identity as though it were hardware-backed
//     would launder a guess into a fact, which is worse than having no identity.
package hostid

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"runtime"
	"strings"
	"time"
)

// DefaultTimeout bounds every external command this package runs. Nothing here
// is allowed to block start-up indefinitely: the deadline is applied before the
// process starts, not after its output has been read.
const DefaultTimeout = 3 * time.Second

// Identity is what a host publishes about itself.
type Identity struct {
	// ID is the salted digest of the platform's stable machine identifier,
	// prefixed "host:". Stable across reboots; reveals nothing about the machine.
	ID string
	// Name is the hostname, published in clear because distinguishing CNC-1 from
	// BRADY-HPREMOTE by eye is the entire point of recording this.
	Name string
	// OS and Arch are runtime.GOOS and runtime.GOARCH.
	OS, Arch string
	// User is the OS account the record was written under, "" if undeterminable.
	User string
	// Source names where ID came from, so a reader can judge how much it is
	// worth: "linux:machine-id", "windows:MachineGuid", "darwin:IOPlatformUUID",
	// or "hostname-only" when no stable source was readable.
	Source string
	// Stable is false when Source is "hostname-only". A false value means this
	// identity will change if the machine is renamed, and may collide with any
	// other machine of the same name.
	Stable bool
}

// Options configure Derive. The zero value is correct for production; the fields
// exist so the platform-specific paths and commands can be exercised on a host
// that is not that platform.
type Options struct {
	// Salt separates this fleet's digests from anyone else's. It is not a secret
	// in the cryptographic sense and does not need protecting like one, but a
	// shared salt across machines is required or the same machine would hash
	// differently per repository.
	Salt string
	// GOOS overrides the detected operating system.
	GOOS string
	// LinuxIDPaths overrides the files consulted on Linux, in order.
	LinuxIDPaths []string
	// Runner overrides external command execution.
	Runner func(ctx context.Context, name string, args ...string) ([]byte, error)
	// Hostname overrides hostname lookup.
	Hostname func() (string, error)
	// Username overrides OS account lookup.
	Username func() string
	// Timeout bounds external commands; DefaultTimeout when zero.
	Timeout time.Duration
}

// ErrNoStableSource reports that no platform identifier could be read. Derive
// does not return it — it degrades and labels the result — but the probe helpers
// return it so a caller that needs a hard failure can have one.
var ErrNoStableSource = errors.New("hostid: no stable machine identifier available")

var defaultLinuxIDPaths = []string{"/etc/machine-id", "/var/lib/dbus/machine-id"}

func (o Options) goos() string {
	if o.GOOS != "" {
		return o.GOOS
	}
	return runtime.GOOS
}

func (o Options) timeout() time.Duration {
	if o.Timeout > 0 {
		return o.Timeout
	}
	return DefaultTimeout
}

func (o Options) run(name string, args ...string) ([]byte, error) {
	// The deadline starts here, before the process does, so that a child which
	// never writes and never exits is still bounded. CommandContext kills the
	// process when the context expires rather than waiting on a read that may
	// never return.
	ctx, cancel := context.WithTimeout(context.Background(), o.timeout())
	defer cancel()
	if o.Runner != nil {
		return o.Runner(ctx, name, args...)
	}
	// Arguments are passed as a vector. Nothing here is interpolated into a
	// shell, so a hostile value in the environment cannot become a command.
	out, err := exec.CommandContext(ctx, name, args...).Output()
	if ctxErr := ctx.Err(); ctxErr != nil {
		return nil, fmt.Errorf("hostid: %s timed out after %s: %w", name, o.timeout(), ctxErr)
	}
	return out, err
}

func (o Options) hostname() string {
	f := o.Hostname
	if f == nil {
		f = os.Hostname
	}
	h, err := f()
	if err != nil || strings.TrimSpace(h) == "" {
		return "unknown-host"
	}
	return strings.TrimSpace(h)
}

func (o Options) username() string {
	if o.Username != nil {
		return o.Username()
	}
	u, err := user.Current()
	if err != nil {
		return ""
	}
	return u.Username
}

// Derive returns this machine's identity. It never fails: when no stable source
// can be read it degrades to the hostname and records that in Source, because a
// record that says "I do not know which machine this was" is useful and a record
// that silently guesses is not.
func Derive(opt Options) Identity {
	name := opt.hostname()
	id := Identity{
		Name: name,
		OS:   opt.goos(),
		Arch: runtime.GOARCH,
		User: opt.username(),
	}

	raw, source, err := probe(opt)
	if err != nil || strings.TrimSpace(raw) == "" {
		id.Source = "hostname-only"
		id.Stable = false
		id.ID = digest(opt.Salt, "hostname-only", strings.ToLower(name))
		return id
	}
	id.Source = source
	id.Stable = true
	// Normalising case and separators means a platform that reports the same
	// identifier in a different shape after an OS upgrade still hashes equal.
	norm := strings.ToLower(strings.NewReplacer("-", "", "{", "", "}", "", " ", "").Replace(strings.TrimSpace(raw)))
	id.ID = digest(opt.Salt, source, norm)
	return id
}

// digest binds the salt, the source and the value together, so that an identifier
// which happens to be numerically equal on two platforms still yields different
// IDs, and so that the published value cannot be reversed to the raw fingerprint.
func digest(salt, source, value string) string {
	sum := sha256.Sum256([]byte(salt + "\x00" + source + "\x00" + value))
	return "host:" + hex.EncodeToString(sum[:])[:16]
}

func probe(opt Options) (raw, source string, err error) {
	switch opt.goos() {
	case "linux":
		return probeLinux(opt)
	case "windows":
		return probeWindows(opt)
	case "darwin":
		return probeDarwin(opt)
	default:
		return "", "", fmt.Errorf("%w: unsupported platform %q", ErrNoStableSource, opt.goos())
	}
}

func probeLinux(opt Options) (string, string, error) {
	paths := opt.LinuxIDPaths
	if paths == nil {
		paths = defaultLinuxIDPaths
	}
	for _, p := range paths {
		b, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		if v := strings.TrimSpace(string(b)); v != "" {
			return v, "linux:machine-id", nil
		}
	}
	return "", "", fmt.Errorf("%w: none of %v were readable and non-empty", ErrNoStableSource, paths)
}

func probeWindows(opt Options) (string, string, error) {
	// reg.exe is used rather than a registry binding so that the collector stays
	// dependency-free and cross-compiles from any host. Output looks like:
	//     HKEY_LOCAL_MACHINE\SOFTWARE\Microsoft\Cryptography
	//         MachineGuid    REG_SZ    4f2a...-...
	out, err := opt.run("reg", "query",
		`HKLM\SOFTWARE\Microsoft\Cryptography`, "/v", "MachineGuid")
	if err != nil {
		return "", "", fmt.Errorf("%w: reading MachineGuid: %v", ErrNoStableSource, err)
	}
	for _, line := range strings.Split(string(out), "\n") {
		if !strings.Contains(line, "MachineGuid") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) >= 3 {
			return fields[len(fields)-1], "windows:MachineGuid", nil
		}
	}
	return "", "", fmt.Errorf("%w: MachineGuid not present in reg output", ErrNoStableSource)
}

func probeDarwin(opt Options) (string, string, error) {
	out, err := opt.run("ioreg", "-rd1", "-c", "IOPlatformExpertDevice")
	if err != nil {
		return "", "", fmt.Errorf("%w: reading IOPlatformUUID: %v", ErrNoStableSource, err)
	}
	for _, line := range strings.Split(string(out), "\n") {
		if !strings.Contains(line, "IOPlatformUUID") {
			continue
		}
		if _, after, ok := strings.Cut(line, "="); ok {
			return strings.Trim(strings.TrimSpace(after), `"`), "darwin:IOPlatformUUID", nil
		}
	}
	return "", "", fmt.Errorf("%w: IOPlatformUUID not present in ioreg output", ErrNoStableSource)
}
