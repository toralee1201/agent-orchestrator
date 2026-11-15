package sessionmanager

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// An isolated checkout gets its own copy of the dependency folders the worker's
// workspace already installed, with no installer running.
func TestCopyInstalledDependenciesCopiesInstalledFolders(t *testing.T) {
	source, target := t.TempDir(), t.TempDir()
	writeTestFile(t, filepath.Join(source, "node_modules", "left-pad", "index.js"), "pad")
	writeTestFile(t, filepath.Join(source, "packages", "app", "node_modules", "react", "index.js"), "react")
	writeTestFile(t, filepath.Join(source, ".venv", "bin", "python"), "python")
	writeTestFile(t, filepath.Join(source, "src", "main.go"), "package main")
	if err := os.MkdirAll(filepath.Join(target, "packages", "app"), 0o755); err != nil {
		t.Fatal(err)
	}

	copied, err := CopyInstalledDependencies(context.Background(), source, target)
	if err != nil {
		t.Fatalf("CopyInstalledDependencies: %v", err)
	}
	for _, rel := range []string{"node_modules/left-pad/index.js", "packages/app/node_modules/react/index.js", ".venv/bin/python"} {
		if _, err := os.Stat(filepath.Join(target, rel)); err != nil {
			t.Fatalf("%s not copied: %v", rel, err)
		}
	}
	if len(copied) != 3 {
		t.Fatalf("copied = %v, want the three dependency folders", copied)
	}
	// Only dependencies: tracked content stays whatever git checked out.
	if _, err := os.Stat(filepath.Join(target, "src", "main.go")); !os.IsNotExist(err) {
		t.Fatalf("tracked file copied: err=%v", err)
	}
}

