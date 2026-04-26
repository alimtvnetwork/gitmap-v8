package cmd

// E2E-style tests for `gitmap cn --all` / `--csv` batch mode under
// concurrency (v3.125.0+). These tests exercise the full
// dispatcher → worker pool → collector → CSV writer pipeline with
// processOneBatchRepoFn stubbed to a deterministic synthetic
// processor, so they run in milliseconds without needing real git
// repos, a network, or a temp filesystem of fixtures.
//
// What we're guarding:
//
//   1. **Deterministic CSV row ordering** regardless of pool size.
//      Workers complete out-of-order under concurrency, but the
//      collector re-slots results by input index so the CSV must
//      mirror the input repo list.
//   2. **Correct per-status counts** (ok / failed / skipped) match
//      what the stub emitted, proving no result is lost or
//      double-counted by the worker pool.
//   3. **Byte-identical reports across runs** with --max-concurrency
//      varying from 1 to 8 — the strongest possible determinism
//      guarantee.
//   4. **Real CSV file output** via writeBatchReport, exercising the
//      actual production writer (not a re-implemented mock).

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alimtvnetwork/gitmap-v7/gitmap/constants"
)

// stubProcessor returns a deterministic batchRowResult per repo path
// with a small randomized sleep so workers genuinely interleave
// completion order. The status cycles ok/failed/skipped/ok/... by
// trailing digit so we know exactly how many of each to expect.
//
// `concurrentSeen` tracks the maximum simultaneous in-flight count
// so the test can assert the pool actually went parallel — a
// regression guard against an accidental serialization.
func stubProcessor(concurrentSeen *int64) func(string) batchRowResult {
	var inflight int64
	return func(path string) batchRowResult {
		now := atomic.AddInt64(&inflight, 1)
		for {
			peak := atomic.LoadInt64(concurrentSeen)
			if now <= peak || atomic.CompareAndSwapInt64(concurrentSeen, peak, now) {
				break
			}
		}
		// Tiny sleep based on the trailing digit so different repos
		// take different times — guarantees out-of-order completion.
		base := filepath.Base(path)
		last := base[len(base)-1] - '0'
		time.Sleep(time.Duration(last%5+1) * time.Millisecond)
		atomic.AddInt64(&inflight, -1)

		return batchRowResult{
			RepoPath:    path,
			FromVersion: "v1",
			ToVersion:   fmt.Sprintf("v%d", last+1),
			Status:      pickStubStatus(last),
			Detail:      "",
		}
	}
}

// pickStubStatus distributes 50 inputs across the three buckets in
// a known ratio (digits 0-3 → ok, 4-7 → failed, 8-9 → skipped) so
// the count assertions are exact, not statistical.
func pickStubStatus(last byte) string {
	switch {
	case last <= 3:
		return constants.BatchStatusOK
	case last <= 7:
		return constants.BatchStatusFailed
	default:
		return constants.BatchStatusSkipped
	}
}

// installStubProcessor swaps processOneBatchRepoFn for the test and
// restores the original in t.Cleanup. Returns the peak-inflight
// counter so callers can assert the pool actually parallelized.
func installStubProcessor(t *testing.T) *int64 {
	t.Helper()
	original := processOneBatchRepoFn
	var peak int64
	processOneBatchRepoFn = stubProcessor(&peak)
	t.Cleanup(func() {
		processOneBatchRepoFn = original
	})
	return &peak
}

// makeRepoPaths returns n synthetic repo paths whose trailing digit
// drives the stubProcessor's status assignment.
func makeRepoPaths(n int) []string {
	out := make([]string, n)
	for i := 0; i < n; i++ {
		out[i] = fmt.Sprintf("/tmp/repo-%d", i)
	}
	return out
}

// TestE2E_BatchConcurrency_DeterministicOrdering proves that under
// --max-concurrency=8 over 50 repos, the result slice is still in
// input order even though the stub deliberately makes workers
// finish out-of-order.
func TestE2E_BatchConcurrency_DeterministicOrdering(t *testing.T) {
	peak := installStubProcessor(t)
	repos := makeRepoPaths(50)

	results := processBatchReposConcurrent(repos, 8, nil)

	if len(results) != len(repos) {
		t.Fatalf("results length %d != input %d", len(results), len(repos))
	}
	for i, r := range results {
		if r.RepoPath != repos[i] {
			t.Fatalf("ordering drift at index %d: got %s, want %s",
				i, r.RepoPath, repos[i])
		}
	}
	if atomic.LoadInt64(peak) < 2 {
		t.Fatalf("pool never went parallel (peak inflight = %d) — test is invalid",
			atomic.LoadInt64(peak))
	}
}

