package gitsync

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
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
	return newEmptyRemote(t)
}

// newEmptyRemote is emptyRemote for a subtest, whose parent has called
// requireGit.
func newEmptyRemote(t *testing.T) string {
	t.Helper()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
	requireGit(t)
	for _, at := range []string{"--show-toplevel", "remote.origin.url", "@{u}",
		"detached symbolic-ref", "detached ls-remote", "detached update-ref", "detached read-tree",
		"behind merge-base", "hand for-each-ref"} {
		t.Run(at, func(t *testing.T) {
			t.Parallel()
			remote := newEmptyRemote(t)
			dir := filepath.Join(t.TempDir(), "journal")
			mustInit(t, InitOptions{URL: remote, Dir: dir, Salt: "s"})
			// Off the journal's branch, the time can run out while init puts the clone
			// back: detached with main gone, or behind origin's, or on a branch made by hand.
			if state, call, ok := strings.Cut(at, " "); ok {
				other := filepath.Join(t.TempDir(), "other")
				run(t, filepath.Dir(other), "clone", "--quiet", remote, other)
				identify(t, other)
				write(t, filepath.Join(other, "host-b.jsonl"), "{\"id\":\"hive:b1\"}\n")
				run(t, other, "add", ".")
				run(t, other, "commit", "--quiet", "-m", "b")
				run(t, other, "push", "--quiet")
				switch state {
				case "detached":
					run(t, dir, "checkout", "--quiet", "--detach")
					run(t, dir, "branch", "--quiet", "-D", "main")
				case "behind":
					run(t, dir, "checkout", "--quiet", "--detach")
				case "hand":
					run(t, dir, "switch", "--quiet", "-c", "local-only")
				}
				at = call
			}
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
	t.Parallel()
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
	if !raced || err == nil || !strings.Contains(err.Error(), "two branches at once") || !strings.Contains(err.Error(), ".git") ||
		!strings.Contains(err.Error(), "`fleetd init --dir \"<its journal directory>\" --branch <branch> <journal URL>`") {
		t.Fatalf("raced %v, err = %v; want the second machine told the journal was started on two branches, "+
			"and what a machine already following the other one must do", raced, err)
	}
}

// A first push that fails for any reason but a race is reported as it is, at
// once: one the remote declines as refused, and one the network fails as that
// failure. init neither tries again nor says the journal still lacks fleetd.json.
func TestInitReportsAFirstPushThatFailsAtOnce(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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

// A clone put back on the journal's branch goes to the remote's default, as a new
// machine's clone would, even with other branches beside it.
func TestInitPutsACloneBackOnTheRemotesDefaultBranch(t *testing.T) {
	t.Parallel()
	remote, m := fleet(t, 2)
	a, admin := m[0], m[1]
	run(t, admin, "push", "--quiet", "origin", "main:refs/heads/trunk", "main:refs/heads/stray")
	run(t, admin, "--git-dir", remote, "symbolic-ref", "HEAD", "refs/heads/trunk")
	run(t, admin, "push", "--quiet", "origin", ":refs/heads/main")
	run(t, a, "checkout", "--quiet", "--detach")
	run(t, a, "branch", "--quiet", "-D", "main")
	if res := mustInit(t, InitOptions{URL: remote, Dir: a, Salt: "s"}); !res.Reattached || res.Branch != "trunk" {
		t.Fatalf("init = %+v, want the clone put on trunk", res)
	}
	if up := run(t, a, "rev-parse", "--abbrev-ref", "@{upstream}"); up != "origin/trunk" {
		t.Fatalf("after init the clone follows %s", up)
	}
}

// With several branches, none of them the remote's default, and no branch here
// that follows one of them, the journal could be on any: init moves nothing, and
// says to make the right one the default.
func TestInitPutsNoCloneBackOnABranchItWouldHaveToGuess(t *testing.T) {
	t.Parallel()
	remote, m := fleet(t, 2)
	a, admin := m[0], m[1]
	run(t, admin, "push", "--quiet", "origin", "main:refs/heads/stray")
	run(t, admin, "--git-dir", remote, "symbolic-ref", "HEAD", "refs/heads/gone")
	run(t, a, "checkout", "--quiet", "--detach")
	run(t, a, "branch", "--quiet", "-D", "main")
	_, err := Init(context.Background(), InitOptions{URL: remote, Dir: a, Salt: "s"})
	if !errors.Is(err, ErrNoDefaultBranch) {
		t.Fatalf("init with several branches and no default: %v, want ErrNoDefaultBranch", err)
	}
	if want := "`fleetd init --dir \"" + a + "\" --branch <branch> <journal URL>`"; !strings.Contains(err.Error(), want) {
		t.Fatalf("init: %v, want it to name %s", err, want)
	}
	if out, err := exec.Command("git", "-C", a, "symbolic-ref", "--quiet", "HEAD").Output(); err == nil {
		t.Fatalf("init moved HEAD to %s", out)
	}
	if res := mustInit(t, InitOptions{URL: remote, Dir: a, Salt: "s", Branch: "main"}); !res.Reattached || res.Branch != "main" {
		t.Fatalf("init = %+v, want the clone put on main", res)
	}
}

// A server that names no default branch, as git before 2.31 does for one that
// does not exist, leaves origin's only branch as the journal's, for a clone that
// follows none of origin's.
func TestInitPutsACloneBackOnOriginsOnlyBranchWhenTheRemoteNamesNoDefault(t *testing.T) {
	t.Parallel()
	remote, m := fleet(t, 1)
	run(t, m[0], "--git-dir", remote, "symbolic-ref", "HEAD", "refs/heads/master")
	run(t, m[0], "--git-dir", remote, "config", "lsrefs.unborn", "ignore")
	run(t, m[0], "checkout", "--quiet", "--detach")
	run(t, m[0], "branch", "--quiet", "-D", "main")
	if res := mustInit(t, InitOptions{URL: remote, Dir: m[0], Salt: "s"}); !res.Reattached || res.Branch != "main" {
		t.Fatalf("init = %+v, want the clone put on main", res)
	}
}

// The journal's branch here moves only forward, to origin's tip: one with a
// commit of its own stays where it is, and the sync after init reports it, with
// the way to drop it that keeps this machine's records.
func TestInitLeavesABranchWithACommitOfItsOwnWhereItIs(t *testing.T) {
	t.Parallel()
	remote, m := fleet(t, 1)
	a := m[0]
	write(t, filepath.Join(a, "notes.txt"), "mine\n")
	run(t, a, "add", "notes.txt")
	run(t, a, "commit", "--quiet", "-m", "not pushed")
	mine := run(t, a, "rev-parse", "main")
	run(t, a, "branch", "--unset-upstream")
	if res := mustInit(t, InitOptions{URL: remote, Dir: a, Salt: "s"}); !res.Reattached {
		t.Fatalf("init = %+v, want the clone put back", res)
	}
	if now := run(t, a, "rev-parse", "main"); now != mine {
		t.Fatalf("init moved main from %s to %s", mine, now)
	}
	if _, err := Sync(context.Background(), options(a, "host-a")); !errors.Is(err, ErrLocalCommits) {
		t.Fatalf("expected ErrLocalCommits, got %v", err)
	}
}

// A current branch that follows one of origin's the clone no longer has is left
// for a person: the remote may have deleted or renamed it, and init would not know
// where the journal went. Nothing moves, and nothing is pushed, whether a person
// pruned the clone's copy of the branch or init's own fetch did.
func TestInitLeavesABranchTheCloneNoLongerHasForAPerson(t *testing.T) {
	t.Parallel()
	requireGit(t)
	for _, pruned := range []bool{false, true} {
		t.Run(map[bool]string{false: "pruned by init", true: "pruned by hand"}[pruned], func(t *testing.T) {
			t.Parallel()
			remote, m := newFleet(t, 2)
			a, admin := m[0], m[1]
			run(t, admin, "push", "--quiet", "origin", "main:refs/heads/trunk")
			run(t, admin, "--git-dir", remote, "symbolic-ref", "HEAD", "refs/heads/trunk")
			run(t, admin, "push", "--quiet", "origin", ":refs/heads/main")
			if pruned {
				run(t, a, "fetch", "--quiet", "--prune")
			}
			_, err := Init(context.Background(), InitOptions{URL: remote, Dir: a, Salt: "s"})
			if !errors.Is(err, ErrNoUpstream) || !strings.Contains(err.Error(), "main follows origin/main, which this clone no longer has") {
				t.Fatalf("init on a branch the remote deleted: %v, want ErrNoUpstream saying so", err)
			}
			if up := run(t, a, "config", "branch.main.merge"); up != "refs/heads/main" {
				t.Fatalf("init changed main's upstream to %s", up)
			}
			if out, err := exec.Command("git", "--git-dir", remote, "rev-parse", "--verify", "--quiet", "refs/heads/main").Output(); err == nil {
				t.Fatalf("init recreated main at %s", out)
			}
		})
	}
}

// A repository with commits of its own whose origin is the empty journal
// repository has nothing to follow: init says to move its git directory, and the
// commits with it, out of the journal directory, rather than to delete it, from a
// branch, an orphan branch or a detached HEAD alike, and whatever ref holds them.
func TestInitTellsACloneWithCommitsBesideAnEmptyRepositoryToMoveThemOut(t *testing.T) {
	t.Parallel()
	requireGit(t)
	for _, c := range []struct {
		name  string
		leave [][]string
	}{
		{"on its branch", nil},
		{"on an orphan branch", [][]string{{"checkout", "--quiet", "--orphan", "scratch"}}},
		{"detached, with no branch", [][]string{{"checkout", "--quiet", "--detach"}, {"branch", "--quiet", "-D", "main"}}},
		{"on an orphan branch, with its commit held by a tag alone", [][]string{
			{"tag", "keep"}, {"checkout", "--quiet", "--orphan", "scratch"}, {"branch", "--quiet", "-D", "main"},
		}},
		{"on an orphan branch, with its commit held by a stash alone", [][]string{
			{"rm", "-q", "--cached", "notes.txt"}, {"stash", "push", "--quiet"},
			{"checkout", "--quiet", "--orphan", "scratch"}, {"branch", "--quiet", "-D", "main"},
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			remote := newEmptyRemote(t)
			dir := filepath.Join(t.TempDir(), "journal")
			run(t, filepath.Dir(dir), "init", "--quiet", "--initial-branch=main", dir)
			identify(t, dir)
			write(t, filepath.Join(dir, "notes.txt"), "mine\n")
			run(t, dir, "add", "notes.txt")
			run(t, dir, "commit", "--quiet", "-m", "mine")
			run(t, dir, "remote", "add", "origin", remote)
			for _, args := range c.leave {
				run(t, dir, args...)
			}
			_, err := Init(context.Background(), InitOptions{URL: remote, Dir: dir, Salt: "s"})
			if !errors.Is(err, ErrNoUpstream) || !strings.Contains(err.Error(), "move its .git directory, and every file there but journal files") ||
				strings.Contains(err.Error(), "delete") {
				t.Fatalf("init beside an empty repository with commits of its own: %v, want ErrNoUpstream saying to move them out", err)
			}
		})
	}
}

// clonedState is what init must leave as it was when it refuses: where HEAD is,
// every ref, the index and every branch's upstream.
func clonedState(t *testing.T, dir string) string {
	t.Helper()
	head, err := exec.Command("git", "-C", dir, "symbolic-ref", "--quiet", "HEAD").Output()
	if err != nil {
		head = []byte(run(t, dir, "rev-parse", "HEAD"))
	}
	branches, _ := exec.Command("git", "-C", dir, "config", "--get-regexp", `^branch\.`).Output()
	return strings.Join([]string{string(head), run(t, dir, "for-each-ref", "--format=%(refname) %(objectname)"),
		run(t, dir, "ls-files", "--stage"), string(branches)}, "\n--\n")
}

// unrecord takes away the journal's branch init recorded, if it did, as a clone
// set up before init recorded one has none.
func unrecord(t *testing.T, dir string) {
	t.Helper()
	if err := os.Remove(filepath.Join(dir, ".git", branchName)); err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
}

// A repository that is not a journal, given to init by mistake with its own URL,
// is refused before anything in it moves: not its branches, not HEAD, not a change
// a person staged.
func TestInitMovesNothingInARepositoryThatIsNotAJournal(t *testing.T) {
	t.Parallel()
	requireGit(t)
	for _, c := range []struct {
		name  string
		leave [][]string
	}{
		{"on a branch of its own", [][]string{{"switch", "--quiet", "-c", "feature"}}},
		{"detached", [][]string{{"checkout", "--quiet", "--detach"}}},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			root := t.TempDir()
			remote := filepath.Join(root, "project.git")
			run(t, root, "init", "--quiet", "--bare", "--initial-branch=main", remote)
			seed := filepath.Join(root, "seed")
			run(t, root, "clone", "--quiet", remote, seed)
			identify(t, seed)
			write(t, filepath.Join(seed, "go.mod"), "module example.com/project\n")
			run(t, seed, "add", ".")
			run(t, seed, "commit", "--quiet", "-m", "start")
			run(t, seed, "push", "--quiet", "origin", "main")
			dir := filepath.Join(root, "project")
			run(t, root, "clone", "--quiet", remote, dir)
			// main falls a commit behind origin's, where a repair would move it.
			write(t, filepath.Join(seed, "go.mod"), "module example.com/project\n\ngo 1.27\n")
			run(t, seed, "commit", "--quiet", "-am", "go")
			run(t, seed, "push", "--quiet", "origin", "main")
			run(t, dir, "fetch", "--quiet")
			for _, args := range c.leave {
				run(t, dir, args...)
			}
			write(t, filepath.Join(dir, "staged.txt"), "a change a person staged\n")
			run(t, dir, "add", "staged.txt")
			before := clonedState(t, dir)
			if _, err := Init(context.Background(), InitOptions{URL: remote, Dir: dir, Salt: "s"}); !errors.Is(err, ErrNotJournal) {
				t.Fatalf("init on a project's clone: %v, want ErrNotJournal", err)
			}
			if after := clonedState(t, dir); after != before {
				t.Fatalf("init moved things in a repository it refused:\nbefore:\n%s\nafter:\n%s", before, after)
			}
		})
	}
}

// A repair cut short at any step, by a git that fails or a kill, leaves a clone
// off the journal's branch, which init run again puts back: HEAD moves last, and
// a branch's upstream never reads as one origin no longer has.
func TestARepairCutShortIsFinishedByInitRunAgain(t *testing.T) {
	t.Parallel()
	requireGit(t)
	orphan := [][]string{{"checkout", "--quiet", "--orphan", "scratch"}, {"rm", "-r", "-q", "--cached", "."}}
	followsX := [][]string{{"branch", "--quiet", "x"}, {"branch", "--quiet", "-u", "x"}}
	for _, c := range []struct {
		name  string
		leave [][]string
		at    []string
	}{
		{"an orphan branch, filling the index", orphan, []string{"read-tree"}},
		{"an orphan branch, moving main", orphan, []string{"update-ref"}},
		{"an orphan branch, main's upstream branch", orphan, []string{"config", "branch.main.merge"}},
		{"an orphan branch, main's upstream remote", orphan, []string{"config", "branch.main.remote"}},
		{"an orphan branch, HEAD", orphan, []string{"symbolic-ref", "refs/heads/main"}},
		{"main following a branch of the clone, its upstream branch", followsX, []string{"config", "branch.main.merge"}},
		{"main following a branch of the clone, its upstream remote", followsX, []string{"config", "branch.main.remote"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			remote, a, _ := offBranch(t)
			for _, args := range c.leave {
				run(t, a, args...)
			}
			appendLines(t, filepath.Join(a, "host-a.jsonl"), `{"id":"hive:a2"}`)
			cut := false
			_, err := Init(context.Background(), InitOptions{URL: remote, Dir: a, Salt: "s",
				Run: func(ctx context.Context, dir string, stdin []byte, args ...string) (string, error) {
					if !cut && len(args) > 0 && !slices.ContainsFunc(c.at, func(s string) bool { return !slices.Contains(args, s) }) {
						cut = true
						return "", errors.New("cut short")
					}
					return Git(ctx, dir, stdin, args...)
				}})
			if !cut || err == nil {
				t.Fatalf("the repair was not cut short at %v: %v", c.at, err)
			}
			if res := mustInit(t, InitOptions{URL: remote, Dir: a, Salt: "s"}); !res.Reattached {
				t.Fatalf("init run again = %+v, want it to finish putting the clone back", res)
			}
			mustSync(t, options(a, "host-a"))
			if got := remoteFile(t, remote, "host-a.jsonl"); got != "{\"id\":\"hive:a1\"}\n{\"id\":\"hive:a2\"}\n" {
				t.Fatalf("main holds %q for host-a", got)
			}
			if got := readFile(t, filepath.Join(a, "host-b.jsonl")); got != "{\"id\":\"hive:b1\"}\n{\"id\":\"hive:b2\"}\n" {
				t.Fatalf("the work tree holds %q for host-b", got)
			}
			if out := run(t, a, "status", "--porcelain", "--untracked-files=no"); out != "" {
				t.Fatalf("git status after the sync:\n%s", out)
			}
		})
	}
}

// A remote's default can change while its machines go on publishing to the branch
// they follow. A clone put back goes to the branch it follows, if the remote still
// has it, rather than split the journal; with that branch gone, to the default.
func TestInitPutsACloneBackOnTheBranchItFollows(t *testing.T) {
	t.Parallel()
	requireGit(t)
	for _, c := range []struct {
		name       string
		deleteMain bool
		want       string
	}{
		{"the default changed to trunk, main still in use", false, "main"},
		{"main renamed to trunk", true, "trunk"},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			remote, m := newFleet(t, 2)
			a, admin := m[0], m[1]
			run(t, admin, "push", "--quiet", "origin", "main:refs/heads/trunk")
			run(t, admin, "--git-dir", remote, "symbolic-ref", "HEAD", "refs/heads/trunk")
			if c.deleteMain {
				run(t, admin, "push", "--quiet", "origin", ":refs/heads/main")
			}
			run(t, a, "checkout", "--quiet", "--detach")
			if res := mustInit(t, InitOptions{URL: remote, Dir: a, Salt: "s"}); !res.Reattached || res.Branch != c.want {
				t.Fatalf("init = %+v, want the clone put back on %s", res, c.want)
			}
		})
	}
}

