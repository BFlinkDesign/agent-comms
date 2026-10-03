package gitsync

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
)

// emptyRemote is a bare repository with no commits, as GitHub creates one.
func emptyRemote(t *testing.T) string {
	t.Helper()
	requireGit(t)
	root := t.TempDir()
	remote := filepath.Join(root, "journal.git")
	run(t, root, "init", "--quiet", "--bare", "--initial-branch=main", remote)
	return remote
}

func mustInit(t *testing.T, o InitOptions) InitResult {
	t.Helper()
	res, err := Init(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	return res
}

// set up checks that a clone is ready for Sync: on the remote's branch, with an
// upstream, holding fleetd.json with the given salt.
func setUp(t *testing.T, dir, salt string) {
	t.Helper()
	if up := run(t, dir, "rev-parse", "--abbrev-ref", "@{upstream}"); up != "origin/main" {
		t.Fatalf("upstream = %q, want origin/main", up)
	}
	fleet, ok, err := ReadFleet(dir)
	if err != nil || !ok || fleet.Salt != salt {
		t.Fatalf("fleetd.json = %+v, %v, %v; want salt %q", fleet, ok, err, salt)
	}
}

func TestInitStartsAnEmptyJournalWithTheSaltItWasGiven(t *testing.T) {
	remote := emptyRemote(t)
	dir := filepath.Join(t.TempDir(), "journal")
	res := mustInit(t, InitOptions{URL: remote, Dir: dir, Salt: "fleet-salt"})
	if !res.Started || !res.WroteFleetFile || res.Branch != "main" || res.Salt != "fleet-salt" {
		t.Fatalf("result = %+v", res)
	}
	setUp(t, dir, "fleet-salt")
	if !strings.Contains(remoteFile(t, remote, FleetFile), `"salt": "fleet-salt"`) {
		t.Fatalf("the remote's fleetd.json is %q", remoteFile(t, remote, FleetFile))
	}
	if author := run(t, dir, "log", "-1", "--format=%an <%ae>"); author != "fleetd <fleetd@fleetd.invalid>" {
		t.Fatalf("the first commit is by %q, want fleetd's own identity", author)
	}
	// The clone is ready for a sync as it stands.
	appendLines(t, filepath.Join(dir, "host-a.jsonl"), `{"id":"hive:1"}`)
	if res := mustSync(t, options(dir, "host-a")); res.Published != 1 {
		t.Fatalf("sync after init: %+v", res)
	}
}

func TestInitGivesAJournalWithoutASaltARandomOne(t *testing.T) {
	remote := emptyRemote(t)
	a := mustInit(t, InitOptions{URL: remote, Dir: filepath.Join(t.TempDir(), "a")})
	if len(a.Salt) != 32 {
		t.Fatalf("salt = %q, want 32 hex digits", a.Salt)
	}
	// Every later machine takes the journal's salt, given none of its own.
	b := mustInit(t, InitOptions{URL: remote, Dir: filepath.Join(t.TempDir(), "b")})
	if b.Salt != a.Salt || b.Started || b.WroteFleetFile {
		t.Fatalf("second machine: %+v, want the first machine's salt and no commit", b)
	}
}

func TestInitAddsTheSaltToAJournalThatHasNone(t *testing.T) {
	remote, m := fleet(t, 1)
	appendLines(t, filepath.Join(m[0], "host-a.jsonl"), `{"id":"hive:1"}`)
	mustSync(t, options(m[0], "host-a"))

	dir := filepath.Join(t.TempDir(), "journal")
	res := mustInit(t, InitOptions{URL: remote, Dir: dir, Salt: "s"})
	if res.Started || !res.WroteFleetFile {
		t.Fatalf("result = %+v", res)
	}
	setUp(t, dir, "s")
	if remoteFile(t, remote, "host-a.jsonl") == "" || remoteFile(t, remote, "README.md") == "" {
		t.Fatal("adding fleetd.json lost the journal's other files")
	}
}

// A salt given explicitly that contradicts the journal's is refused; one taken
// from the environment is overridden, and said to be.
func TestInitRefusesASaltThatContradictsTheJournals(t *testing.T) {
	remote := emptyRemote(t)
	mustInit(t, InitOptions{URL: remote, Dir: filepath.Join(t.TempDir(), "a"), Salt: "first"})
	b := filepath.Join(t.TempDir(), "b")
	_, err := Init(context.Background(), InitOptions{URL: remote, Dir: b, Salt: "second", Strict: true})
	if !errors.Is(err, ErrSaltMismatch) {
		t.Fatalf("err = %v, want ErrSaltMismatch", err)
	}
	if _, err := os.Stat(b); !os.IsNotExist(err) {
		t.Fatalf("a refused init left %s behind: %v", b, err)
	}
	res := mustInit(t, InitOptions{URL: remote, Dir: b, Salt: "second"})
	if !res.SaltDiffers || res.Salt != "first" {
		t.Fatalf("result = %+v, want the journal's salt, said to differ", res)
	}
	if !strings.Contains(remoteFile(t, remote, FleetFile), `"salt": "first"`) {
		t.Fatal("the journal's salt changed")
	}
}

// A repository that already holds records has machines deriving ids with some
// salt. Init without one would give them all a second id, so it refuses, and
// pushes nothing.
func TestInitNeedsTheSaltOfAJournalThatHoldsRecords(t *testing.T) {
	remote, m := fleet(t, 1)
	appendLines(t, filepath.Join(m[0], "host-a.jsonl"), `{"id":"hive:1"}`)
	mustSync(t, options(m[0], "host-a"))
	before := run(t, m[0], "ls-remote", remote, "refs/heads/main")
	_, err := Init(context.Background(), InitOptions{URL: remote, Dir: filepath.Join(t.TempDir(), "journal")})
	if !errors.Is(err, ErrNeedSalt) {
		t.Fatalf("err = %v, want ErrNeedSalt", err)
	}
	if after := run(t, m[0], "ls-remote", remote, "refs/heads/main"); after != before {
		t.Fatalf("init pushed to a journal it refused: %s became %s", before, after)
	}
}

// A journal file is host-<id>.jsonl, and a journal holds no directories:
// training data in JSON Lines is not a journal, however few its other files.
func TestInitRefusesJSONLinesDataAndDirectories(t *testing.T) {
	requireGit(t)
	for name, files := range map[string]map[string]string{
		"eval data":   {"train.jsonl": "{}\n", "README.md": "data\n"},
		"a directory": {"host-a.jsonl": "{}\n", "readme-assets/main.go": "package main\n"},
	} {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			remote := filepath.Join(root, "repo.git")
			run(t, root, "init", "--quiet", "--bare", "--initial-branch=main", remote)
			seed := filepath.Join(root, "seed")
			run(t, root, "clone", "--quiet", remote, seed)
			identify(t, seed)
			for path, content := range files {
				if err := os.MkdirAll(filepath.Dir(filepath.Join(seed, path)), 0o755); err != nil {
					t.Fatal(err)
				}
				write(t, filepath.Join(seed, path), content)
			}
			run(t, seed, "add", ".")
			run(t, seed, "commit", "--quiet", "-m", "data")
			run(t, seed, "push", "--quiet", "-u", "origin", "main")
			_, err := Init(context.Background(), InitOptions{URL: remote, Dir: filepath.Join(root, "journal"), Salt: "s"})
			if !errors.Is(err, ErrNotJournal) {
				t.Fatalf("err = %v, want ErrNotJournal", err)
			}
		})
	}
}