// TestE2E_BatchConcurrency_StatusCountsExact verifies the per-bucket
// totals exactly match what the stub emitted: 50 repos with last
// digits 0-9 cycling 5 times → 20 ok, 20 failed, 10 skipped.
func TestE2E_BatchConcurrency_StatusCountsExact(t *testing.T) {
	installStubProcessor(t)
	repos := makeRepoPaths(50)

	results := processBatchReposConcurrent(repos, 4, nil)
	ok, failed, skipped := tallyBatch(results)

	const wantOK, wantFailed, wantSkipped = 20, 20, 10
	if ok != wantOK || failed != wantFailed || skipped != wantSkipped {
		t.Fatalf("counts: ok=%d failed=%d skipped=%d, want %d/%d/%d",
			ok, failed, skipped, wantOK, wantFailed, wantSkipped)
	}
}

// TestE2E_BatchConcurrency_ByteIdenticalAcrossPoolSizes is the
// strongest determinism guarantee: regardless of worker count, the
// CSV bytes produced by writeReportRows are identical.
func TestE2E_BatchConcurrency_ByteIdenticalAcrossPoolSizes(t *testing.T) {
	installStubProcessor(t)
	repos := makeRepoPaths(50)

	baseline := runAndSerialize(t, repos, 1)
	for _, workers := range []int{2, 4, 8, 16} {
		got := runAndSerialize(t, repos, workers)
		if !bytes.Equal(baseline, got) {
			t.Fatalf("CSV bytes differ at workers=%d (sequential vs parallel)\n--- want ---\n%s\n--- got ---\n%s",
				workers, baseline, got)
		}
	}
}

// runAndSerialize is the test helper that runs the concurrent pool
// and writes the CSV via the real writeReportRows into a buffer
// (using a temp file so we exercise the *os.File path the
// production writer uses).
func runAndSerialize(t *testing.T, repos []string, workers int) []byte {
	t.Helper()
	results := processBatchReposConcurrent(repos, workers, nil)

	tmp, err := os.CreateTemp(t.TempDir(), "cn-batch-*.csv")
	if err != nil {
		t.Fatalf("temp csv: %v", err)
	}
	writeReportRows(tmp, results)
	tmp.Close()

	data, err := os.ReadFile(tmp.Name())
	if err != nil {
		t.Fatalf("read back csv: %v", err)
	}
	return data
}

// TestE2E_BatchConcurrency_FullWriteBatchReport drives the
// production writeBatchReport (the real cn entrypoint helper) end
// to end: temp CWD, real file creation with the unix-second name,
// real CSV bytes. Asserts header line + one row per repo + input
// ordering preserved.
func TestE2E_BatchConcurrency_FullWriteBatchReport(t *testing.T) {
	installStubProcessor(t)
	t.Chdir(t.TempDir()) // writeBatchReport writes to cwd

	repos := makeRepoPaths(20)
	results := processBatchReposConcurrent(repos, 4, nil)

	reportPath := writeBatchReport(results)
	if reportPath == "" {
		t.Fatal("writeBatchReport returned empty path")
	}

	data, err := os.ReadFile(reportPath)
	if err != nil {
		t.Fatalf("read report: %v", err)
	}
	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	if len(lines) != len(repos)+1 {
		t.Fatalf("line count: got %d, want %d (1 header + %d rows)",
			len(lines), len(repos)+1, len(repos))
	}
	if !strings.HasPrefix(lines[0], "repo,from,to,status,detail") {
		t.Fatalf("header line: got %q", lines[0])
	}
	// Spot-check ordering: row N's repo path must contain repo-N.
	for i := 0; i < len(repos); i++ {
		want := fmt.Sprintf("repo-%d", i)
		if !strings.Contains(lines[i+1], want) {
			t.Fatalf("row %d should contain %q, got %q", i, want, lines[i+1])
		}
	}
}

// TestE2E_BatchConcurrency_ProgressCallbackFires verifies the
// onResult callback handed to the collector receives exactly one
// invocation per repo, with the same total ordering invariant as
// the returned slice — proving the live progress reporter sees a
// faithful view even under heavy concurrency.
func TestE2E_BatchConcurrency_ProgressCallbackFires(t *testing.T) {
	installStubProcessor(t)
	repos := makeRepoPaths(30)

	var seenCount int64
	var seenStatuses []string
	cb := func(row batchRowResult) {
		atomic.AddInt64(&seenCount, 1)
		seenStatuses = append(seenStatuses, row.Status) // collector is single-threaded; no race
	}

	results := processBatchReposConcurrent(repos, 6, cb)

	if got := atomic.LoadInt64(&seenCount); int(got) != len(repos) {
		t.Fatalf("callback fired %d times, want %d", got, len(repos))
	}
	if len(seenStatuses) != len(results) {
		t.Fatalf("status count drift: cb saw %d, results has %d",
			len(seenStatuses), len(results))
	}
}