// A clone that follows several of origin's branches goes back to the remote's
// default among them. With none of them the default, the fleet may publish to
// any, since a remote's default can move: init moves nothing, and says to name
// the journal's branch, after which init puts the clone back there.
func TestInitPutsACloneFollowingSeveralBranchesBackOnTheDefaultAmongThem(t *testing.T) {
	t.Parallel()
	requireGit(t)
	for _, c := range []struct {
		name, head, want string
		why              error
	}{
		{"trunk the default", "refs/heads/trunk", "trunk", nil},
		{"a third branch the default", "refs/heads/journal", "", ErrNoUpstream},
		{"no branch the default", "refs/heads/gone", "", ErrNoDefaultBranch},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			remote, m := newFleet(t, 2)
			a, admin := m[0], m[1]
			run(t, admin, "push", "--quiet", "origin", "main:refs/heads/trunk", "main:refs/heads/journal")
			run(t, admin, "--git-dir", remote, "symbolic-ref", "HEAD", c.head)
			run(t, a, "fetch", "--quiet")
			run(t, a, "branch", "--quiet", "--track", "trunk", "origin/trunk")
			run(t, a, "checkout", "--quiet", "--detach")
			before := clonedState(t, a)
			res, err := Init(context.Background(), InitOptions{URL: remote, Dir: a, Salt: "s"})
			if c.why != nil {
				advice := "`fleetd init --dir \"" + a + "\" --branch <branch> <journal URL>`"
				if !errors.Is(err, c.why) || !strings.Contains(err.Error(), "main, trunk") || !strings.Contains(err.Error(), advice) {
					t.Fatalf("init on a clone following two branches, neither the default: %v, want %v naming both and saying %s", err, c.why, advice)
				}
				if after := clonedState(t, a); after != before {
					t.Fatalf("init moved things in a clone it refused:\nbefore:\n%s\nafter:\n%s", before, after)
				}
				// The fleet publishes to main.
				c.want = "main"
				res, err = Init(context.Background(), InitOptions{URL: remote, Dir: a, Salt: "s", Branch: "main"})
			}
			if err != nil || !res.Reattached || res.Branch != c.want {
				t.Fatalf("init = %+v, %v; want the clone put back on %s", res, err, c.want)
			}
		})
	}
}

