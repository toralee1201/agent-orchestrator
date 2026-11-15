package sessionmanager

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
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
			resolved := resolveLinkTarget(tc.stored, sourceDir)
			got, ok := trees.rebase(resolved, filepath.Join(targetDir, "app"), treeSource, treeTarget)
			if !ok {
				t.Fatalf("rebase(%q) reported no mapping, want %q", tc.stored, tc.want)
			}
			if got != tc.want {
				t.Fatalf("rebase(%q) = %q, want %q", tc.stored, got, tc.want)
			}
			mapped := got
			if !filepath.IsAbs(mapped) {
				mapped = filepath.Join(filepath.Join(targetDir, "app"), mapped)
			}
			if within, inWorker := withinTree(source, filepath.Clean(mapped)); inWorker {
				t.Fatalf("rebased link %q resolves to %q inside the worker workspace", got, within)
			}
		})
	}
}

// A link naming something outside both trees — a global package store or a
// system directory — is not rebased; the caller copies its content in instead.
func TestDependencyTreeCopyKeepsLinksOutsideBothTrees(t *testing.T) {
	source, target := t.TempDir(), t.TempDir()
	trees := dependencyTreeCopy{sourceWorkspace: source, targetWorkspace: target}
	treeSource := filepath.Join(source, "node_modules")
	treeTarget := filepath.Join(target, "node_modules")
	store := filepath.Join(t.TempDir(), "global-pnpm")

	resolved := resolveLinkTarget(store, treeSource)
	if got, ok := trees.rebase(resolved, filepath.Join(treeTarget, "x"), treeSource, treeTarget); ok {
		t.Fatalf("rebase(%q) = %q, want no mapping outside both trees", store, got)
	}
}