// Two machines starting the same empty journal must start the same branch, so
// neither may use its own git's default: the remote's, else main.
func TestInitStartsAnEmptyJournalOnTheRemotesBranchNotThisMachinesDefault(t *testing.T) {
	remote := emptyRemote(t)
	config := filepath.Join(t.TempDir(), "gitconfig")
	// Protocol v0 does not advertise an empty repository's unborn HEAD, as some
	// servers do not, so the clone falls back to this machine's default.
	write(t, config, "[init]\n\tdefaultBranch = master\n[protocol]\n\tversion = 0\n")
	t.Setenv("GIT_CONFIG_GLOBAL", config)
	res := mustInit(t, InitOptions{URL: remote, Dir: filepath.Join(t.TempDir(), "journal"), Salt: "s"})
	if res.Branch != "main" {
		t.Fatalf("started branch %q, want main", res.Branch)
	}
	if heads := run(t, t.TempDir(), "ls-remote", "--heads", remote); strings.Count(heads, "refs/heads/") != 1 {
		t.Fatalf("the journal has branches %q, want main alone", heads)
	}
}

func TestInitRefusesARepositoryThatIsNotAJournal(t *testing.T) {
	requireGit(t)
	root := t.TempDir()
	remote := filepath.Join(root, "project.git")
	run(t, root, "init", "--quiet", "--bare", "--initial-branch=main", remote)
	seed := filepath.Join(root, "seed")
	run(t, root, "clone", "--quiet", remote, seed)
	identify(t, seed)
	write(t, filepath.Join(seed, "go.mod"), "module example\n")
	write(t, filepath.Join(seed, "README.md"), "a project\n")
	run(t, seed, "add", ".")
	run(t, seed, "commit", "--quiet", "-m", "a project")
	run(t, seed, "push", "--quiet", "-u", "origin", "main")
	before := run(t, seed, "ls-remote", remote, "refs/heads/main")

	_, err := Init(context.Background(), InitOptions{URL: remote, Dir: filepath.Join(root, "journal"), Salt: "s"})
	if !errors.Is(err, ErrNotJournal) || !strings.Contains(err.Error(), "go.mod") {
		t.Fatalf("err = %v, want ErrNotJournal naming go.mod", err)
	}
	if after := run(t, seed, "ls-remote", remote, "refs/heads/main"); after != before {
		t.Fatalf("init pushed to a repository it refused: %s became %s", before, after)
	}
}