// A clone in the middle of something a person does by hand is left to them, with
// nothing moved: a rebase stopped on a conflict, a commit on a detached HEAD that no
// branch holds, the journal's branch checked out in another worktree.
func TestInitLeavesACloneInTheMiddleOfSomethingToAPerson(t *testing.T) {
	t.Parallel()
	requireGit(t)
	for _, c := range []struct {
		name string
		do   func(t *testing.T, a string)
		want string
	}{
		{"a rebase stopped on a conflict", func(t *testing.T, a string) {
			run(t, a, "switch", "--quiet", "-c", "mine")
			write(t, filepath.Join(a, "README.md"), "mine\n")
			run(t, a, "commit", "--quiet", "-am", "mine")
			run(t, a, "switch", "--quiet", "main")
			write(t, filepath.Join(a, "README.md"), "theirs\n")
			run(t, a, "commit", "--quiet", "-am", "theirs")
			run(t, a, "switch", "--quiet", "mine")
			if err := exec.Command("git", "-C", a, "rebase", "--quiet", "main").Run(); err == nil {
				t.Fatal("the rebase did not stop on a conflict")
			}
		}, "in the middle of a rebase"},
		{"a commit on a detached HEAD", func(t *testing.T, a string) {
			run(t, a, "checkout", "--quiet", "--detach")
			write(t, filepath.Join(a, "notes.txt"), "mine\n")
			run(t, a, "add", "notes.txt")
			run(t, a, "commit", "--quiet", "-m", "mine")
		}, "a commit no branch or tag holds"},
		{"main checked out in another worktree", func(t *testing.T, a string) {
			run(t, a, "checkout", "--quiet", "--detach")
			run(t, a, "worktree", "add", "--quiet", filepath.Join(t.TempDir(), "other"), "main")
		}, "is checked out in the worktree at"},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			remote, m := newFleet(t, 1)
			a := m[0]
			c.do(t, a)
			before := clonedState(t, a)
			_, err := Init(context.Background(), InitOptions{URL: remote, Dir: a, Salt: "s"})
			if !errors.Is(err, ErrNoUpstream) || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("init: %v, want ErrNoUpstream saying %q", err, c.want)
			}
			if after := clonedState(t, a); after != before {
				t.Fatalf("init moved things in a clone it left to a person:\nbefore:\n%s\nafter:\n%s", before, after)
			}
		})
	}
}

// A branch of origin's that init's own fetch did not bring in, such as one made
// just after it, is fetched for the repair.
func TestInitFetchesTheJournalsBranchItPutsTheCloneBackOn(t *testing.T) {
	t.Parallel()
	remote, m := fleet(t, 1)
	a := m[0]
	run(t, a, "checkout", "--quiet", "--detach")
	run(t, a, "branch", "--quiet", "-D", "main")
	run(t, a, "update-ref", "-d", "refs/remotes/origin/main")
	first := true
	res, err := Init(context.Background(), InitOptions{URL: remote, Dir: a, Salt: "s",
		Run: func(ctx context.Context, dir string, stdin []byte, args ...string) (string, error) {
			if first && slices.Contains(args, "fetch") {
				first = false
				return "", nil
			}
			return Git(ctx, dir, stdin, args...)
		}})
	if err != nil || !res.Reattached || res.Branch != "main" {
		t.Fatalf("init = %+v, %v; want the clone put back on main", res, err)
	}
	mustSync(t, options(a, "host-a"))
}

// A clone with no remote named origin is told so: fleetd syncs with origin alone.
func TestInitSaysWhenThereIsNoRemoteNamedOrigin(t *testing.T) {
	t.Parallel()
	remote, m := fleet(t, 1)
	run(t, m[0], "remote", "rename", "origin", "upstream")
	_, err := Sync(context.Background(), options(m[0], "h"))
	if !errors.Is(err, ErrNoUpstream) || !strings.Contains(err.Error(), "fleetd init --dir") {
		t.Fatalf("sync on a clone whose remote is upstream: %v, want ErrNoUpstream saying to run init", err)
	}
	_, err = Init(context.Background(), InitOptions{URL: remote, Dir: m[0], Salt: "s"})
	if !errors.Is(err, ErrOtherRemote) || !strings.Contains(err.Error(), "has no remote named origin") {
		t.Fatalf("init on a clone whose remote is upstream: %v, want ErrOtherRemote saying it has no origin", err)
	}
	run(t, m[0], "remote", "rename", "upstream", "origin")
	mustInit(t, InitOptions{URL: remote, Dir: m[0], Salt: "s"})
}

// A clone made with --single-branch fetches its own branch alone. Put back on
// another of origin's, such as the remote's default after it changed, it fetches
// that branch too from then on: its syncs publish there and bring in the others',
// and init run again finishes a repair cut short as it widened the fetch.
func TestInitPutsASingleBranchCloneBackOnABranchItDidNotFetch(t *testing.T) {
	t.Parallel()
	requireGit(t)
	for _, cut := range []bool{false, true} {
		t.Run(map[bool]string{false: "in one go", true: "cut short as it widens the fetch"}[cut], func(t *testing.T) {
			t.Parallel()
			remote, m := newFleet(t, 1)
			b := m[0]
			run(t, b, "push", "--quiet", "origin", "main:refs/heads/trunk")
			run(t, b, "--git-dir", remote, "symbolic-ref", "HEAD", "refs/heads/trunk")
			run(t, b, "fetch", "--quiet")
			run(t, b, "switch", "--quiet", "--track", "origin/trunk")
			a := filepath.Join(t.TempDir(), "a")
			run(t, filepath.Dir(a), "clone", "--quiet", "--single-branch", "--branch", "main", remote, a)
			run(t, a, "branch", "--unset-upstream")
			// Lines of the clone's refspec that name no branch of origin's to fetch.
			run(t, a, "config", "--add", "remote.origin.fetch", "+refs/notes/*:refs/notes/*")
			run(t, a, "config", "--add", "remote.origin.fetch", "^refs/heads/huge")
			appendLines(t, filepath.Join(a, "host-a.jsonl"), `{"id":"hive:a1"}`)
			if cut {
				_, err := Init(context.Background(), InitOptions{URL: remote, Dir: a, Salt: "s",
					Run: func(ctx context.Context, dir string, stdin []byte, args ...string) (string, error) {
						if slices.Contains(args, "remote.origin.fetch") {
							return "", errors.New("cut short")
						}
						return Git(ctx, dir, stdin, args...)
					}})
				if err == nil {
					t.Fatal("the repair was not cut short")
				}
			}
			if res := mustInit(t, InitOptions{URL: remote, Dir: a, Salt: "s"}); !res.Reattached || res.Branch != "trunk" {
				t.Fatalf("init = %+v, want the clone put back on trunk", res)
			}
			mustSync(t, options(a, "host-a"))
			appendLines(t, filepath.Join(b, "host-b.jsonl"), `{"id":"hive:b1"}`)
			mustSync(t, options(b, "host-b"))
			mustSync(t, options(a, "host-a"))
			if got := run(t, a, "--git-dir", remote, "show", "trunk:host-a.jsonl"); got != `{"id":"hive:a1"}` {
				t.Fatalf("trunk holds %q for host-a", got)
			}
			if got := readFile(t, filepath.Join(a, "host-b.jsonl")); got != "{\"id\":\"hive:b1\"}\n" {
				t.Fatalf("the work tree holds %q for host-b", got)
			}
			// The clone now fetches every branch of origin's, so that main, which it
			// was made with, can go: the clone's own fetch, and sync, carry on. Its
			// other lines stay.
			refspec := strings.Split(run(t, a, "config", "--get-all", "remote.origin.fetch"), "\n")
			slices.Sort(refspec)
			if want := []string{"+refs/heads/*:refs/remotes/origin/*", "+refs/notes/*:refs/notes/*", "^refs/heads/huge"}; !slices.Equal(refspec, want) {
				t.Fatalf("the clone fetches %q, want %q", refspec, want)
			}
			run(t, b, "push", "--quiet", "origin", ":refs/heads/main")
			run(t, a, "fetch", "--quiet")
			mustSync(t, options(a, "host-a"))
		})
	}
}

