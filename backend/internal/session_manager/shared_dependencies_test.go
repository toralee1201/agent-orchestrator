package sessionmanager

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// An isolated checkout gets the dependency folders the worker's workspace
// already installed, without any installer running: the folder is git-ignored
// there and is linked in.
func TestCarryInstalledDependenciesLinksInstalledFolders(t *testing.T) {
	source, target := t.TempDir(), t.TempDir()
	writeTestFile(t, filepath.Join(source, "node_modules", "left-pad", "index.js"), "pad")
	writeTestFile(t, filepath.Join(source, "packages", "app", "node_modules", "react", "index.js"), "react")
	writeTestFile(t, filepath.Join(source, ".venv", "bin", "python"), "python")
	writeTestFile(t, filepath.Join(source, "src", "main.go"), "package main")
	if err := os.MkdirAll(filepath.Join(target, "packages", "app"), 0o755); err != nil {
		t.Fatal(err)
	}

	linked, err := CarryInstalledDependencies(context.Background(), source, target)
	if err != nil {
		t.Fatalf("CarryInstalledDependencies: %v", err)
	}
	if _, err := os.Stat(filepath.Join(target, "node_modules", "left-pad", "index.js")); err != nil {
		t.Fatalf("node_modules not linked: %v", err)
	}
	if _, err := os.Stat(filepath.Join(target, "packages", "app", "node_modules", "react", "index.js")); err != nil {
		t.Fatalf("nested node_modules not linked: %v", err)
	}
	if _, err := os.Stat(filepath.Join(target, ".venv", "bin", "python")); err != nil {
		t.Fatalf(".venv not linked: %v", err)
	}
	if len(linked) != 3 {
		t.Fatalf("linked = %v, want the three dependency folders", linked)
	}
	// Only dependencies: tracked content stays whatever git checked out.
	if _, err := os.Stat(filepath.Join(target, "src", "main.go")); !os.IsNotExist(err) {
		t.Fatalf("tracked file copied: err=%v", err)
	}
}

// A folder the target already provides is not replaced, and its content is
// what the suite will see.
func TestCarryInstalledDependenciesKeepsAnExistingFolder(t *testing.T) {
	source, target := t.TempDir(), t.TempDir()
	writeTestFile(t, filepath.Join(source, "node_modules", "a.js"), "source")
	writeTestFile(t, filepath.Join(target, "node_modules", "a.js"), "target")

	if _, err := CarryInstalledDependencies(context.Background(), source, target); err != nil {
		t.Fatalf("CarryInstalledDependencies: %v", err)
	}
	body, err := os.ReadFile(filepath.Join(target, "node_modules", "a.js"))
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != "target" {
		t.Fatalf("existing dependency folder was replaced: %q", body)
	}
}

// The worker's own workspace may have nothing installed yet; that is not an
// error, the suite reports what it actually needs.
func TestCarryInstalledDependenciesWithoutInstalledFolders(t *testing.T) {
	source, target := t.TempDir(), t.TempDir()
	writeTestFile(t, filepath.Join(source, "src", "main.go"), "package main")

	linked, err := CarryInstalledDependencies(context.Background(), source, target)
	if err != nil {
		t.Fatalf("CarryInstalledDependencies: %v", err)
	}
	if len(linked) != 0 {
		t.Fatalf("linked = %v, want nothing", linked)
	}
	entries, err := os.ReadDir(target)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("target gained entries: %v", entries)
	}
}

// A dependency path with a space in it is what breaks a junction built by
// concatenating one quoted command string: the shell would split or mis-parse
// it. Each argument goes to mklink separately, so the path survives intact.
func TestLinkDependencyDirHandlesPathsWithSpaces(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "dep source")
	target := filepath.Join(root, "dep target")
	writeTestFile(t, filepath.Join(source, "left-pad", "index.js"), "pad")

	if err := linkDependencyDir(source, target); err != nil {
		t.Fatalf("linkDependencyDir with spaces in the path: %v", err)
	}
	body, err := os.ReadFile(filepath.Join(target, "left-pad", "index.js"))
	if err != nil {
		t.Fatalf("linked dependency unreadable: %v", err)
	}
	if string(body) != "pad" {
		t.Fatalf("linked content = %q, want the source content", body)
	}
	if runtime.GOOS == "windows" {
		// Without the symlink privilege the junction path is the one that made
		// this work; that is the case the separate-argument form protects.
		info, err := os.Lstat(target)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			t.Log("symlink privilege available; the junction fallback was not needed")
		}
	}
}

// Where no link can be created the tree is copied instead: the suite still sees
// the dependency, and the copy is independent of the source.
func TestCopyDependencyDirCopiesTheTree(t *testing.T) {
	source := t.TempDir()
	writeTestFile(t, filepath.Join(source, "left-pad", "index.js"), "pad")
	writeTestFile(t, filepath.Join(source, "nested", "deep", "x.js"), "deep")
	// The copy target is created inside a checkout that has no node_modules
	// yet, so it does not exist when the copy starts.
	target := filepath.Join(t.TempDir(), "node_modules")

	if err := copyDependencyDir(source, target); err != nil {
		t.Fatalf("copyDependencyDir: %v", err)
	}
	for _, rel := range []string{"left-pad/index.js", "nested/deep/x.js"} {
		if _, err := os.Stat(filepath.Join(target, rel)); err != nil {
			t.Fatalf("%s not copied: %v", rel, err)
		}
	}
	// Independent copy: writing through the target does not reach the source.
	if err := os.WriteFile(filepath.Join(target, "left-pad", "index.js"), []byte("changed"), 0o600); err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(filepath.Join(source, "left-pad", "index.js"))
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != "pad" {
		t.Fatalf("copy shares storage with the source: %q", body)
	}
}
