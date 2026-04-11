// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

package hfdownloader

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// flakyRangeServer serves HTTP Range requests for a fixed byte slice but aborts
// the connection mid-stream on the FIRST request it sees for each distinct
// "end" byte (i.e. once per part). Subsequent retries for the same part — which
// carry the same `end` but a higher `start` — are served cleanly. This
// simulates a flaky network where each part gets interrupted once.
// Returns the test server and a pointer to the ordered list of Range headers
// the server observed.
func flakyRangeServer(t *testing.T, full []byte) (*httptest.Server, *[]string) {
	t.Helper()
	var (
		mu        sync.Mutex
		cutEnds   = make(map[int64]bool)
		rangeHdrs []string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodHead {
			w.Header().Set("Content-Length", strconv.Itoa(len(full)))
			w.Header().Set("Accept-Ranges", "bytes")
			return
		}
		rng := r.Header.Get("Range")
		var start, end int64
		if _, err := fmt.Sscanf(rng, "bytes=%d-%d", &start, &end); err != nil || end >= int64(len(full)) {
			http.Error(w, "bad range", http.StatusBadRequest)
			return
		}

		mu.Lock()
		rangeHdrs = append(rangeHdrs, rng)
		cut := !cutEnds[end]
		if cut {
			cutEnds[end] = true
		}
		mu.Unlock()

		content := full[start : end+1]
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, len(full)))
		w.Header().Set("Content-Length", strconv.Itoa(len(content)))
		w.WriteHeader(http.StatusPartialContent)

		if cut && len(content) > 4 {
			// Write half the bytes then abort. The client sees a short body
			// and falls into its retry path.
			half := len(content) / 2
			w.Write(content[:half])
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
			panic(http.ErrAbortHandler)
		}
		w.Write(content)
	}))
	return srv, &rangeHdrs
}

// TestDownloadMultipart_ResumesAfterFlakyConnection simulates the scenario
// from github issue #75: a slow/flaky network where each range request gets
// cut mid-stream, the client retries, and the download should converge. Before
// the fix for issue #70 each part restarted from zero on every retry, so the
// user saw progress oscillating between two values for hours. This test
// asserts: (a) the download completes, (b) the final bytes are correct, and
// (c) the retries use Range requests that RESUME from the cut point rather
// than re-requesting the full part range.
func TestDownloadMultipart_ResumesAfterFlakyConnection(t *testing.T) {
	tmpDir := t.TempDir()
	const totalSize = 20000
	full := make([]byte, totalSize)
	for i := range full {
		full[i] = byte(i % 251)
	}
	const nParts = 4
	const partSize = totalSize / nParts // 5000

	dst := filepath.Join(tmpDir, "blobs", "tmp-flaky")
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		t.Fatal(err)
	}

	srv, rangeHdrs := flakyRangeServer(t, full)
	defer srv.Close()

	it := PlanItem{
		RelativePath: "flaky.bin",
		URL:          srv.URL + "/flaky.bin",
		Size:         int64(len(full)),
		AcceptRanges: true,
	}
	cfg := Settings{Concurrency: nParts, Retries: 5}

	err := downloadMultipart(context.Background(), srv.Client(), "", Job{Repo: "o/r"}, cfg, it, dst, func(ProgressEvent) {})
	if err != nil {
		t.Fatalf("downloadMultipart: %v", err)
	}

	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatalf("read dst: %v", err)
	}
	if !bytes.Equal(got, full) {
		t.Errorf("final content mismatch: got len=%d want len=%d", len(got), len(full))
	}

	// For each part, verify:
	//   - The initial "bytes=start-end" request was made (got cut).
	//   - A SECOND request was made with start = start+half, not start=start
	//     (the latter would indicate truncate-and-restart behavior).
	seen := *rangeHdrs
	for i := 0; i < nParts; i++ {
		start := int64(i * partSize)
		end := int64((i+1)*partSize - 1)
		initial := fmt.Sprintf("bytes=%d-%d", start, end)

		// Must have at least one initial request.
		foundInitial := false
		for _, r := range seen {
			if r == initial {
				foundInitial = true
				break
			}
		}
		if !foundInitial {
			t.Errorf("part %d: no initial request %q seen; requests=%v", i, initial, seen)
		}

		// Must have a resume request: start somewhere > start, end = end.
		foundResume := false
		for _, r := range seen {
			var rs, re int64
			if _, err := fmt.Sscanf(r, "bytes=%d-%d", &rs, &re); err != nil {
				continue
			}
			if rs > start && re == end {
				foundResume = true
				break
			}
		}
		if !foundResume {
			t.Errorf("part %d: no resume request (start > %d, end = %d) seen; requests=%v",
				i, start, end, seen)
		}
	}
}

// TestDownloadMultipart_ProgressMonotonic captures every file_progress event
// emitted during a flaky download and asserts that the reported downloaded
// byte count never regresses — i.e. the UI never jumps backwards. This is
// what the user of issue #75 observed ("2.4% → 2.5% → 2.4% for hours"), which
// happens when retries truncate a part file or the progress ticker observes
// parts being deleted during assembly / the verify step.
//
// We wait a short window after downloadMultipart returns to let the periodic
// ticker fire at least once in the post-assembly state. Before the fix, the
// ticker continued stating the now-deleted part files and emitted a
// downloaded=0 event, producing a steep backwards jump on the progress bar.
func TestDownloadMultipart_ProgressMonotonic(t *testing.T) {
	tmpDir := t.TempDir()
	const totalSize = 200_000
	full := make([]byte, totalSize)
	for i := range full {
		full[i] = byte(i % 251)
	}
	const nParts = 4

	dst := filepath.Join(tmpDir, "blobs", "tmp-mono")
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		t.Fatal(err)
	}

	srv, _ := flakyRangeServer(t, full)
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var (
		mu        sync.Mutex
		events    []int64
		maxSoFar  atomic.Int64
		regressed atomic.Int32
	)
	emit := func(ev ProgressEvent) {
		if ev.Event != "file_progress" || ev.Path != "mono.bin" {
			return
		}
		mu.Lock()
		events = append(events, ev.Downloaded)
		mu.Unlock()
		for {
			cur := maxSoFar.Load()
			if ev.Downloaded >= cur {
				if maxSoFar.CompareAndSwap(cur, ev.Downloaded) {
					break
				}
				continue
			}
			// Regression: downloaded < previous max
			regressed.Add(1)
			break
		}
	}

	it := PlanItem{
		RelativePath: "mono.bin",
		URL:          srv.URL + "/mono.bin",
		Size:         int64(len(full)),
		AcceptRanges: true,
	}
	cfg := Settings{Concurrency: nParts, Retries: 5}

	err := downloadMultipart(ctx, srv.Client(), "", Job{Repo: "o/r"}, cfg, it, dst, emit)
	if err != nil {
		t.Fatalf("downloadMultipart: %v", err)
	}

	// Allow the progress ticker to fire at least once post-assembly. On the
	// unfixed code, the ticker stats deleted part files and emits a
	// downloaded=0 event here.
	time.Sleep(300 * time.Millisecond)

	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatalf("read dst: %v", err)
	}
	if !bytes.Equal(got, full) {
		t.Errorf("final content mismatch")
	}

	if regressed.Load() > 0 {
		mu.Lock()
		evs := append([]int64(nil), events...)
		mu.Unlock()
		t.Errorf("observed %d progress regression(s) — events=%v", regressed.Load(), evs)
	}
}