// Beside an empty journal repository, an init whose context ends while it looks
// for a commit of the clone's own says the time ran out, never that the clone has
// none and its .git directory can go.
func TestAnInitBesideAnEmptyRepositoryThatRunsOutOfTimeSaysSo(t *testing.T) {
	t.Parallel()
	requireGit(t)
	for _, at := range []string{"HEAD^{commit}", "--count=1"} {
		t.Run(at, func(t *testing.T) {
			t.Parallel()
			remote := newEmptyRemote(t)
			dir := filepath.Join(t.TempDir(), "journal")
			run(t, filepath.Dir(dir), "init", "--quiet", "--initial-branch=main", dir)
			identify(t, dir)
			write(t, filepath.Join(dir, "notes.txt"), "mine\n")
			run(t, dir, "add", "notes.txt")
			run(t, dir, "commit", "--quiet", "-m", "mine")
			run(t, dir, "remote", "add", "origin", remote)
			run(t, dir, "checkout", "--quiet", "--detach")
			run(t, dir, "branch", "--quiet", "-D", "main")
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			_, err := Init(ctx, InitOptions{URL: remote, Dir: dir, Salt: "s",
				Run: func(ctx context.Context, d string, stdin []byte, args ...string) (string, error) {
					if slices.Contains(args, at) {
						cancel()
					}
					return Git(ctx, d, stdin, args...)
				}})
			if !errors.Is(err, context.Canceled) || errors.Is(err, ErrNoUpstream) {
				t.Fatalf("err = %v, want the cancellation", err)
			}
		})
	}
}

// The remote's default is the branch its HEAD names, never one a branch of the
// remote names, as master does when a remote keeps it as a symbolic ref to main.
func TestInitTakesTheRemotesDefaultFromItsHEADAlone(t *testing.T) {
	t.Parallel()
	remote, m := fleet(t, 2)
	a, admin := m[0], m[1]
	run(t, admin, "push", "--quiet", "origin", "main:refs/heads/trunk")
	run(t, admin, "--git-dir", remote, "symbolic-ref", "HEAD", "refs/heads/trunk")
	run(t, admin, "--git-dir", remote, "symbolic-ref", "refs/heads/master", "refs/heads/main")
	run(t, a, "checkout", "--quiet", "--detach")
	run(t, a, "branch", "--quiet", "-D", "main")
	if res := mustInit(t, InitOptions{URL: remote, Dir: a, Salt: "s"}); !res.Reattached || res.Branch != "trunk" {
		t.Fatalf("init = %+v, want the clone put back on trunk, the remote's default", res)
	}
}

// A commit made on a detached HEAD, which no branch holds, stops init, which
// names the command that keeps it on a branch. Once that has run, init puts the
// clone back, and the commit stays on its branch.
func TestInitPutsACloneBackOnceItsDetachedCommitIsKept(t *testing.T) {
	t.Parallel()
	remote, m := fleet(t, 1)
	a := m[0]
	run(t, a, "checkout", "--quiet", "--detach")
	write(t, filepath.Join(a, "notes.txt"), "mine\n")
	run(t, a, "add", "notes.txt")
	run(t, a, "commit", "--quiet", "-m", "mine")
	commit := run(t, a, "rev-parse", "HEAD")
	kept := "fleetd-kept-" + commit[:12]
	_, err := Init(context.Background(), InitOptions{URL: remote, Dir: a, Salt: "s"})
	if want := "`git -C \"" + a + "\" branch " + kept + " " + commit + "`"; !errors.Is(err, ErrNoUpstream) || !strings.Contains(err.Error(), want) {
		t.Fatalf("init: %v, want ErrNoUpstream naming %s", err, want)
	}
	run(t, a, "branch", kept, commit)
	if res := mustInit(t, InitOptions{URL: remote, Dir: a, Salt: "s"}); !res.Reattached || res.Branch != "main" {
		t.Fatalf("init = %+v, want the clone put back on main", res)
	}
	if got := run(t, a, "rev-parse", kept); got != commit {
		t.Fatalf("%s is at %s, want %s", kept, got, commit)
	}
	mustSync(t, options(a, "host-a"))
}

// A branch the remote deleted while init was setting the journal up on it, once
// another machine's push had beaten init's, is not started again from nothing,
// with a salt of its own: init stops, and pushes nothing, in a clone made with
// --single-branch too. The time running out as init looks for the branch again
// reads as that, not as the branch gone.
func TestInitStopsWhenTheJournalsBranchGoesWhileItSetsItUp(t *testing.T) {
	t.Parallel()
	requireGit(t)
	for _, c := range []struct {
		name         string
		gone, single bool
	}{
		{"deleted", true, false},
		{"deleted, in a clone made with --single-branch", true, true},
		{"the time runs out", false, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			remote, m := newFleet(t, 1)
			a := m[0]
			if c.single {
				a = filepath.Join(t.TempDir(), "a")
				run(t, filepath.Dir(a), "clone", "--quiet", "--single-branch", "--branch", "main", remote, a)
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			pushed, refetched := false, false
			_, err := Init(ctx, InitOptions{URL: remote, Dir: a,
				Run: func(ctx context.Context, dir string, stdin []byte, args ...string) (string, error) {
					switch {
					case !pushed && slices.Contains(args, "push"):
						pushed = true
						// Another machine's push lands first.
						id := []string{"-c", "user.name=t", "-c", "user.email=t@example.invalid", "--git-dir", remote}
						next := run(t, a, append(id, "commit-tree", "main^{tree}", "-p", "main", "-m", "another machine")...)
						run(t, a, append(id, "update-ref", "refs/heads/main", next)...)
					case pushed && !refetched && slices.Contains(args, "fetch"):
						refetched = true
						if c.gone {
							run(t, a, "--git-dir", remote, "update-ref", "-d", "refs/heads/main")
						}
					case refetched && !c.gone && slices.Contains(args, "refs/remotes/origin/main"):
						cancel()
					}
					return Git(ctx, dir, stdin, args...)
				}})
			if !refetched {
				t.Fatalf("init did not look again: %v", err)
			}
			if c.gone && (!errors.Is(err, ErrNoUpstream) || !strings.Contains(err.Error(), "no longer has main")) {
				t.Fatalf("init: %v, want ErrNoUpstream saying the branch went", err)
			}
			if !c.gone && (!errors.Is(err, context.Canceled) || errors.Is(err, ErrNoUpstream)) {
				t.Fatalf("init: %v, want the cancellation", err)
			}
			if heads := run(t, a, "--git-dir", remote, "for-each-ref", "--format=%(refname)", "refs/heads/"); c.gone && heads != "" {
				t.Fatalf("init pushed %q", heads)
			}
		})
	}
}