// Two machines start the same empty journal at once. The one whose push loses
// takes the winner's commit, and with it the winner's salt, so the fleet ends
// with one salt and one first commit.
func TestTwoMachinesStartingAnEmptyJournalAtOnceAgreeOnOneSalt(t *testing.T) {
	remote := emptyRemote(t)
	root := t.TempDir()
	var winner InitResult
	raced := false
	loser := InitOptions{URL: remote, Dir: filepath.Join(root, "loser"), Run: func(ctx context.Context, dir string, stdin []byte, args ...string) (string, error) {
		if !raced && slices.Contains(args, "push") {
			raced = true
			winner = mustInit(t, InitOptions{URL: remote, Dir: filepath.Join(root, "winner")})
		}
		return Git(ctx, dir, stdin, args...)
	}}
	res := mustInit(t, loser)
	if !raced || !winner.Started {
		t.Fatalf("the race did not happen: winner %+v", winner)
	}
	if res.Salt != winner.Salt || res.WroteFleetFile || res.Started {
		t.Fatalf("loser = %+v, want the winner's salt %q and no commit of its own", res, winner.Salt)
	}
	setUp(t, loser.Dir, winner.Salt)
	if n := run(t, loser.Dir, "rev-list", "--count", "HEAD"); n != "1" {
		t.Fatalf("the journal has %s commits, want the winner's one", n)
	}
}