// The copy must be independent. A suite that writes to its dependencies would
// otherwise corrupt the worker's live workspace mid-run.
func TestCopyInstalledDependenciesIsolatesWritesFromTheWorkspace(t *testing.T) {
	source, target := t.TempDir(), t.TempDir()
	writeTestFile(t, filepath.Join(source, "node_modules", "left-pad", "index.js"), "pad")

	if _, err := CopyInstalledDependencies(context.Background(), source, target); err != nil {
		t.Fatalf("CopyInstalledDependencies: %v", err)
	}
	copied := filepath.Join(target, "node_modules", "left-pad", "index.js")
	if err := os.WriteFile(copied, []byte("rewritten by the suite"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(filepath.Join(target, "node_modules")); err != nil {
		t.Fatal(err)
	}

	original := filepath.Join(source, "node_modules", "left-pad", "index.js")
	body, err := os.ReadFile(original)
	if err != nil {
		t.Fatalf("the worker's dependency folder did not survive: %v", err)
	}
	if string(body) != "pad" {
		t.Fatalf("worker dependency content = %q, want the untouched %q", body, "pad")
	}
}

// The copy is not a link, so there is nothing for the suite to write through.
// The check follows the target rather than the mode bit: a Windows junction
// carries neither the symlink bit nor a directory type.
func TestCopyInstalledDependenciesCreatesNoLinksToTheWorkspace(t *testing.T) {
	source, target := t.TempDir(), t.TempDir()
	writeTestFile(t, filepath.Join(source, "node_modules", "left-pad", "index.js"), "pad")

	if _, err := CopyInstalledDependencies(context.Background(), source, target); err != nil {
		t.Fatalf("CopyInstalledDependencies: %v", err)
	}
	copied := filepath.Join(target, "node_modules")
	root, err := os.OpenRoot(target)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	if got, linkErr := root.Readlink("node_modules"); linkErr == nil {
		t.Fatalf("dependency folder %s is a link to %q, so the suite writes through to the workspace", copied, got)
	}
}

// A workspace-local package link (the shape pnpm and npm workspaces use) names
// source inside the worker's checkout. Copied verbatim it would point the suite
// at the worker's uncommitted tree, so it is rebased onto the copy's own files.
func TestDependencyTreeCopyRebasesLinksThatNameTheWorkspace(t *testing.T) {
	source, target := t.TempDir(), t.TempDir()
	trees := dependencyTreeCopy{sourceWorkspace: source, targetWorkspace: target}
	treeSource := filepath.Join(source, "node_modules")
	treeTarget := filepath.Join(target, "node_modules")
	sourceDir := treeSource
	targetDir := treeTarget

	cases := []struct {
		name   string
		stored string
		want   string
	}{
		{
			// node_modules/app -> ../packages/app, the pnpm/npm-workspaces shape.
			name:   "relative link into the workspace",
			stored: filepath.Join("..", "packages", "app"),
			want:   filepath.Join("..", "packages", "app"),
		},
		{
			// An install that wrote an absolute path into the same checkout.
			name:   "absolute link into the workspace",
			stored: filepath.Join(source, "packages", "app"),
			want:   filepath.Join("..", "packages", "app"),
		},
		{
			// A link inside the dependency tree itself follows the copy.
			name:   "relative link inside the copied tree",
			stored: filepath.Join(".pnpm", "left-pad@1.0.0", "node_modules", "left-pad"),
			want:   filepath.Join(".pnpm", "left-pad@1.0.0", "node_modules", "left-pad"),
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := trees.rebase(tc.stored, sourceDir, targetDir, treeSource, treeTarget)
			if got != tc.want {
				t.Fatalf("rebase(%q) = %q, want %q", tc.stored, got, tc.want)
			}
			resolved := got
			if !filepath.IsAbs(resolved) {
				resolved = filepath.Join(targetDir, resolved)
			}
			if within, ok := withinTree(source, filepath.Clean(resolved)); ok {
				t.Fatalf("rebased link %q resolves to %q inside the worker workspace", got, within)
			}
		})
	}
}

// A link naming something outside both trees — a global package store or a
// system directory — cannot reach the worker's workspace, so it is kept as-is.
func TestDependencyTreeCopyKeepsLinksOutsideBothTrees(t *testing.T) {
	source, target := t.TempDir(), t.TempDir()
	trees := dependencyTreeCopy{sourceWorkspace: source, targetWorkspace: target}
	treeSource := filepath.Join(source, "node_modules")
	treeTarget := filepath.Join(target, "node_modules")
	store := filepath.Join(t.TempDir(), "global-pnpm")

	got := trees.rebase(store, treeSource, treeTarget, treeSource, treeTarget)
	if got != store {
		t.Fatalf("rebase(%q) = %q, want it kept verbatim", store, got)
	}
}

// With links available, the copy rewrites the workspace-local package link and
// the suite reads the snapshot's committed content.
func TestCopyInstalledDependenciesRebasesAWorkspaceLocalPackageLink(t *testing.T) {
	source, target := t.TempDir(), t.TempDir()
	writeTestFile(t, filepath.Join(source, "packages", "app", "index.js"), "worker uncommitted")
	writeTestFile(t, filepath.Join(target, "packages", "app", "index.js"), "snapshot committed")
	if err := os.MkdirAll(filepath.Join(source, "node_modules"), 0o750); err != nil {
		t.Fatal(err)
	}
	linkPath := filepath.Join(source, "node_modules", "app")
	if err := createDependencyLink(linkPath, filepath.Join("..", "packages", "app")); err != nil {
		t.Skipf("this host cannot create the workspace-local link the test needs: %v", err)
	}

	if _, err := CopyInstalledDependencies(context.Background(), source, target); err != nil {
		t.Fatalf("CopyInstalledDependencies: %v", err)
	}
	copiedLink := filepath.Join(target, "node_modules", "app")
	if _, err := os.Lstat(copiedLink); err != nil {
		t.Fatalf("dependency link was not carried into the copy: %v", err)
	}
	body, err := os.ReadFile(filepath.Join(copiedLink, "index.js"))
	if err != nil {
		t.Fatalf("copied package link does not resolve: %v", err)
	}
	if string(body) != "snapshot committed" {
		t.Fatalf("package link resolved to %q, want the snapshot's committed content", body)
	}
}

// A folder the target already provides is not replaced, and its content is
// what the suite will see.
func TestCopyInstalledDependenciesKeepsAnExistingFolder(t *testing.T) {
	source, target := t.TempDir(), t.TempDir()
	writeTestFile(t, filepath.Join(source, "node_modules", "a.js"), "source")
	writeTestFile(t, filepath.Join(target, "node_modules", "a.js"), "target")

	if _, err := CopyInstalledDependencies(context.Background(), source, target); err != nil {
		t.Fatalf("CopyInstalledDependencies: %v", err)
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
func TestCopyInstalledDependenciesWithoutInstalledFolders(t *testing.T) {
	source, target := t.TempDir(), t.TempDir()
	writeTestFile(t, filepath.Join(source, "src", "main.go"), "package main")

	copied, err := CopyInstalledDependencies(context.Background(), source, target)
	if err != nil {
		t.Fatalf("CopyInstalledDependencies: %v", err)
	}
	if len(copied) != 0 {
		t.Fatalf("copied = %v, want nothing", copied)
	}
	entries, err := os.ReadDir(target)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("target gained entries: %v", entries)
	}
}

// A dependency path with a space in it must survive the Windows junction
// fallback, which receives each argument separately.
func TestCreateDependencyLinkHandlesPathsWithSpaces(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "dep source")
	target := filepath.Join(root, "dep target")
	writeTestFile(t, filepath.Join(source, "left-pad", "index.js"), "pad")

	if err := createDependencyLink(target, source); err != nil {
		t.Fatalf("createDependencyLink with spaces in the path: %v", err)
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