// A person who names the journal's branch has init put the clone there, a new
// directory as a clone, and start an empty repository's journal on it; a name the
// repository does not have, or one that is no branch name, is refused, saying
// which branches it has, with nothing moved.
func TestInitPutsTheJournalOnTheBranchAPersonNames(t *testing.T) {
	t.Parallel()
	requireGit(t)
	t.Run("a clone, on a branch not the default", func(t *testing.T) {
		t.Parallel()
		remote, m := newFleet(t, 2)
		a, admin := m[0], m[1]
		run(t, admin, "push", "--quiet", "origin", "main:refs/heads/journal")
		res := mustInit(t, InitOptions{URL: remote, Dir: a, Salt: "s", Branch: "journal"})
		if !res.Reattached || res.Branch != "journal" {
			t.Fatalf("init = %+v, want the clone put on journal", res)
		}
		appendLines(t, filepath.Join(a, "host-a.jsonl"), `{"id":"hive:a1"}`)
		mustSync(t, options(a, "host-a"))
		if got := run(t, a, "--git-dir", remote, "show", "journal:host-a.jsonl"); got != `{"id":"hive:a1"}` {
			t.Fatalf("journal holds %q for host-a", got)
		}
	})
	t.Run("a new directory, on a branch not the default", func(t *testing.T) {
		t.Parallel()
		remote, m := newFleet(t, 1)
		run(t, m[0], "push", "--quiet", "origin", "main:refs/heads/journal")
		dir := filepath.Join(t.TempDir(), "journal")
		if res := mustInit(t, InitOptions{URL: remote, Dir: dir, Salt: "s", Branch: "journal"}); !res.Cloned || res.Branch != "journal" {
			t.Fatalf("init = %+v, want the directory set up on journal", res)
		}
		if up := run(t, dir, "rev-parse", "--abbrev-ref", "@{upstream}"); up != "origin/journal" {
			t.Fatalf("the clone follows %s", up)
		}
	})
	t.Run("an empty repository, started on the branch named", func(t *testing.T) {
		t.Parallel()
		remote := newEmptyRemote(t)
		dir := filepath.Join(t.TempDir(), "journal")
		if res := mustInit(t, InitOptions{URL: remote, Dir: dir, Salt: "s", Branch: "journal"}); !res.Started || res.Branch != "journal" {
			t.Fatalf("init = %+v, want the journal started on journal", res)
		}
		if heads := run(t, dir, "--git-dir", remote, "for-each-ref", "--format=%(refname)", "refs/heads/"); heads != "refs/heads/journal" {
			t.Fatalf("the remote has %q", heads)
		}
	})
	t.Run("an empty repository, named -x", func(t *testing.T) {
		t.Parallel()
		remote := newEmptyRemote(t)
		dir := filepath.Join(t.TempDir(), "journal")
		if _, err := Init(context.Background(), InitOptions{URL: remote, Dir: dir, Salt: "s", Branch: "-x"}); !errors.Is(err, ErrNoBranch) {
			t.Fatalf("init: %v, want ErrNoBranch", err)
		}
		if heads := run(t, filepath.Dir(remote), "--git-dir", remote, "for-each-ref", "--format=%(refname)", "refs/heads/"); heads != "" {
			t.Fatalf("init pushed %q", heads)
		}
	})
	for _, name := range []string{"trunk", "-x"} {
		t.Run("a clone, named "+name, func(t *testing.T) {
			t.Parallel()
			remote, m := newFleet(t, 1)
			a := m[0]
			run(t, a, "checkout", "--quiet", "--detach")
			before := clonedState(t, a)
			_, err := Init(context.Background(), InitOptions{URL: remote, Dir: a, Salt: "s", Branch: name})
			if !errors.Is(err, ErrNoBranch) || name == "trunk" && !strings.Contains(err.Error(), "it has main") {
				t.Fatalf("init: %v, want ErrNoBranch", err)
			}
			if after := clonedState(t, a); after != before {
				t.Fatalf("init moved things in a clone it refused:\nbefore:\n%s\nafter:\n%s", before, after)
			}
		})
		t.Run("a new directory, named "+name, func(t *testing.T) {
			t.Parallel()
			remote, _ := newFleet(t, 1)
			dir := filepath.Join(t.TempDir(), "journal")
			if _, err := Init(context.Background(), InitOptions{URL: remote, Dir: dir, Salt: "s", Branch: name}); !errors.Is(err, ErrNoBranch) {
				t.Fatalf("init: %v, want ErrNoBranch", err)
			}
			if _, err := os.Lstat(filepath.Join(dir, ".git")); err == nil {
				t.Fatal("init set the directory up")
			}
		})
	}
}

// Init starts no journal, with a salt of its own, on a branch that holds none
// while another of the repository's branches holds the journal, since the fleet
// publishes there: a new machine whose repository's default holds only a README,
// a clone the repair would put on that default, a clone told that branch by
// mistake, and one made of it by hand are each refused, naming the branch the
// journal is on, with nothing pushed and nothing moved; the advice followed puts
// each on the journal's branch.
func TestInitStartsNoJournalBesideOneOnAnotherBranch(t *testing.T) {
	t.Parallel()
	requireGit(t)
	// besideReadme returns a remote whose journal is on main, with fleetd.json
	// holding salt s unless v010, as fleetd v0.1.0 left one, and whose default is
	// docs, a branch holding a README; and the clone of machine a, which synced
	// a record there.
	besideReadme := func(t *testing.T, v010 bool) (remote, a string) {
		remote, m := newFleet(t, 1)
		a = m[0]
		if !v010 {
			mustInit(t, InitOptions{URL: remote, Dir: a, Salt: "s"})
		}
		appendLines(t, filepath.Join(a, "host-a.jsonl"), `{"id":"hive:a1"}`)
		mustSync(t, options(a, "host-a"))
		docs := filepath.Join(t.TempDir(), "docs")
		run(t, filepath.Dir(docs), "init", "--quiet", "--initial-branch=docs", docs)
		identify(t, docs)
		write(t, filepath.Join(docs, "README.md"), "about this repository\n")
		run(t, docs, "add", ".")
		run(t, docs, "commit", "--quiet", "-m", "docs")
		run(t, docs, "push", "--quiet", remote, "docs")
		run(t, filepath.Dir(remote), "--git-dir", remote, "symbolic-ref", "HEAD", "refs/heads/docs")
		run(t, a, "fetch", "--quiet", "origin")
		return remote, a
	}
	heads := func(t *testing.T, remote string) string {
		return run(t, filepath.Dir(remote), "--git-dir", remote, "for-each-ref", "--format=%(refname) %(objectname)", "refs/heads/")
	}
	// refused checks the refusal, that it names the journal's branch and the way
	// on, and that the remote's branches are as they were.
	refused := func(t *testing.T, err error, remote, dir, before string) {
		t.Helper()
		if !errors.Is(err, ErrJournalElsewhere) || !strings.Contains(err.Error(), "holds no journal on docs, but holds one on main") {
			t.Fatalf("init: %v, want ErrJournalElsewhere naming main", err)
		}
		if want := "`fleetd init --dir \"" + dir + "\" --branch <branch> <journal URL>`"; !strings.Contains(err.Error(), want) {
			t.Fatalf("init: %v, want it to name %s", err, want)
		}
		if after := heads(t, remote); after != before {
			t.Fatalf("init pushed:\nbefore:\n%s\nafter:\n%s", before, after)
		}
	}
	for _, v010 := range []bool{false, true} {
		name := "a new directory"
		if v010 {
			name += ", beside a journal of fleetd v0.1.0's"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			remote, _ := besideReadme(t, v010)
			before := heads(t, remote)
			dir := filepath.Join(t.TempDir(), "journal")
			_, err := Init(context.Background(), InitOptions{URL: remote, Dir: dir, Salt: "s"})
			refused(t, err, remote, dir, before)
			if _, err := os.Lstat(filepath.Join(dir, ".git")); err == nil {
				t.Fatal("init set the directory up")
			}
			res := mustInit(t, InitOptions{URL: remote, Dir: dir, Salt: "s", Branch: "main"})
			if !res.Cloned || res.Branch != "main" || res.Salt != "s" {
				t.Fatalf("init = %+v, want the directory set up on main, with salt s", res)
			}
		})
	}
	for _, c := range []struct {
		name  string
		named string
		off   [][]string
	}{
		{"a clone with no branch of its own, set up before init recorded the journal's branch", "", [][]string{
			{"checkout", "--quiet", "--detach"}, {"branch", "--quiet", "-D", "main"},
		}},
		{"a clone told the wrong branch", "docs", nil},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			remote, a := besideReadme(t, false)
			for _, args := range c.off {
				run(t, a, args...)
			}
			if c.named == "" {
				unrecord(t, a)
			}
			before, state := heads(t, remote), clonedState(t, a)
			_, err := Init(context.Background(), InitOptions{URL: remote, Dir: a, Branch: c.named})
			refused(t, err, remote, a, before)
			if after := clonedState(t, a); after != state {
				t.Fatalf("init moved things in a clone it refused:\nbefore:\n%s\nafter:\n%s", state, after)
			}
			appendLines(t, filepath.Join(a, "host-a.jsonl"), `{"id":"hive:a2"}`)
			if res := mustInit(t, InitOptions{URL: remote, Dir: a, Branch: "main"}); res.Branch != "main" || res.Salt != "s" {
				t.Fatalf("init = %+v, want the clone on main, with salt s", res)
			}
			mustSync(t, options(a, "host-a"))
			if got := remoteFile(t, remote, "host-a.jsonl"); got != "{\"id\":\"hive:a1\"}\n{\"id\":\"hive:a2\"}\n" {
				t.Fatalf("main holds %q for host-a", got)
			}
		})
	}
	t.Run("a new directory, beside two branches holding the journal", func(t *testing.T) {
		t.Parallel()
		remote, a := besideReadme(t, false)
		run(t, a, "push", "--quiet", "origin", "main:refs/heads/stray")
		before := heads(t, remote)
		dir := filepath.Join(t.TempDir(), "journal")
		_, err := Init(context.Background(), InitOptions{URL: remote, Dir: dir})
		if !errors.Is(err, ErrJournalElsewhere) || !strings.Contains(err.Error(), "holds no journal on docs, but holds one on each of main, stray") {
			t.Fatalf("init: %v, want ErrJournalElsewhere naming main and stray", err)
		}
		if after := heads(t, remote); after != before {
			t.Fatalf("init pushed:\nbefore:\n%s\nafter:\n%s", before, after)
		}
	})
	t.Run("a clone of the default, made by hand", func(t *testing.T) {
		t.Parallel()
		remote, _ := besideReadme(t, false)
		dir := filepath.Join(t.TempDir(), "c")
		run(t, filepath.Dir(dir), "clone", "--quiet", remote, dir)
		before := heads(t, remote)
		_, err := Init(context.Background(), InitOptions{URL: remote, Dir: dir})
		refused(t, err, remote, dir, before)
		if res := mustInit(t, InitOptions{URL: remote, Dir: dir, Branch: "main"}); !res.Reattached || res.Branch != "main" || res.Salt != "s" {
			t.Fatalf("init = %+v, want the clone put on main, with salt s", res)
		}
	})
}

