package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/BFlinkDesign/agent-comms/internal/gitsync"
)

// cmdInit sets up this machine's journal: the journal directory becomes a clone
// of the journal repository, with the fleet's salt in it. Records written there
// before init stay where they are; the sync init ends with publishes them, and
// files those this machine wrote under another identity under its fleet one.
func cmdInit(args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("init", flag.ContinueOnError)
	fs.SetOutput(stderr)
	asJSON := fs.Bool("json", false, "emit JSON")
	dir := fs.String("dir", "", "journal directory (else $COMMS_CHANNELS/journal, else ~/.ai/channels/journal)")
	salt := fs.String("salt", "", "fleet salt for a journal that has none yet (else $FLEET_SALT, else a random one for a journal with no records)")
	reclaim := fs.Bool("reclaim", false, "put this machine's published records back at the start of its journal file, "+
		"after its journal directory was set up again or restored from an older copy")
	timeout := fs.Duration("timeout", 2*time.Minute, "give up after this long")
	branch := fs.String("branch", "", "the journal's branch, where init cannot tell which it is (else the one init "+
		"recorded, else the branch the clone follows, else the remote's default); init records it")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return errors.New("init takes one argument: the journal repository's URL")
	}
	if *timeout <= 0 {
		return errors.New("--timeout must be positive")
	}
	url := fs.Arg(0)
	given, from := *salt, "--salt"
	if given == "" {
		given, from = os.Getenv("FLEET_SALT"), "FLEET_SALT"
	}
	journalDir, err := resolveDir(*dir)
	if err != nil {
		return err
	}
	if journalDir, err = filepath.Abs(journalDir); err != nil {
		return err
	}

	deadline := time.Now().Add(*timeout)
	ctx, cancel := context.WithDeadline(context.Background(), deadline)
	defer cancel()
	res, err := gitsync.Init(ctx, gitsync.InitOptions{
		URL: url, Dir: journalDir, Salt: given, Strict: from == "--salt" && given != "", Branch: *branch,
	})
	switch {
	case errors.Is(err, gitsync.ErrSaltMismatch) && from == "--salt" && *branch != "":
		// The branch named may not be the fleet's at all.
		return fmt.Errorf("%w: --salt differs from the salt in %s's %s on %s; with it this machine would get a "+
			"second host id. If %s holds the fleet's journal, unset --salt, or set it to that journal's; if not, name "+
			"the branch that does with --branch", gitsync.ErrSaltMismatch, url, gitsync.FleetFile, *branch, *branch)
	case errors.Is(err, gitsync.ErrSaltMismatch) && from == "--salt":
		return saltMismatch(from, url)
	case errors.Is(err, gitsync.ErrNeedSalt):
		return fmt.Errorf("%w. Give init the salt its machines record with, with --salt or FLEET_SALT, so they keep "+
			"their host ids. If they ran without one, as fleetd v0.1.0 allowed, give a new salt: the records they "+
			"published stay under the ids they had, and the rest, with every new one, go under new ones once each "+
			"machine has run fleetd init", err)
	case err != nil:
		return err
	}
	if res.SaltDiffers {
		fmt.Fprintf(stderr, "fleetd: warning: %s differs from the salt in %s's %s, which is used; unset %s\n",
			from, url, gitsync.FleetFile, from)
	}
	// The salt init read is the one fleetd.json last held, for whenever the file
	// is unusable before another command has read it: one a push made a directory,
	// and init's own sync brought in, say.
	keepSalt(journalDir, res.Salt, stderr)

	// The journal is set up, with its fleetd.json in place. Syncing now publishes
	// this machine's records and, once fleetd.json has been in place long enough
	// for every hook to use it, files under this machine's fleet identity the
	// records it wrote under another.
	h := identity(res.Salt)
	waitForFleetFile(journalDir, deadline)
	r, _, syncErr := syncJournal(journalDir, h, time.Until(deadline), *reclaim, stderr)
	published := r.Published

	failed := finalSyncError(syncErr, *reclaim)
	if *asJSON {
		out := map[string]any{
			"dir": journalDir, "url": url, "branch": res.Branch, "head": res.Head, "started": res.Started,
			"wrote_fleet_file": res.WroteFleetFile, "cloned": res.Cloned, "reattached": res.Reattached, "restored": res.Restored,
			"published": published,
		}
		if syncErr != nil {
			out["sync_error"] = syncErr.Error()
		}
		if err := writeJSON(stdout, out); err != nil {
			return err
		}
		return failed
	}
	switch {
	case res.Started && given == "":
		fmt.Fprintf(stdout, "started the journal in %s: its first commit holds %s, with a new salt for this fleet\n", url, gitsync.FleetFile)
	case res.Started:
		fmt.Fprintf(stdout, "started the journal in %s: its first commit holds %s, with the salt this machine was given\n", url, gitsync.FleetFile)
	case res.WroteFleetFile && given == "":
		fmt.Fprintf(stdout, "added %s to %s, with a new salt for this fleet\n", gitsync.FleetFile, url)
	case res.WroteFleetFile:
		fmt.Fprintf(stdout, "added %s to %s, with the salt this machine was given\n", gitsync.FleetFile, url)
	}
	switch {
	case res.Cloned:
		fmt.Fprintf(stdout, "the journal is set up at %s, following %s\n", journalDir, res.Branch)
	case res.Reattached:
		fmt.Fprintf(stdout, "the journal at %s is back on its branch, %s, with its files as they were\n", journalDir, res.Branch)
	default:
		fmt.Fprintf(stdout, "the journal at %s was already set up, following %s\n", journalDir, res.Branch)
	}
	if failed != nil {
		return failed
	}
	if syncErr != nil {
		fmt.Fprintf(stdout, "the first sync failed, and the next one retries: %v\n", syncErr)
		return nil
	}
	fmt.Fprintf(stdout, "published %s from this machine\n", plural(published, "record"))
	return nil
}

