package journal

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/BFlinkDesign/agent-comms/internal/cell"
)

const (
	testHost = "host:d9d9f8d0385c8425"
	testStem = "host-d9d9f8d0385c8425"
	// childEnv makes the test binary re-enter as a journal-writing child, so the
	// concurrency test can use real processes rather than goroutines.
	childEnv = "JOURNAL_TEST_CHILD_DIR"
)

// TestMain lets the test binary act as its own concurrent-writer helper.
func TestMain(m *testing.M) {
	if dir := os.Getenv(childEnv); dir != "" {
		os.Exit(runChild(dir, os.Getenv("JOURNAL_TEST_CHILD_TAG"), os.Getenv("JOURNAL_TEST_CHILD_N")))
	}
	os.Exit(m.Run())
}

func runChild(dir, tag, nStr string) int {
	n, err := strconv.Atoi(nStr)
	if err != nil {
		fmt.Fprintln(os.Stderr, "child: bad count:", err)
		return 2
	}
	s, err := Open(dir)
	if err != nil {
		fmt.Fprintln(os.Stderr, "child: open:", err)
		return 2
	}
	for i := 0; i < n; i++ {
		// A deliberately long payload: short writes could pass by luck, whereas a
		// payload near the record limit will visibly interleave if appends are not
		// atomic.
		c := mustCell(nil, tag, i, strings.Repeat("x", 512))
		if err := s.Append(testHost, c); err != nil {
			fmt.Fprintln(os.Stderr, "child: append:", err)
			return 1
		}
	}
	return 0
}

func mustCell(t *testing.T, tag string, i int, pad string) cell.Cell {
	c, err := cell.New("observation", "fleet/"+tag,
		fmt.Sprintf("2026-09-14T12:00:%02dZ", i%60), "journal",
		map[string]cell.Value{"tag": cell.S(tag), "i": cell.I(int64(i)), "pad": cell.S(pad)},
		nil, nil, 0)
	if err != nil {
		if t != nil {
			t.Fatal(err)
		}
		panic(err)
	}
	return c
}

func TestAppendThenReadRoundTrip(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	want := mustCell(t, "a", 1, "")
	if err := s.Append(testHost, want); err != nil {
		t.Fatal(err)
	}

	got, err := s.Read(testHost)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("read %d records, want 1", len(got))
	}
	if got[0].ID != want.ID {
		t.Errorf("id = %s, want %s", got[0].ID, want.ID)
	}
	if got[0].Host != testStem {
		t.Errorf("Host = %q, want %q — attribution is the point of this file", got[0].Host, testStem)
	}
	if got[0].Line != 1 {
		t.Errorf("Line = %d, want 1", got[0].Line)
	}
	if got[0].Raw != want.Marshal() {
		t.Errorf("Raw was not preserved verbatim:\n got %s\nwant %s", got[0].Raw, want.Marshal())
	}
}

func TestMissingJournalReadsEmptyNotError(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	got, err := s.Read(testHost)
	if err != nil {
		t.Fatalf("reading a host that has never written should not error: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("got %d records from an absent journal", len(got))
	}
}

func TestConcurrentAppendsFromSeparateProcesses(t *testing.T) {
	// This is the test that earns the claim in the package doc. Goroutines share
	// one process and could pass while the real property was absent; separate
	// processes each open their own descriptor, which is the case O_APPEND
	// atomicity actually has to cover.
	dir := t.TempDir()
	const writers, perWriter = 6, 40

	self, err := os.Executable()
	if err != nil {
		t.Skipf("cannot locate the test binary to re-exec: %v", err)
	}

	var wg sync.WaitGroup
	errCh := make(chan error, writers)
	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			cmd := exec.Command(self)
			cmd.Env = append(os.Environ(),
				childEnv+"="+dir,
				"JOURNAL_TEST_CHILD_TAG=w"+strconv.Itoa(w),
				"JOURNAL_TEST_CHILD_N="+strconv.Itoa(perWriter),
			)
			if out, err := cmd.CombinedOutput(); err != nil {
				errCh <- fmt.Errorf("writer %d: %v: %s", w, err, out)
			}
		}(w)
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Fatal(err)
	}

	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	got, err := s.Read(testHost)
	if err != nil {
		t.Fatalf("journal was damaged by concurrent appends: %v", err)
	}

	// Every line must be a whole, parseable record: interleaving would produce
	// malformed JSON, which Read reports as an error rather than skipping.
	if len(got) != writers*perWriter {
		t.Fatalf("read %d records, want %d — records were lost or merged", len(got), writers*perWriter)
	}
	// And every writer's full set must be present, so nothing was overwritten.
	perTag := map[string]int{}
	for _, r := range got {
		perTag[r.From]++
	}
	for w := 0; w < writers; w++ {
		tag := "fleet/w" + strconv.Itoa(w)
		if perTag[tag] != perWriter {
			t.Errorf("writer %s contributed %d records, want %d", tag, perTag[tag], perWriter)
		}
	}
}