// Init records the branch it puts the journal on, and that is the journal's
// branch from then on: a clone off it again is put back there, not on the
// remote's default, which an earlier split may have left holding a journal with
// a salt of its own; a sync on a clone a person switched to another branch is
// refused, and init puts it back; and a recorded branch the remote deleted is
// left for a person, with advice that works.
func TestInitRemembersTheBranchItPutsTheJournalOn(t *testing.T) {
	t.Parallel()
	requireGit(t)
	// splitRemote returns a remote whose fleet publishes to journal, with salt s,
	// while its default, main, holds a journal of its own with salt x, as an
	// earlier split left one; and machine a, which synced a record to journal.
	splitRemote := func(t *testing.T) (remote, a string) {
		remote, m := newFleet(t, 1)
		a = m[0]
		mustInit(t, InitOptions{URL: remote, Dir: a, Salt: "s"})
		appendLines(t, filepath.Join(a, "host-a.jsonl"), `{"id":"hive:a1"}`)
		mustSync(t, options(a, "host-a"))
		run(t, filepath.Dir(remote), "--git-dir", remote, "branch", "-m", "main", "journal")
		apart := filepath.Join(t.TempDir(), "apart")
		run(t, filepath.Dir(apart), "init", "--quiet", "--initial-branch=main", apart)
		identify(t, apart)
		write(t, filepath.Join(apart, FleetFile), "{\"salt\": \"x\"}\n")
		appendLines(t, filepath.Join(apart, "host-z.jsonl"), `{"id":"hive:z1"}`)
		run(t, apart, "add", ".")
		run(t, apart, "commit", "--quiet", "-m", "a journal started apart")
		run(t, apart, "push", "--quiet", remote, "main")
		run(t, filepath.Dir(remote), "--git-dir", remote, "symbolic-ref", "HEAD", "refs/heads/main")
		return remote, a
	}
	onJournal := func(t *testing.T, remote, dir, host string, want string) {
		t.Helper()
		appendLines(t, filepath.Join(dir, host+".jsonl"), `{"id":"hive:later"}`)
		mustSync(t, options(dir, host))
		if got := run(t, dir, "--git-dir", remote, "show", "journal:"+host+".jsonl"); got != want {
			t.Fatalf("journal holds %q for %s, want %q", got, host, want)
		}
		if out, err := exec.Command("git", "--git-dir", remote, "show", "main:"+host+".jsonl").Output(); err == nil {
			t.Fatalf("main holds %q for %s", out, host)
		}
	}
	// offAndBack detaches the clone, as a person looking at its history does,
	// and follows sync's advice, a plain init.
	offAndBack := func(t *testing.T, remote, dir string) {
		t.Helper()
		run(t, dir, "checkout", "--quiet", "--detach")
		if _, err := Sync(context.Background(), options(dir, "host-c")); !errors.Is(err, ErrNoUpstream) {
			t.Fatalf("sync on a detached clone: %v, want ErrNoUpstream", err)
		}
		if res := mustInit(t, InitOptions{URL: remote, Dir: dir}); res.Branch != "journal" || res.Salt != "s" {
			t.Fatalf("init = %+v, want the clone back on journal, with salt s", res)
		}
	}
	t.Run("a new directory set up on the branch named", func(t *testing.T) {
		t.Parallel()
		remote, _ := splitRemote(t)
		c := filepath.Join(t.TempDir(), "c")
		if res := mustInit(t, InitOptions{URL: remote, Dir: c, Branch: "journal"}); res.Branch != "journal" || res.Salt != "s" {
			t.Fatalf("init = %+v, want the directory set up on journal, with salt s", res)
		}
		offAndBack(t, remote, c)
		onJournal(t, remote, c, "host-c", `{"id":"hive:later"}`)
	})
	t.Run("a clone put on the branch named", func(t *testing.T) {
		t.Parallel()
		remote, a := splitRemote(t)
		run(t, a, "fetch", "--quiet", "--prune", "origin")
		run(t, a, "checkout", "--quiet", "-B", "main", "origin/main")
		unrecord(t, a)
		if res := mustInit(t, InitOptions{URL: remote, Dir: a, Branch: "journal"}); res.Branch != "journal" {
			t.Fatalf("init = %+v, want the clone put on journal", res)
		}
		offAndBack(t, remote, a)
		onJournal(t, remote, a, "host-a", "{\"id\":\"hive:a1\"}\n{\"id\":\"hive:later\"}")
	})
	t.Run("a clone a person switched to another branch", func(t *testing.T) {
		t.Parallel()
		remote, a := splitRemote(t)
		c := filepath.Join(t.TempDir(), "c")
		mustInit(t, InitOptions{URL: remote, Dir: c, Branch: "journal"})
		run(t, c, "switch", "--quiet", "main")
		appendLines(t, filepath.Join(c, "host-c.jsonl"), `{"id":"hive:c1"}`)
		_, err := Sync(context.Background(), options(c, "host-c"))
		if !errors.Is(err, ErrNoUpstream) || !strings.Contains(err.Error(), "it follows origin/main, while the journal is on journal") {
			t.Fatalf("sync on a clone switched to main: %v, want ErrNoUpstream naming both branches", err)
		}
		if want := "`fleetd init --dir \"" + c + "\" <journal URL>` puts it back there"; !strings.Contains(err.Error(), want) {
			t.Fatalf("sync: %v, want it to say %s", err, want)
		}
		if res := mustInit(t, InitOptions{URL: remote, Dir: c}); !res.Reattached || res.Branch != "journal" {
			t.Fatalf("init = %+v, want the clone put back on journal", res)
		}
		onJournal(t, remote, c, "host-c", "{\"id\":\"hive:c1\"}\n{\"id\":\"hive:later\"}")
		_ = a
	})
	t.Run("a repair cut short before HEAD moves", func(t *testing.T) {
		t.Parallel()
		remote, a := splitRemote(t)
		run(t, a, "fetch", "--quiet", "--prune", "origin")
		run(t, a, "checkout", "--quiet", "-B", "main", "origin/main")
		_, err := Init(context.Background(), InitOptions{URL: remote, Dir: a, Branch: "journal",
			Run: func(ctx context.Context, dir string, stdin []byte, args ...string) (string, error) {
				if slices.Equal(args, []string{"symbolic-ref", "HEAD", "refs/heads/journal"}) {
					return "", errors.New("cut short")
				}
				return Git(ctx, dir, stdin, args...)
			}})
		if err == nil || !strings.Contains(err.Error(), "cut short") {
			t.Fatalf("init: %v, want it cut short", err)
		}
		appendLines(t, filepath.Join(a, "host-a.jsonl"), `{"id":"hive:a2"}`)
		if _, err := Sync(context.Background(), options(a, "host-a")); !errors.Is(err, ErrNoUpstream) {
			t.Fatalf("sync after a repair cut short: %v, want ErrNoUpstream", err)
		}
		if res := mustInit(t, InitOptions{URL: remote, Dir: a}); !res.Reattached || res.Branch != "journal" {
			t.Fatalf("init = %+v, want the repair finished, on journal", res)
		}
	})
	t.Run("a clone set up before init recorded the journal's branch", func(t *testing.T) {
		t.Parallel()
		remote, a := splitRemote(t)
		run(t, a, "fetch", "--quiet", "--prune", "origin")
		run(t, a, "branch", "--quiet", "-m", "journal")
		run(t, a, "branch", "--quiet", "--set-upstream-to", "origin/journal")
		unrecord(t, a)
		if res := mustInit(t, InitOptions{URL: remote, Dir: a}); res.Branch != "journal" {
			t.Fatalf("init = %+v, want the clone left on journal", res)
		}
		run(t, a, "switch", "--quiet", "-c", "main", "--track", "origin/main")
		if _, err := Sync(context.Background(), options(a, "host-a")); !errors.Is(err, ErrNoUpstream) {
			t.Fatalf("sync on a clone switched to main: %v, want ErrNoUpstream", err)
		}
	})
	t.Run("a stray branch the clone was switched to, deleted", func(t *testing.T) {
		t.Parallel()
		remote, _ := splitRemote(t)
		c := filepath.Join(t.TempDir(), "c")
		mustInit(t, InitOptions{URL: remote, Dir: c, Branch: "journal"})
		run(t, c, "push", "--quiet", "origin", "journal:refs/heads/stray")
		run(t, c, "fetch", "--quiet", "origin")
		run(t, c, "switch", "--quiet", "-c", "stray", "--track", "origin/stray")
		run(t, c, "push", "--quiet", "origin", ":refs/heads/stray")
		if res := mustInit(t, InitOptions{URL: remote, Dir: c}); !res.Reattached || res.Branch != "journal" {
			t.Fatalf("init = %+v, want the clone put back on journal", res)
		}
		if out, err := exec.Command("git", "--git-dir", remote, "rev-parse", "--verify", "--quiet", "refs/heads/stray").Output(); err == nil {
			t.Fatalf("stray came back, at %s", out)
		}
		onJournal(t, remote, c, "host-c", `{"id":"hive:later"}`)
	})
	t.Run("a recorded branch the remote deleted", func(t *testing.T) {
		t.Parallel()
		remote, _ := splitRemote(t)
		c := filepath.Join(t.TempDir(), "c")
		mustInit(t, InitOptions{URL: remote, Dir: c, Branch: "journal"})
		run(t, filepath.Dir(remote), "--git-dir", remote, "branch", "-m", "journal", "trunk")
		run(t, c, "checkout", "--quiet", "--detach")
		run(t, c, "fetch", "--quiet", "--prune", "origin")
		before := clonedState(t, c)
		_, err := Init(context.Background(), InitOptions{URL: remote, Dir: c})
		if !errors.Is(err, ErrNoUpstream) || !strings.Contains(err.Error(), "journal is the journal's branch, the one init set this clone up on") {
			t.Fatalf("init: %v, want ErrNoUpstream saying the recorded branch is gone", err)
		}
		for _, want := range []string{"`fleetd init --dir \"" + c + "\" --branch <branch> <journal URL>`",
			"`git -C \"" + c + "\" push origin refs/heads/<local>:refs/heads/<branch>`, with journal for <local> and journal for <branch>"} {
			if !strings.Contains(err.Error(), want) {
				t.Fatalf("init: %v, want it to say %s", err, want)
			}
		}
		if after := clonedState(t, c); after != before {
			t.Fatalf("init moved things in a clone it refused:\nbefore:\n%s\nafter:\n%s", before, after)
		}
		if res := mustInit(t, InitOptions{URL: remote, Dir: c, Branch: "trunk"}); res.Branch != "trunk" || res.Salt != "s" {
			t.Fatalf("init = %+v, want the clone put on trunk", res)
		}
	})
}