func TestReadFleetRefusesAFileWithoutASalt(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, FleetFile), []byte(`{"salt": ""}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := ReadFleet(dir); !errors.Is(err, ErrBadFleetFile) {
		t.Fatalf("err = %v, want ErrBadFleetFile", err)
	}
	if _, ok, err := ReadFleet(t.TempDir()); ok || err != nil {
		t.Fatalf("a directory without fleetd.json: ok %v, err %v", ok, err)
	}
}

// An empty repository whose HEAD names a branch, as GitHub's names its default,
// tells a clone which branch that is (protocol v2's unborn HEAD). The journal
// starts on it, so the repository's HEAD then resolves.
func TestInitStartsAnEmptyJournalOnTheBranchTheRemoteNames(t *testing.T) {
	requireGit(t)
	root := t.TempDir()
	remote := filepath.Join(root, "journal.git")
	run(t, root, "init", "--quiet", "--bare", "--initial-branch=trunk", remote)
	res := mustInit(t, InitOptions{URL: remote, Dir: filepath.Join(t.TempDir(), "journal"), Salt: "s"})
	if res.Branch != "trunk" {
		t.Fatalf("started branch %q, want trunk", res.Branch)
	}
	if head := run(t, root, "--git-dir", remote, "rev-parse", "--verify", "--quiet", "HEAD^{commit}"); head == "" {
		t.Fatal("the repository's HEAD does not resolve")
	}
}

// A server that does not advertise an empty repository's unborn HEAD leaves the
// first machine's clone on no branch, and that machine starts main. The
// repository's HEAD still names its own missing default, so the second machine's
// clone again names no branch: it follows main, the only branch there is, rather
// than being refused.
func TestASecondMachineFollowsTheOnlyBranchWhenTheServerNamesNone(t *testing.T) {
	requireGit(t)
	root := t.TempDir()
	remote := filepath.Join(root, "journal.git")
	run(t, root, "init", "--quiet", "--bare", "--initial-branch=master", remote)
	run(t, root, "--git-dir", remote, "config", "lsrefs.unborn", "ignore")

	first := mustInit(t, InitOptions{URL: remote, Dir: filepath.Join(root, "a"), Salt: "s"})
	if !first.Started || first.Branch != "main" {
		t.Fatalf("first machine = %+v, want it to start main", first)
	}
	second, err := Init(context.Background(), InitOptions{URL: remote, Dir: filepath.Join(root, "b")})
	if err != nil {
		t.Fatalf("the second machine was refused: %v", err)
	}
	if second.Started || second.Branch != "main" || second.Salt != "s" {
		t.Fatalf("second machine = %+v, want it to follow main with the first machine's salt", second)
	}
	setUp(t, filepath.Join(root, "b"), "s")
	if heads := run(t, root, "ls-remote", "--heads", remote); strings.Count(heads, "refs/heads/") != 1 {
		t.Fatalf("the journal has branches %q, want main alone", heads)
	}
}

// An init whose context ends while git is answering a question says the time ran
// out, never what a failure there would otherwise mean: not a clone, a clone of
// another repository, or no upstream.
func TestAnInitThatRunsOutOfTimeSaysSoWhereverItStops(t *testing.T) {
	for _, at := range []string{"--show-toplevel", "remote.origin.url", "@{u}"} {
		t.Run(at, func(t *testing.T) {
			remote := emptyRemote(t)
			dir := filepath.Join(t.TempDir(), "journal")
			mustInit(t, InitOptions{URL: remote, Dir: dir, Salt: "s"})
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			_, err := Init(ctx, InitOptions{URL: remote, Dir: dir, Run: func(ctx context.Context, d string, stdin []byte, args ...string) (string, error) {
				if slices.Contains(args, at) {
					cancel()
				}
				return Git(ctx, d, stdin, args...)
			}})
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("err = %v, want the cancellation", err)
			}
			for _, wrong := range []error{ErrNotClone, ErrOtherRemote, ErrNoUpstream} {
				if errors.Is(err, wrong) {
					t.Fatalf("err = %v: the time running out reads as %v", err, wrong)
				}
			}
		})
	}
}

// Two machines start the same empty journal at once, one told the default branch
// by the server (trunk) and one not (main). The one that finds both branches is
// told to delete one, instead of the fleet ending with two salts on two branches.
func TestTwoMachinesStartingTwoBranchesAtOnceAreToldSo(t *testing.T) {
	requireGit(t)
	root := t.TempDir()
	remote := filepath.Join(root, "journal.git")
	run(t, root, "init", "--quiet", "--bare", "--initial-branch=trunk", remote)
	raced := false
	_, err := Init(context.Background(), InitOptions{URL: remote, Dir: filepath.Join(root, "b"), Salt: "b",
		Run: func(ctx context.Context, dir string, stdin []byte, args ...string) (string, error) {
			if !raced && slices.Contains(args, "push") {
				raced = true
				mustInit(t, InitOptions{URL: remote, Dir: filepath.Join(root, "a"), Salt: "a"})
			}
			// This machine's git is not told the default branch (protocol v0).
			return Git(ctx, dir, stdin, append([]string{"-c", "protocol.version=0"}, args...)...)
		}})
	if !raced || err == nil || !strings.Contains(err.Error(), "two branches at once") || !strings.Contains(err.Error(), ".git") {
		t.Fatalf("raced %v, err = %v; want the second machine told the journal was started on two branches, "+
			"and what a machine already following the other one must do", raced, err)
	}
}

// A first push that fails for any reason but a race is reported as it is, at
// once: one the remote declines as refused, and one the network fails as that
// failure. init neither tries again nor says the journal still lacks fleetd.json.
func TestInitReportsAFirstPushThatFailsAtOnce(t *testing.T) {
	network := errors.New("network down")
	for _, tc := range []struct {
		name string
		want error
	}{{"declined", ErrRejected}, {"network", network}} {
		t.Run(tc.name, func(t *testing.T) {
			remote := emptyRemote(t)
			if tc.want == ErrRejected {
				hook := filepath.Join(remote, "hooks", "pre-receive")
				write(t, hook, "#!/bin/sh\necho 'protected branch' >&2\nexit 1\n")
				if err := os.Chmod(hook, 0o755); err != nil {
					t.Fatal(err)
				}
			}
			pushes := 0
			_, err := Init(context.Background(), InitOptions{URL: remote, Dir: filepath.Join(t.TempDir(), "journal"), Salt: "s",
				Run: func(ctx context.Context, dir string, stdin []byte, args ...string) (string, error) {
					if slices.Contains(args, "push") {
						pushes++
						if tc.want == network {
							return "", network
						}
					}
					return Git(ctx, dir, stdin, args...)
				}})
			if !errors.Is(err, tc.want) || pushes != 1 {
				t.Fatalf("err = %v after %d pushes, want %v after one", err, pushes, tc.want)
			}
		})
	}
}

// Hooks that fire at once can each note a past salt, and none of the salts is
// lost.
func TestPastSaltsNotedAtOnceAreAllKept(t *testing.T) {
	gitDir := t.TempDir()
	const n = 24
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- NotePastSalt(gitDir, fmt.Sprintf("salt-%02d", i))
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Errorf("noting a salt while others were noted: %v", err)
		}
	}
	got := PastSalts(gitDir)
	slices.Sort(got)
	got = slices.Compact(got)
	if len(got) != n {
		t.Fatalf("%d of %d salts noted at once are kept: %q", len(got), n, got)
	}
}

// A note whose last write stopped partway, on a full disk say, still takes the
// next salt whole.
func TestASaltNotedAfterAnUnfinishedLineIsKept(t *testing.T) {
	path := filepath.Join(t.TempDir(), "salts")
	if err := os.WriteFile(path, []byte("\"first\"\n\"seco"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := AppendSalt(path, "third"); err != nil {
		t.Fatal(err)
	}
	if got := ReadSalts(path); !slices.Equal(got, []string{"first", "third"}) {
		t.Fatalf("the salts read back are %q, want first and third", got)
	}
}

// init on a clone puts the journal's fleetd.json in git's index only once the
// work tree holds it. The other way round, an init stopped between the two
// leaves a file that differs from git's copy, which no sync replaces.
func TestInitPutsTheJournalsFleetFileInTheIndexOnlyOnceTheWorkTreeHasIt(t *testing.T) {
	remote := emptyRemote(t)
	root := t.TempDir()
	first := filepath.Join(root, "first")
	mustInit(t, InitOptions{URL: remote, Dir: first, Salt: "A"})
	clone := filepath.Join(root, "clone")
	run(t, root, "clone", "--quiet", remote, clone)
	want := `{"salt": "B"}` + "\n"
	if err := os.WriteFile(filepath.Join(first, FleetFile), []byte(want), 0o644); err != nil {
		t.Fatal(err)
	}
	run(t, first, "-c", "user.name=t", "-c", "user.email=t@example.invalid", "commit", "--quiet", "-am", "a new salt")
	run(t, first, "push", "--quiet")
	var early []string
	_, err := Init(context.Background(), InitOptions{URL: remote, Dir: clone, Run: func(ctx context.Context, dir string, stdin []byte, args ...string) (string, error) {
		if slices.Contains(args, "update-index") && slices.ContainsFunc(args, func(a string) bool { return strings.Contains(a, FleetFile) }) {
			if have, _ := os.ReadFile(filepath.Join(clone, FleetFile)); string(have) != want {
				early = append(early, strings.Join(args, " "))
			}
		}
		return Git(ctx, dir, stdin, args...)
	}})
	if err != nil {
		t.Fatal(err)
	}
	if len(early) > 0 {
		t.Fatalf("init put fleetd.json in the index before the work tree held the journal's copy: %q", early)
	}
	if have, err := os.ReadFile(filepath.Join(clone, FleetFile)); err != nil || string(have) != want {
		t.Fatalf("after init the clone's fleetd.json is %q (%v), want the journal's", have, err)
	}
}

// init sizes up the journal's fleetd.json before reading any of it, on a new
// directory or a clone: one far larger, which anyone able to push can commit,
// would otherwise be read whole into memory before it was refused.
func TestInitNeverReadsAnOversizedFleetFile(t *testing.T) {
	remote := emptyRemote(t)
	root := t.TempDir()
	first := filepath.Join(root, "first")
	mustInit(t, InitOptions{URL: remote, Dir: first, Salt: "s"})
	big := `{"salt": "s"}` + strings.Repeat(" ", 70<<10) + "\n"
	if err := os.WriteFile(filepath.Join(first, FleetFile), []byte(big), 0o644); err != nil {
		t.Fatal(err)
	}
	run(t, first, "-c", "user.name=t", "-c", "user.email=t@example.invalid", "commit", "--quiet", "-am", "a large fleetd.json")
	run(t, first, "push", "--quiet")
	for _, dir := range []string{filepath.Join(root, "new"), first} {
		var read []string
		_, err := Init(context.Background(), InitOptions{URL: remote, Dir: dir, Run: func(ctx context.Context, dir string, stdin []byte, args ...string) (string, error) {
			if slices.Contains(args, "blob") && slices.ContainsFunc(args, func(a string) bool { return strings.HasSuffix(a, ":"+FleetFile) }) {
				read = append(read, strings.Join(args, " "))
			}
			return Git(ctx, dir, stdin, args...)
		}})
		if !errors.Is(err, ErrBadFleetFile) || len(read) > 0 {
			t.Fatalf("init --dir %s on a %d-byte fleetd.json: %v, having read it with %q; want ErrBadFleetFile, unread",
				dir, len(big), err, read)
		}
	}
}

// fleetd.json is a few lines. One far larger is not read, even when it is valid
// JSON throughout, so that a file of any size, which anyone able to push can
// commit, cannot exhaust memory.
func TestReadFleetRefusesAnOversizedFleetFile(t *testing.T) {
	dir := t.TempDir()
	big := `{"salt": "s"}` + strings.Repeat(" ", 100<<10) + "\n"
	if err := os.WriteFile(filepath.Join(dir, FleetFile), []byte(big), 0o644); err != nil {
		t.Fatal(err)
	}
	if f, ok, err := ReadFleet(dir); !errors.Is(err, ErrBadFleetFile) {
		t.Fatalf("ReadFleet on a %d-byte fleetd.json: %+v, %v, %v; want ErrBadFleetFile", len(big), f, ok, err)
	}
}

// A salts file holds a line per salt. One far larger, even of salts throughout,
// is neither read nor appended to, so that a large file planted there cannot
// exhaust memory.
func TestASaltsFileTooLargeToBeOneIsRefused(t *testing.T) {
	path := filepath.Join(t.TempDir(), "salts")
	var b strings.Builder
	for i := 0; b.Len() < 2<<20; i++ {
		fmt.Fprintf(&b, "%q\n", fmt.Sprintf("salt-%d", i))
	}
	if err := os.WriteFile(path, []byte(b.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	size := int64(b.Len())
	if got := ReadSalts(path); len(got) != 0 {
		t.Fatalf("read %q from an oversized salts file", got)
	}
	if err := AppendSalt(path, "x"); err == nil {
		t.Fatal("noted a salt in an oversized salts file")
	}
	if info, err := os.Stat(path); err != nil || info.Size() != size {
		t.Fatalf("the oversized salts file changed: %v (%v)", info.Size(), err)
	}
}
