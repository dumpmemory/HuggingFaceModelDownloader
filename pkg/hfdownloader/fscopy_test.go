// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

package hfdownloader

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCopyFileStream(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src.bin")
	content := bytes.Repeat([]byte("model-weights-"), 100_000) // ~1.4 MB
	if err := os.WriteFile(src, content, 0o640); err != nil {
		t.Fatal(err)
	}

	dst := filepath.Join(dir, "nested", "deep", "dst.bin")
	if err := CopyFileStream(src, dst); err != nil {
		t.Fatalf("CopyFileStream error: %v", err)
	}

	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, content) {
		t.Errorf("copied content differs from source (%d vs %d bytes)", len(got), len(content))
	}

	fi, err := os.Stat(dst)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o640 {
		t.Errorf("dst mode = %v, want 0640 (source mode preserved)", fi.Mode().Perm())
	}

	// No temp files left behind in the destination directory.
	entries, err := os.ReadDir(filepath.Dir(dst))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".mirror-tmp-") {
			t.Errorf("leftover temp file after copy: %s", e.Name())
		}
	}
}

func TestSameFileSHA256(t *testing.T) {
	dir := t.TempDir()
	a := filepath.Join(dir, "a")
	b := filepath.Join(dir, "b")
	c := filepath.Join(dir, "c")
	if err := os.WriteFile(a, []byte("payload!"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(b, []byte("payload!"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Same length, one byte different — a size check would call these equal.
	if err := os.WriteFile(c, []byte("payload?"), 0o644); err != nil {
		t.Fatal(err)
	}

	if same, err := SameFileSHA256(a, b); err != nil || !same {
		t.Errorf("SameFileSHA256(identical) = (%v, %v), want (true, nil)", same, err)
	}
	if same, err := SameFileSHA256(a, c); err != nil || same {
		t.Errorf("SameFileSHA256(same-size, different) = (%v, %v), want (false, nil)", same, err)
	}
	if _, err := SameFileSHA256(a, filepath.Join(dir, "missing")); err == nil {
		t.Error("SameFileSHA256 should error on a missing file")
	}
}