// A salt given with --salt that differs from the journal's on the branch named
// is refused before the clone moves onto that branch.
func TestInitRefusesAStrictSaltBeforeMovingTheClone(t *testing.T) {
	t.Parallel()
	remote, m := fleet(t, 1)
	a := m[0]
	mustInit(t, InitOptions{URL: remote, Dir: a, Salt: "s"})
	apart := filepath.Join(t.TempDir(), "apart")
	run(t, filepath.Dir(apart), "init", "--quiet", "--initial-branch=docs", apart)
	identify(t, apart)
	write(t, filepath.Join(apart, FleetFile), "{\"salt\": \"other\"}\n")
	run(t, apart, "add", ".")
	run(t, apart, "commit", "--quiet", "-m", "another fleet's journal")
	run(t, apart, "push", "--quiet", remote, "docs")
	run(t, a, "fetch", "--quiet", "origin")
	before := clonedState(t, a)
	_, err := Init(context.Background(), InitOptions{URL: remote, Dir: a, Salt: "s", Strict: true, Branch: "docs"})
	if !errors.Is(err, ErrSaltMismatch) {
		t.Fatalf("init: %v, want ErrSaltMismatch", err)
	}
	if after := clonedState(t, a); after != before {
		t.Fatalf("init moved things in a clone it refused:\nbefore:\n%s\nafter:\n%s", before, after)
	}
}

// The advice for a clone beside a repository with no branch keeps the branch a
// person named, so that following it starts the journal there.
func TestInitBesideAnEmptyRepositoryKeepsTheBranchNamedInItsAdvice(t *testing.T) {
	t.Parallel()
	requireGit(t)
	t.Run("a clone set up before the repository was emptied", func(t *testing.T) {
		t.Parallel()
		remote, m := newFleet(t, 1)
		a := m[0]
		mustInit(t, InitOptions{URL: remote, Dir: a, Salt: "s"})
		run(t, a, "--git-dir", remote, "update-ref", "-d", "refs/heads/main")
		want := "out of it, then run fleetd init again with the same --branch, which starts the journal there"
		_, err := Init(context.Background(), InitOptions{URL: remote, Dir: a, Salt: "s", Branch: "journal"})
		if !errors.Is(err, ErrNoUpstream) || !strings.Contains(err.Error(), want) {
			t.Fatalf("init: %v, want ErrNoUpstream saying %s", err, want)
		}
	})
	for _, c := range []struct {
		name   string
		commit bool
		want   string
	}{
		{"a clone with no commit", false, "delete its .git directory, then run fleetd init again with the same --branch, which sets it up"},
		{"a clone with commits of its own", true, "out of it, then run fleetd init again with the same --branch, which starts the journal there"},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			remote := newEmptyRemote(t)
			dir := filepath.Join(t.TempDir(), "journal")
			run(t, filepath.Dir(dir), "init", "--quiet", "--initial-branch=main", dir)
			identify(t, dir)
			if c.commit {
				write(t, filepath.Join(dir, "notes.txt"), "mine\n")
				run(t, dir, "add", "notes.txt")
				run(t, dir, "commit", "--quiet", "-m", "mine")
			}
			run(t, dir, "remote", "add", "origin", remote)
			_, err := Init(context.Background(), InitOptions{URL: remote, Dir: dir, Salt: "s", Branch: "journal"})
			if !errors.Is(err, ErrNoUpstream) || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("init: %v, want ErrNoUpstream saying %s", err, c.want)
			}
		})
	}
}

// A clone whose fetch configuration takes origin's branches somewhere else
// first, which git then takes for the branch's upstream, so that every sync says
// to run init, is put right by init, and syncs; one that does so from a file its
// config includes, which init does not rewrite, is refused, saying so; and one
// narrowed to the journal's branch, which works, is left as it is.
func TestInitPutsRightAFetchThatTakesOriginsBranchesElsewhere(t *testing.T) {
	t.Parallel()
	requireGit(t)
	t.Run("in the clone's own config", func(t *testing.T) {
		t.Parallel()
		remote, m := newFleet(t, 1)
		a := m[0]
		run(t, a, "config", "--unset-all", "remote.origin.fetch")
		run(t, a, "config", "--add", "remote.origin.fetch", "+refs/heads/*:refs/remotes/upstream/*")
		run(t, a, "config", "--add", "remote.origin.fetch", "+refs/heads/*:refs/remotes/origin/*")
		run(t, a, "fetch", "--quiet", "origin")
		appendLines(t, filepath.Join(a, "host-a.jsonl"), `{"id":"hive:a1"}`)
		if _, err := Sync(context.Background(), options(a, "host-a")); !errors.Is(err, ErrNoUpstream) {
			t.Fatalf("sync: %v, want ErrNoUpstream", err)
		}
		mustInit(t, InitOptions{URL: remote, Dir: a, Salt: "s"})
		mustSync(t, options(a, "host-a"))
		if got := remoteFile(t, remote, "host-a.jsonl"); got != "{\"id\":\"hive:a1\"}\n" {
			t.Fatalf("main holds %q for host-a", got)
		}
	})
	t.Run("in a file the clone's config includes", func(t *testing.T) {
		t.Parallel()
		remote, m := newFleet(t, 1)
		a := m[0]
		inc := filepath.Join(t.TempDir(), "upstream.cfg")
		write(t, inc, "[remote \"origin\"]\n\tfetch = +refs/heads/*:refs/remotes/upstream/*\n")
		cfg := filepath.Join(a, ".git", "config")
		write(t, cfg, "[include]\n\tpath = "+filepath.ToSlash(inc)+"\n"+readFile(t, cfg))
		run(t, a, "fetch", "--quiet", "origin")
		_, err := Init(context.Background(), InitOptions{URL: remote, Dir: a, Salt: "s"})
		if !errors.Is(err, ErrNoUpstream) || !strings.Contains(err.Error(), "for remote.origin.fetch or for the branch") {
			t.Fatalf("init: %v, want ErrNoUpstream naming the included line", err)
		}
	})
	t.Run("the branch's remote in a file the clone's config includes", func(t *testing.T) {
		t.Parallel()
		remote, m := newFleet(t, 1)
		a := m[0]
		run(t, a, "remote", "add", "backup", remote)
		run(t, a, "fetch", "--quiet", "backup")
		inc := filepath.Join(t.TempDir(), "branch.cfg")
		write(t, inc, "[branch \"main\"]\n\tremote = backup\n")
		run(t, a, "config", "--add", "include.path", filepath.ToSlash(inc))
		run(t, a, "checkout", "--quiet", "--detach")
		_, err := Init(context.Background(), InitOptions{URL: remote, Dir: a, Salt: "s"})
		if !errors.Is(err, ErrNoUpstream) || !strings.Contains(err.Error(), "for remote.origin.fetch or for the branch") ||
			!strings.Contains(err.Error(), "config --show-origin --list") {
			t.Fatalf("init: %v, want ErrNoUpstream naming the included line, for the fetch or the branch", err)
		}
	})
	t.Run("a branch named @", func(t *testing.T) {
		t.Parallel()
		remote := newEmptyRemote(t)
		dir := filepath.Join(t.TempDir(), "journal")
		mustInit(t, InitOptions{URL: remote, Dir: dir, Salt: "s", Branch: "@"})
		run(t, dir, "checkout", "--quiet", "--detach")
		if res := mustInit(t, InitOptions{URL: remote, Dir: dir}); !res.Reattached || res.Branch != "@" {
			t.Fatalf("init = %+v, want the clone put back on @", res)
		}
		appendLines(t, filepath.Join(dir, "host-a.jsonl"), `{"id":"hive:a1"}`)
		mustSync(t, options(dir, "host-a"))
	})
	t.Run("narrowed to the journal's branch", func(t *testing.T) {
		t.Parallel()
		remote, m := newFleet(t, 1)
		a := m[0]
		run(t, a, "config", "--replace-all", "remote.origin.fetch", "+refs/heads/main:refs/remotes/origin/main")
		run(t, a, "checkout", "--quiet", "--detach")
		if res := mustInit(t, InitOptions{URL: remote, Dir: a, Salt: "s"}); !res.Reattached || res.Branch != "main" {
			t.Fatalf("init = %+v, want the clone put back on main", res)
		}
		if got := run(t, a, "config", "--get-all", "remote.origin.fetch"); got != "+refs/heads/main:refs/remotes/origin/main" {
			t.Fatalf("the clone fetches %q", got)
		}
	})
}