// A link that names neither checkout is copied in rather than carried over as a
// link, so the isolated checkout holds nothing that can reach a live path.
func TestCopyInstalledDependenciesCopiesLinksToOutsideDirectories(t *testing.T) {
	source, target := t.TempDir(), t.TempDir()
	external := filepath.Join(t.TempDir(), "global-pnpm", "left-pad")
	writeTestFile(t, filepath.Join(external, "index.js"), "globally installed")
	if err := os.MkdirAll(filepath.Join(source, "node_modules"), 0o750); err != nil {
		t.Fatal(err)
	}
	linkPath := filepath.Join(source, "node_modules", "left-pad")
	if err := createDependencyLink(linkPath, external); err != nil {
		t.Skipf("this host cannot create the link the test needs: %v", err)
	}

	if _, err := CopyInstalledDependencies(context.Background(), source, target); err != nil {
		t.Fatalf("CopyInstalledDependencies: %v", err)
	}
	copied := filepath.Join(target, "node_modules", "left-pad")
	root, err := os.OpenRoot(filepath.Join(target, "node_modules"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	if stored, linkErr := root.Readlink("left-pad"); linkErr == nil {
		t.Fatalf("copied dependency is a link to %q, want a real copy", stored)
	}
	body, err := os.ReadFile(filepath.Join(copied, "index.js"))
	if err != nil {
		t.Fatalf("copied dependency does not resolve: %v", err)
	}
	if string(body) != "globally installed" {
		t.Fatalf("copied dependency content = %q, want the linked content", body)
	}
}

// A relative link that climbs out of the workspace resolves against the source
// checkout, not the snapshot. Copied verbatim it would point at a path that may
// not exist next to the snapshot, so the content is copied in instead.
func TestCopyInstalledDependenciesCopiesRelativeLinksThatEscapeTheWorkspace(t *testing.T) {
	parent := t.TempDir()
	source, target := filepath.Join(parent, "worker"), filepath.Join(parent, "snapshot")
	external := filepath.Join(parent, "shared-store", "left-pad")
	writeTestFile(t, filepath.Join(external, "index.js"), "shared store")
	if err := os.MkdirAll(filepath.Join(source, "node_modules"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(target, 0o750); err != nil {
		t.Fatal(err)
	}
	linkPath := filepath.Join(source, "node_modules", "left-pad")
	escaping := filepath.Join("..", "..", "shared-store", "left-pad")
	if err := createDependencyLink(linkPath, escaping); err != nil {
		t.Skipf("this host cannot create the link the test needs: %v", err)
	}

	if _, err := CopyInstalledDependencies(context.Background(), source, target); err != nil {
		t.Fatalf("CopyInstalledDependencies: %v", err)
	}
	copied := filepath.Join(target, "node_modules", "left-pad")
	root, err := os.OpenRoot(filepath.Join(target, "node_modules"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	if stored, linkErr := root.Readlink("left-pad"); linkErr == nil {
		t.Fatalf("copied dependency is a link to %q, want a real copy", stored)
	}
	body, err := os.ReadFile(filepath.Join(copied, "index.js"))
	if err != nil {
		t.Fatalf("copied dependency does not resolve next to the snapshot: %v", err)
	}
	if string(body) != "shared store" {
		t.Fatalf("copied dependency content = %q, want the linked content", body)
	}
}

// A link can name an alias that is itself a link, with the chain finally
// reaching the worker's package. Classifying only the first target would copy
// uncommitted worker source into the snapshot, so the chain is followed and the
// workspace destination is rebased onto the snapshot's committed files.
func TestCopyInstalledDependenciesFollowsALinkChainToTheWorkspace(t *testing.T) {
	parent := t.TempDir()
	source, target := filepath.Join(parent, "worker"), filepath.Join(parent, "snapshot")
	writeTestFile(t, filepath.Join(source, "packages", "app", "index.js"), "worker uncommitted")
	writeTestFile(t, filepath.Join(target, "packages", "app", "index.js"), "snapshot committed")
	alias := filepath.Join(parent, "alias")
	if err := createDependencyLink(alias, filepath.Join(source, "packages", "app")); err != nil {
		t.Skipf("this host cannot create the alias the test needs: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(source, "node_modules"), 0o750); err != nil {
		t.Fatal(err)
	}
	linkPath := filepath.Join(source, "node_modules", "app")
	if err := createDependencyLink(linkPath, alias); err != nil {
		t.Skipf("this host cannot create the link the test needs: %v", err)
	}

	if _, err := CopyInstalledDependencies(context.Background(), source, target); err != nil {
		t.Fatalf("CopyInstalledDependencies: %v", err)
	}
	body, err := os.ReadFile(filepath.Join(target, "node_modules", "app", "index.js"))
	if err != nil {
		t.Fatalf("copied package link does not resolve: %v", err)
	}
	if string(body) != "snapshot committed" {
		t.Fatalf("link chain resolved to %q, want the snapshot's committed content", body)
	}
}

// An externally installed dependency can contain its own links. They are
// content of that dependency, so their targets are copied too: a nested
// directory link cannot be streamed as a file, and leaving it in place would
// point the suite outside the snapshot.
func TestCopyInstalledDependenciesCopiesNestedLinksInExternalDependencies(t *testing.T) {
	parent := t.TempDir()
	source, target := filepath.Join(parent, "worker"), filepath.Join(parent, "snapshot")
	external := filepath.Join(parent, "global-pnpm", "pkg")
	writeTestFile(t, filepath.Join(external, "index.js"), "pkg entry")
	writeTestFile(t, filepath.Join(parent, "global-pnpm", "shared", "util.js"), "shared util")
	writeTestFile(t, filepath.Join(external, "linked-file.js"), "real file")
	if err := os.MkdirAll(filepath.Join(source, "node_modules"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(target, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := createDependencyLink(filepath.Join(external, "shared"), filepath.Join("..", "shared")); err != nil {
		t.Skipf("this host cannot create the nested directory link: %v", err)
	}
	fileLinkable := createDependencyLink(filepath.Join(external, "file.js"), "linked-file.js") == nil
	if err := createDependencyLink(filepath.Join(source, "node_modules", "pkg"), external); err != nil {
		t.Skipf("this host cannot create the link the test needs: %v", err)
	}

	if _, err := CopyInstalledDependencies(context.Background(), source, target); err != nil {
		t.Fatalf("CopyInstalledDependencies: %v", err)
	}
	copied := filepath.Join(target, "node_modules", "pkg")
	root, err := os.OpenRoot(copied)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	if stored, linkErr := root.Readlink("shared"); linkErr == nil {
		t.Fatalf("nested directory link was kept as a link to %q, want a copy", stored)
	}
	if body, readErr := os.ReadFile(filepath.Join(copied, "shared", "util.js")); readErr != nil || string(body) != "shared util" {
		t.Fatalf("nested directory link content = %q, err = %v, want the copied subtree", body, readErr)
	}
	if !fileLinkable {
		t.Log("this host cannot create a file link; only the nested directory link was checked")
		return
	}
	if body, readErr := os.ReadFile(filepath.Join(copied, "file.js")); readErr != nil || string(body) != "real file" {
		t.Fatalf("nested file link content = %q, err = %v, want the copied file", body, readErr)
	}
}

// A dependency that links back to a directory already being copied must not
// recurse forever. The loop becomes a link to the ancestor's copy, so the
// layout is preserved and nothing points at a live path.
func TestCopyInstalledDependenciesClosesACycleWithAnIsolatedLink(t *testing.T) {
	parent := t.TempDir()
	source, target := filepath.Join(parent, "worker"), filepath.Join(parent, "snapshot")
	external := filepath.Join(parent, "global-pnpm", "pkg")
	writeTestFile(t, filepath.Join(external, "index.js"), "pkg entry")
	if err := os.MkdirAll(filepath.Join(source, "node_modules"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(target, 0o750); err != nil {
		t.Fatal(err)
	}
	// pkg/loop -> pkg, a cycle that a naive walk would follow forever.
	if err := createDependencyLink(filepath.Join(external, "loop"), "."); err != nil {
		t.Skipf("this host cannot create the cycle the test needs: %v", err)
	}
	if err := createDependencyLink(filepath.Join(source, "node_modules", "pkg"), external); err != nil {
		t.Skipf("this host cannot create the link the test needs: %v", err)
	}

	done := make(chan error, 1)
	go func() {
		_, copyErr := CopyInstalledDependencies(context.Background(), source, target)
		done <- copyErr
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("CopyInstalledDependencies: %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("CopyInstalledDependencies did not finish; the cycle was followed")
	}

	copied := filepath.Join(target, "node_modules", "pkg")
	body, err := os.ReadFile(filepath.Join(copied, "index.js"))
	if err != nil {
		t.Fatalf("copied dependency does not resolve: %v", err)
	}
	if string(body) != "pkg entry" {
		t.Fatalf("copied dependency content = %q, want the original entry", body)
	}
	if _, err := os.Stat(filepath.Join(copied, "loop", "index.js")); err != nil {
		t.Fatalf("cycle link does not resolve inside the snapshot: %v", err)
	}
	if target, err := filepath.EvalSymlinks(filepath.Join(copied, "loop")); err == nil {
		if within, ok := withinTree(external, target); ok {
			t.Fatalf("cycle link resolves outside the snapshot to %q", within)
		}
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
