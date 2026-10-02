package gitsync

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
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
