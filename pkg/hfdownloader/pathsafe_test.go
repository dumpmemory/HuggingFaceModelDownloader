// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

package hfdownloader

import "testing"

// TestUnsafeRepoPath guards the path-traversal containment used on
// remote-controlled tree paths before they reach any filesystem write.
func TestUnsafeRepoPath(t *testing.T) {
	safe := []string{
		"config.json",
		"subdir/model.safetensors",
		"a/b/c.txt",
		"weird name.bin", // spaces are fine
		"a/../b",         // normalises to "b", stays in root
		"./config.json",  // normalises to "config.json"
		"deep/nested/dir/file.gguf",
	}
	for _, p := range safe {
		if unsafeRepoPath(p) {
			t.Errorf("unsafeRepoPath(%q) = true, want false (should be allowed)", p)
		}
	}

	unsafe := []string{
		"",                 // empty
		".",                // not a real file
		"..",               // parent
		"./..",             // normalises to ".."
		"../escape",        // escapes root
		"../../etc/passwd", // escapes root
		"a/../../b",        // normalises to "../b"
		"foo/../../bar",    // normalises to "../bar"
		"/etc/passwd",      // absolute
		"/abs/path",        // absolute
		"dir\\file",        // backslash (Windows separator)
		"C:\\Windows\\x",   // drive + backslash
	}
	for _, p := range unsafe {
		if !unsafeRepoPath(p) {
			t.Errorf("unsafeRepoPath(%q) = false, want true (should be rejected)", p)
		}
	}
}