// A record of the journal's branch that cannot be read, a directory put where
// the file goes, say, stops sync and init alike, saying what to do, and is never
// taken for no record, which would let sync publish wherever the clone points;
// the advice followed works.
func TestARecordOfTheJournalsBranchThatCannotBeReadStopsSyncAndInit(t *testing.T) {
	t.Parallel()
	remote, m := fleet(t, 1)
	a := m[0]
	mustInit(t, InitOptions{URL: remote, Dir: a, Salt: "s"})
	record := filepath.Join(a, ".git", branchName)
	if got := strings.TrimSpace(readFile(t, record)); got != "main" {
		t.Fatalf("init recorded %q, want main", got)
	}
	if err := os.Remove(record); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(record, 0o755); err != nil {
		t.Fatal(err)
	}
	appendLines(t, filepath.Join(a, "host-a.jsonl"), `{"id":"hive:a1"}`)
	for _, step := range []string{"sync", "init"} {
		var err error
		if step == "sync" {
			_, err = Sync(context.Background(), options(a, "host-a"))
		} else {
			_, err = Init(context.Background(), InitOptions{URL: remote, Dir: a})
		}
		// The path is the one git gives for the git directory, which on Windows
		// can be the long form of a short temporary path.
		if err == nil || !strings.Contains(err.Error(), branchName+", init's record of the journal's branch, cannot be read") ||
			!strings.Contains(err.Error(), "delete it, then run fleetd init again") {
			t.Fatalf("%s: %v, want it to say the record cannot be read", step, err)
		}
	}
	if err := os.Remove(record); err != nil {
		t.Fatal(err)
	}
	if res := mustInit(t, InitOptions{URL: remote, Dir: a}); res.Branch != "main" {
		t.Fatalf("init = %+v, want the clone on main", res)
	}
	mustSync(t, options(a, "host-a"))
	if got := remoteFile(t, remote, "host-a.jsonl"); got != "{\"id\":\"hive:a1\"}\n" {
		t.Fatalf("main holds %q for host-a", got)
	}
}

// The recorded branch, deleted from origin by mistake, is pushed back as init
// says on a clone off it, switched to another branch or detached: never this
// clone's HEAD, which is not the journal's, but the branch here that followed
// the recorded one; with it back, init puts the clone on it again, keeping the
// journal's salt, and the other machine's next sync takes it up again.
func TestARecordedBranchGoneIsPushedBackFromTheBranchThatHeldIt(t *testing.T) {
	t.Parallel()
	requireGit(t)
	for _, c := range []struct {
		name string
		off  func(t *testing.T, remote, a string)
	}{
		{"switched to a branch holding a README", func(t *testing.T, remote, a string) {
			docs := filepath.Join(t.TempDir(), "docs")
			run(t, filepath.Dir(docs), "init", "--quiet", "--initial-branch=docs", docs)
			identify(t, docs)
			write(t, filepath.Join(docs, "README.md"), "about this repository\n")
			run(t, docs, "add", ".")
			run(t, docs, "commit", "--quiet", "-m", "docs")
			run(t, docs, "push", "--quiet", remote, "docs")
			run(t, a, "fetch", "--quiet", "origin")
			run(t, a, "switch", "--quiet", "docs")
		}},
		{"detached", func(t *testing.T, remote, a string) {
			run(t, a, "checkout", "--quiet", "--detach")
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			remote, m := newFleet(t, 2)
			a, b := m[0], m[1]
			mustInit(t, InitOptions{URL: remote, Dir: a, Salt: "s"})
			mustInit(t, InitOptions{URL: remote, Dir: b, Salt: "s"})
			appendLines(t, filepath.Join(b, "host-b.jsonl"), `{"id":"hive:b1"}`)
			mustSync(t, options(b, "host-b"))
			appendLines(t, filepath.Join(a, "host-a.jsonl"), `{"id":"hive:a1"}`)
			mustSync(t, options(a, "host-a"))
			c.off(t, remote, a)
			run(t, a, "--git-dir", remote, "update-ref", "-d", "refs/heads/main")
			_, err := Init(context.Background(), InitOptions{URL: remote, Dir: a})
			if !errors.Is(err, ErrNoUpstream) || strings.Contains(err.Error(), "push origin HEAD") {
				t.Fatalf("init: %v, want ErrNoUpstream, never saying to push this clone's HEAD", err)
			}
			want := "`git -C \"" + a + "\" push origin refs/heads/<local>:refs/heads/<branch>`, with main for <local> and main for <branch>"
			if !strings.Contains(err.Error(), want) {
				t.Fatalf("init: %v, want it to say %s", err, want)
			}
			run(t, a, "push", "--quiet", "origin", "refs/heads/main:refs/heads/main")
			if res := mustInit(t, InitOptions{URL: remote, Dir: a}); res.Branch != "main" || res.Salt != "s" || res.WroteFleetFile {
				t.Fatalf("init = %+v, want the clone back on main, with salt s", res)
			}
			appendLines(t, filepath.Join(b, "host-b.jsonl"), `{"id":"hive:b2"}`)
			mustSync(t, options(b, "host-b"))
			if got := remoteFile(t, remote, "host-b.jsonl"); got != "{\"id\":\"hive:b1\"}\n{\"id\":\"hive:b2\"}\n" {
				t.Fatalf("main holds %q for host-b", got)
			}
			if got := remoteFile(t, remote, "host-a.jsonl"); got != "{\"id\":\"hive:a1\"}\n" {
				t.Fatalf("main holds %q for host-a", got)
			}
		})
	}
}

// Two machines adding fleetd.json at once to two branches that hold no journal,
// a README each, would each find no journal before pushing; the one that looks
// again after its push finds the other's, and says the journal was started on
// two branches at once.
func TestTwoMachinesAddingTheJournalToTwoBranchesAtOnceAreToldSo(t *testing.T) {
	t.Parallel()
	requireGit(t)
	remote := newEmptyRemote(t)
	w := filepath.Join(t.TempDir(), "w")
	run(t, filepath.Dir(w), "init", "--quiet", "--initial-branch=main", w)
	identify(t, w)
	write(t, filepath.Join(w, "README.md"), "about\n")
	run(t, w, "add", ".")
	run(t, w, "commit", "--quiet", "-m", "readme")
	run(t, w, "push", "--quiet", remote, "main", "main:docs")
	a, b := filepath.Join(t.TempDir(), "a"), filepath.Join(t.TempDir(), "b")
	raced := false
	var errB error
	_, errA := Init(context.Background(), InitOptions{URL: remote, Dir: a, Salt: "a",
		Run: func(ctx context.Context, dir string, stdin []byte, args ...string) (string, error) {
			if !raced && slices.Contains(args, "push") {
				raced = true
				_, errB = Init(ctx, InitOptions{URL: remote, Dir: b, Salt: "b", Branch: "docs"})
			}
			return Git(ctx, dir, stdin, args...)
		}})
	if !raced || errB != nil {
		t.Fatalf("raced %v, the other machine's init: %v", raced, errB)
	}
	if errA == nil || !strings.Contains(errA.Error(), "the journal was started on two branches at once, main and docs") {
		t.Fatalf("init: %v, want it told the journal was started on two branches at once", errA)
	}
}
