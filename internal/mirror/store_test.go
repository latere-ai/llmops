// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: MIT

package mirror

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestLocalStorePutLeavesNothingBehindWhenTheCopyFails covers the path the
// Close-error fix opened. A directory opens fine and fails on read, which is
// the cheapest way to fail a copy midway without a fake filesystem.
//
// What matters is not the error but the absence: a partial .put-* temp file
// left in the store would be indistinguishable from a real object to List.
func TestLocalStorePutLeavesNothingBehindWhenTheCopyFails(t *testing.T) {
	root := t.TempDir()
	s := &LocalStore{Root: root}
	src := filepath.Join(t.TempDir(), "adirectory")
	if err := os.MkdirAll(src, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := s.Put(src, "models/thing.bin"); err == nil {
		t.Fatal("copying a directory must fail")
	}
	entries, err := os.ReadDir(filepath.Join(root, "models"))
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".put-") {
			t.Errorf("a failed Put left %s behind", e.Name())
		}
	}
}

// TestLocalStoreGetFailsRatherThanWritingAShortFile covers Get's copy-failure
// path. Get returning nil for a truncated file is the bug the Close check was
// added for; this pins the neighbouring branch that the same fix reshaped.
func TestLocalStoreGetFailsRatherThanWritingAShortFile(t *testing.T) {
	root := t.TempDir()
	s := &LocalStore{Root: root}
	// A directory where the object should be: Open succeeds, the read fails.
	if err := os.MkdirAll(filepath.Join(root, "models", "thing.bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	local := filepath.Join(t.TempDir(), "out.bin")
	if err := s.Get("models/thing.bin", local); err == nil {
		t.Fatal("copying from a directory must fail rather than write a short file")
	}
}