func TestTornFinalRecordIsReportedWithTheGoodRecords(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if err := s.Append(testHost, mustCell(t, "a", i, "")); err != nil {
			t.Fatal(err)
		}
	}
	// Simulate a crash partway through the fourth append.
	p := filepath.Join(dir, testStem+".jsonl")
	f, err := os.OpenFile(p, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(`{"id":"hive:deadbeef","v":1,"type":"obs`); err != nil {
		t.Fatal(err)
	}
	f.Close()

	got, err := s.Read(testHost)
	if !errors.Is(err, ErrTornRecord) {
		t.Fatalf("err = %v, want it to wrap ErrTornRecord — a silently dropped last entry is the failure mode here", err)
	}
	if len(got) != 3 {
		t.Fatalf("read %d records alongside the torn one, want the 3 intact ones", len(got))
	}
}

func TestMalformedRecordIsDistinctFromTornAndNamesItsLine(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Append(testHost, mustCell(t, "a", 0, "")); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, testStem+".jsonl")
	f, err := os.OpenFile(p, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	// Terminated, so not torn — but missing the required id field.
	if _, err := f.WriteString("{\"type\":\"observation\",\"ts\":\"2026-09-14T12:00:00Z\",\"from\":\"a/b\"}\n"); err != nil {
		t.Fatal(err)
	}
	f.Close()

	_, err = s.Read(testHost)
	if !errors.Is(err, ErrMalformed) {
		t.Fatalf("err = %v, want it to wrap ErrMalformed", err)
	}
	if errors.Is(err, ErrTornRecord) {
		t.Error("a completed-but-wrong write was reported as torn; the two causes need different fixes")
	}
	if !strings.Contains(err.Error(), "line 2") {
		t.Errorf("err = %v, want it to name line 2 so the defect can be found", err)
	}
}

func TestMalformedLineDoesNotHideTheRecordsAfterIt(t *testing.T) {
	// The defect this pins: returning at the first unparseable line discarded
	// every valid record after it, so `fleetd where` reported a stale
	// last-activity and an undercount with no signal but a warning. This bus is
	// explicitly open to any process that can append, including a plane whose
	// records this parser rejects, so such a line is an expected input.
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Append(testHost, mustCell(t, "first", 1, "")); err != nil {
		t.Fatal(err)
	}

	p := filepath.Join(dir, testStem+".jsonl")
	f, err := os.OpenFile(p, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	// Terminated and well-formed JSON, but not a cell: no id, no from.
	if _, err := f.WriteString("{\"note\":\"written by another writer\"}\n"); err != nil {
		t.Fatal(err)
	}
	f.Close()

	if err := s.Append(testHost, mustCell(t, "third", 3, "")); err != nil {
		t.Fatal(err)
	}

	got, err := s.Read(testHost)
	if !errors.Is(err, ErrMalformed) {
		t.Fatalf("err = %v, want the bad line reported as ErrMalformed", err)
	}
	if len(got) != 2 {
		t.Fatalf("read %d records, want both valid ones — the record after the bad line was dropped", len(got))
	}
	// Specifically the LAST record must survive: it is the one "where did I leave
	// off" actually reports.
	if got[len(got)-1].From != "fleet/third" {
		t.Errorf("last record is %q, want fleet/third", got[len(got)-1].From)
	}
	if got[len(got)-1].Line != 3 {
		t.Errorf("last record Line = %d, want 3 — line numbering must still count the skipped line", got[len(got)-1].Line)
	}
}

func TestSeveralMalformedLinesAreAllReported(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Append(testHost, mustCell(t, "ok", 0, "")); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, testStem+".jsonl")
	f, err := os.OpenFile(p, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	f.WriteString("{\"no\":\"id\"}\n")
	f.WriteString("not json at all\n")
	f.Close()

	got, err := s.Read(testHost)
	if err == nil {
		t.Fatal("two malformed lines were not reported")
	}
	if n := strings.Count(err.Error(), "line "); n < 2 {
		t.Errorf("err mentions %d lines, want both: %v", n, err)
	}
	if len(got) != 1 {
		t.Errorf("read %d records, want the 1 valid one", len(got))
	}
}

func TestReadingAnAbsentStoreIsNotAnEmptyStore(t *testing.T) {
	// A mistyped --dir used to create the directory, answer "no records" and exit
	// zero, which is indistinguishable from a machine that genuinely recorded
	// nothing. For a tool whose only job is saying which machine did something,
	// that is the worst available answer.
	missing := filepath.Join(t.TempDir(), "typo", "journal")
	s, err := Open(missing)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ReadAll(); !errors.Is(err, ErrNoStore) {
		t.Fatalf("err = %v, want ErrNoStore", err)
	}
	if _, statErr := os.Stat(missing); !os.IsNotExist(statErr) {
		t.Errorf("a read created %s; only Append may create the store", missing)
	}
}

func TestAppendCreatesTheStoreOnDemand(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "nested", "journal")
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Append(testHost, mustCell(t, "a", 0, "")); err != nil {
		t.Fatalf("Append did not create the store: %v", err)
	}
	got, err := s.Read(testHost)
	if err != nil || len(got) != 1 {
		t.Fatalf("read back %d records, err %v", len(got), err)
	}
}

