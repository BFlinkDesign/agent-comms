package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/BFlinkDesign/agent-comms/internal/gitsync"
)

// cmdInit sets up this machine's journal: a clone of the journal repository at
// the journal directory, with the fleet's salt in it. Records written before
// the journal was set up are kept, in the clone, for the next sync to publish.
func cmdInit(args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("init", flag.ContinueOnError)
	fs.SetOutput(stderr)
	asJSON := fs.Bool("json", false, "emit JSON")
	dir := fs.String("dir", "", "journal directory (else $COMMS_CHANNELS/journal, else ~/.ai/channels/journal)")
	salt := fs.String("salt", "", "fleet salt for a journal that has none yet (else $FLEET_SALT, else a random one)")
	timeout := fs.Duration("timeout", 2*time.Minute, "give up after this long")
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

	if info, err := os.Stat(filepath.Join(journalDir, ".git")); err == nil && info.IsDir() {
		// Already a clone. It is left exactly as it is.
		fleet, ok, err := gitsync.ReadFleet(journalDir)
		switch {
		case err != nil:
			return err
		case !ok:
			return fmt.Errorf("%s is a journal clone from before fleetd.json: publish its records with "+
				"`fleetd sync --dir %s`, then delete it and run fleetd init again", journalDir, journalDir)
		case given != "" && given != fleet.Salt:
			return saltMismatch(from, journalDir)
		}
		if *asJSON {
			return writeJSON(stdout, map[string]any{"dir": journalDir, "already": true})
		}
		fmt.Fprintf(stdout, "the journal at %s is already set up\n", journalDir)
		return nil
	}
	earlier, err := recordsOnly(journalDir)
	if err != nil {
		return err
	}

	parent := filepath.Dir(journalDir)
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return err
	}
	clone, err := os.MkdirTemp(parent, filepath.Base(journalDir)+".init-")
	if err != nil {
		return err
	}
	keepClone := false
	defer func() {
		if !keepClone {
			os.RemoveAll(clone)
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	res, err := gitsync.Init(ctx, gitsync.InitOptions{URL: url, Dir: clone, Salt: given})
	if errors.Is(err, gitsync.ErrSaltMismatch) {
		return saltMismatch(from, url)
	}
	if err != nil {
		return err
	}

	// Records written before the journal was set up move into the clone. The
	// directory is renamed aside first, so an append that races this finds no
	// file to write to rather than one about to be deleted.
	kept, aside := 0, ""
	if earlier != nil {
		aside = journalDir + ".before-init-" + randomSuffix()
		if err := renameRetry(journalDir, aside); err != nil {
			return err
		}
		for _, name := range earlier {
			n, err := adopt(filepath.Join(aside, name), filepath.Join(clone, name))
			if err != nil {
				return fmt.Errorf("%w; the records written before init are in %s", err, aside)
			}
			kept += n
		}
	}
	if err := renameRetry(clone, journalDir); err != nil {
		keepClone = true
		return fmt.Errorf("%w; the new clone is at %s and the records written before init are in %s", err, clone, aside)
	}
	keepClone = true
	if aside != "" {
		// Every record in it is in the clone now.
		if err := os.RemoveAll(aside); err != nil {
			fmt.Fprintf(stderr, "fleetd: warning: could not remove %s, whose records are all in the journal: %v\n", aside, err)
		}
	}

	if *asJSON {
		return writeJSON(stdout, map[string]any{
			"dir": journalDir, "url": url, "branch": res.Branch, "head": res.Head,
			"started": res.Started, "wrote_fleet_file": res.WroteFleetFile, "kept": kept,
		})
	}
	switch {
	case res.Started:
		fmt.Fprintf(stdout, "started the journal in %s: its first commit holds %s, with this fleet's salt\n", url, gitsync.FleetFile)
	case res.WroteFleetFile:
		fmt.Fprintf(stdout, "added %s, with this fleet's salt, to %s\n", gitsync.FleetFile, url)
	}
	fmt.Fprintf(stdout, "the journal is set up at %s, following %s\n", journalDir, res.Branch)
	if kept > 0 {
		fmt.Fprintf(stdout, "kept %s written before init; the next sync publishes this machine's\n", plural(kept, "record"))
	}
	return nil
}

func saltMismatch(from, where string) error {
	return fmt.Errorf("%w: %s differs from the salt in %s's %s, which is the fleet's; with it this machine "+
		"would get a second host id. Unset %s, or set it to the journal's",
		gitsync.ErrSaltMismatch, from, where, gitsync.FleetFile, from)
}

// recordsOnly lists the journal files in dir, which init may take over: nil
// when dir does not exist or is empty. Anything else in dir is refused, so init
// never deletes what it did not write.
func recordsOnly(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var names, foreign []string
	for _, e := range entries {
		if e.Type().IsRegular() && strings.HasSuffix(e.Name(), ".jsonl") {
			names = append(names, e.Name())
			continue
		}
		foreign = append(foreign, e.Name())
	}
	if len(foreign) > 0 {
		return nil, fmt.Errorf("%s holds %s, which is not a journal file; init sets up the journal "+
			"there, so move it, or give init another --dir", dir, strings.Join(foreign, ", "))
	}
	return names, nil
}

// adopt appends to into every line of from that into does not already hold,
// and returns how many that was. A final line without a newline, left by an
// interrupted append, is kept as a line of its own.
func adopt(from, into string) (int, error) {
	local, err := os.ReadFile(from)
	if err != nil {
		return 0, err
	}
	published, err := os.ReadFile(into)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return 0, err
	}
	var merged bytes.Buffer
	seen := map[string]bool{}
	for _, line := range lines(published) {
		seen[line] = true
		merged.WriteString(line + "\n")
	}
	added := 0
	for _, line := range lines(local) {
		if !seen[line] {
			seen[line] = true
			merged.WriteString(line + "\n")
			added++
		}
	}
	if added == 0 {
		return 0, nil
	}
	tmp := into + ".adopt"
	if err := os.WriteFile(tmp, merged.Bytes(), 0o644); err != nil {
		return 0, err
	}
	return added, renameRetry(tmp, into)
}

// lines splits journal content into its non-empty lines, without line endings.
func lines(b []byte) []string {
	var out []string
	for _, line := range strings.Split(string(b), "\n") {
		if line = strings.TrimSuffix(line, "\r"); line != "" {
			out = append(out, line)
		}
	}
	return out
}

// renameRetry renames, retrying for a few seconds while Windows refuses because
// another process, such as a virus scanner or an indexer, has a file open.
func renameRetry(from, to string) error {
	var err error
	for delay := 50 * time.Millisecond; delay < 5*time.Second; delay *= 2 {
		if err = os.Rename(from, to); err == nil {
			return nil
		}
		if _, statErr := os.Stat(from); statErr != nil {
			return err
		}
		time.Sleep(delay)
	}
	return err
}

func randomSuffix() string {
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