// finalSyncError is the error init fails with when its sync failed and no later
// sync makes up for it, else nil: the next sync retries whatever else stopped it,
// a timeout or a network failure. Only init --reclaim puts this machine's
// published records back, so when its sync failed, perhaps before doing that, it
// has to run again. A person has to act on the others: this machine's file not
// starting with the remote's copy, commits fleetd did not make, a push the remote
// declines, and a clone off the journal's branch: right after init put it back, a
// sync finds it so only when the remote deleted that branch meanwhile.
func finalSyncError(syncErr error, reclaim bool) error {
	switch {
	case syncErr == nil:
		return nil
	case reclaim:
		return fmt.Errorf("the journal is set up, but its sync failed, perhaps before putting this machine's published "+
			"records back, which no later sync does; run the same `fleetd init --reclaim` command again: %w", syncErr)
	case errors.Is(syncErr, gitsync.ErrSameFile), errors.Is(syncErr, gitsync.ErrLocalCommits),
		errors.Is(syncErr, gitsync.ErrRejected), errors.Is(syncErr, gitsync.ErrNoUpstream):
		return fmt.Errorf("the journal is set up, but nothing can be published: %w", syncErr)
	}
	return nil
}

func saltMismatch(from, where string) error {
	return fmt.Errorf("%w: %s differs from the salt in %s's %s, which is the fleet's; with it this machine "+
		"would get a second host id. Unset %s, or set it to the journal's",
		gitsync.ErrSaltMismatch, from, where, gitsync.FleetFile, from)
}

// waitForFleetFile waits, at most until deadline, for the journal's fleetd.json
// to have been in place for refileSettle, so that the sync after it can file the
// records this machine wrote under another identity.
func waitForFleetFile(dir string, deadline time.Time) {
	info, err := os.Stat(filepath.Join(dir, gitsync.FleetFile))
	if err != nil {
		return
	}
	if wait := time.Until(info.ModTime().Add(refileSettle)); wait > 0 && time.Now().Add(wait).Before(deadline) {
		time.Sleep(wait)
	}
}

// lines splits journal content into its non-empty lines, without line endings.
func lines(b []byte) []string {
	var out []string
	start := 0
	for i := 0; i <= len(b); i++ {
		if i == len(b) || b[i] == '\n' {
			line := string(b[start:i])
			if n := len(line); n > 0 && line[n-1] == '\r' {
				line = line[:n-1]
			}
			if line != "" {
				out = append(out, line)
			}
			start = i + 1
		}
	}
	return out
}