func TestRefusesSymlinkedTarget(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(t.TempDir(), "elsewhere.jsonl")
	if err := os.WriteFile(target, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, testStem+".jsonl")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("cannot create symlinks here: %v", err)
	}

	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	err = s.Append(testHost, mustCell(t, "a", 0, ""))
	if !errors.Is(err, ErrSymlink) {
		t.Fatalf("err = %v, want ErrSymlink — writing through a link redirects the journal off-store", err)
	}
	if b, _ := os.ReadFile(target); len(b) != 0 {
		t.Errorf("the link target was written to anyway: %q", b)
	}
}

func TestRejectsOversizedRecord(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	// Well past the limit, so the refusal cannot be an off-by-one accident.
	err = s.Append(testHost, mustCell(t, "a", 0, strings.Repeat("y", MaxRecordBytes*2)))
	if !errors.Is(err, ErrTooLarge) {
		t.Fatalf("err = %v, want ErrTooLarge", err)
	}
	if got, rerr := s.Read(testHost); rerr != nil || len(got) != 0 {
		t.Errorf("a refused record still reached the file: %d records, err %v", len(got), rerr)
	}
}

func TestRejectsUnsafeHostNames(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{
		"../escape", "a/b", `..\escape`, "", "has space", "-leading-dash",
		strings.Repeat("a", 65),
	} {
		if err := s.Append(bad, mustCell(t, "a", 0, "")); !errors.Is(err, ErrBadName) {
			t.Errorf("Append(%q) err = %v, want ErrBadName", bad, err)
		}
	}
}

func TestHostNameCaseIsNormalisedNotRejected(t *testing.T) {
	// Case is folded rather than refused, deliberately. Windows filesystems are
	// case-insensitive, so accepting "HostA" and "hosta" as distinct would create
	// two files on Linux that are one file on Windows -- a journal that silently
	// merges on one machine and splits on another. Folding makes the mapping the
	// same everywhere.
	//
	// The consequence, stated because it is a real one: two host identifiers that
	// differ only in case collide into a single journal. Host IDs are lowercase
	// hex digests produced by internal/hostid, so this cannot arise in practice,
	// but a future identifier scheme must not rely on case for distinctness.
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Append("host:AbC1", mustCell(t, "upper", 0, "")); err != nil {
		t.Fatalf("mixed-case host id was rejected: %v", err)
	}
	if err := s.Append("host:abc1", mustCell(t, "lower", 1, "")); err != nil {
		t.Fatalf("lower-case host id was rejected: %v", err)
	}

	names, err := s.Hosts()
	if err != nil {
		t.Fatal(err)
	}
	if len(names) != 1 || names[0] != "host-abc1" {
		t.Fatalf("Hosts() = %v, want exactly [host-abc1]", names)
	}
	got, err := s.Read("host:ABC1")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("read %d records, want both writes in the one folded journal", len(got))
	}
}

func TestHostIDIsMappedToASafeStem(t *testing.T) {
	// "host:abc" is not a legal Windows filename, so the separator is replaced
	// rather than the identity altered.
	if got := FileName("host:ABC123"); got != "host-abc123" {
		t.Errorf("FileName = %q, want host-abc123", got)
	}
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Append("host:ABC123", mustCell(t, "a", 0, "")); err != nil {
		t.Fatalf("a real host id was rejected: %v", err)
	}
}

func TestReadAllSpansHostsAndAttributesEach(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	hosts := []string{"host:aaa1", "host:bbb2", "host:ccc3"}
	for i, h := range hosts {
		for j := 0; j <= i; j++ {
			if err := s.Append(h, mustCell(t, "h"+strconv.Itoa(i), j, "")); err != nil {
				t.Fatal(err)
			}
		}
	}

	names, err := s.Hosts()
	if err != nil {
		t.Fatal(err)
	}
	if len(names) != len(hosts) {
		t.Fatalf("Hosts() = %v, want %d entries", names, len(hosts))
	}

	all, err := s.ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 1+2+3 {
		t.Fatalf("ReadAll returned %d records, want 6", len(all))
	}
	counts := map[string]int{}
	for _, r := range all {
		counts[r.Host]++
	}
	for i, h := range hosts {
		if got := counts[FileName(h)]; got != i+1 {
			t.Errorf("host %s contributed %d records, want %d", h, got, i+1)
		}
	}
}

func TestReadAllReportsOneDamagedHostWithoutHidingTheOthers(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Append("host:good1", mustCell(t, "g", 0, "")); err != nil {
		t.Fatal(err)
	}
	if err := s.Append("host:bad2", mustCell(t, "b", 0, "")); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(filepath.Join(dir, "host-bad2.jsonl"), os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	f.WriteString(`{"id":"hive:x","truncated`)
	f.Close()

	all, err := s.ReadAll()
	if !errors.Is(err, ErrTornRecord) {
		t.Fatalf("err = %v, want the damaged host to be reported", err)
	}
	// Nine good hosts and a named bad one beats failing the whole read.
	var good int
	for _, r := range all {
		if r.Host == "host-good1" {
			good++
		}
	}
	if good != 1 {
		t.Errorf("the intact host's records were lost: got %d", good)
	}
}

func TestJournalFileIsOwnerOnly(t *testing.T) {
	// runtime.GOOS, not os.Getenv("GOOS"): GOOS is a build-time constant, not an
	// environment variable at run time, so the env lookup is always empty and this
	// skip would never have fired. On Windows the assertion below would then fail
	// against permissions POSIX mode bits do not describe. A skip condition that
	// cannot fire is the same defect class as a gate that cannot fail.
	if runtime.GOOS == "windows" {
		t.Skip("POSIX mode bits do not describe Windows ACLs")
	}
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Append(testHost, mustCell(t, "a", 0, "")); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(filepath.Join(dir, testStem+".jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if perm := fi.Mode().Perm(); perm&0o077 != 0 {
		t.Errorf("journal mode is %#o; it records local activity and should not be group- or world-readable", perm)
	}
}

// A crash partway through an append leaves a fragment with no newline. The next
// record must start on a line of its own: joined to the fragment, it would be
// lost with it, and published that way to every machine.
func TestAnAppendAfterATornRecordIsNotJoinedToIt(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Append(testHost, mustCell(t, "a", 0, "")); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(filepath.Join(dir, testStem+".jsonl"), os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(`{"id":"hive:deadbeef","v":1,"type":"obs`); err != nil {
		t.Fatal(err)
	}
	f.Close()
	if err := s.Append(testHost, mustCell(t, "a", 1, "")); err != nil {
		t.Fatal(err)
	}

	got, err := s.Read(testHost)
	if len(got) != 2 {
		t.Fatalf("read %d intact records, want both appended ones (err: %v)", len(got), err)
	}
	if err == nil || errors.Is(err, ErrTornRecord) {
		t.Fatalf("err = %v, want the fragment reported as a malformed line of its own", err)
	}
}
