package sessionmanager

import (
	"context"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	aoprocess "github.com/aoagents/agent-orchestrator/backend/internal/process"
)

// CopyInstalledDependencies gives an isolated checkout its own copy of the
// dependency folders the workspace has already installed (node_modules,
// .venv), so a suite that needs them runs there without an installer.
//
// It copies instead of linking. A link leaves the suite writing through to the
// worker's live workspace — test caches, regenerated files, a prune — and a
// local package link inside node_modules would resolve back into the worker's
// checkout, where uncommitted edits are not part of the commit under test. A
// copy costs disk, but makes both impossible: the suite can only disturb its
// own throwaway checkout, and a link that named workspace source is rebased
// onto the copy's own committed files.
//
// This repeats no external side effect and runs no installer. A folder the
// target already has is left alone. Returns the repo-relative paths copied.
func CopyInstalledDependencies(ctx context.Context, sourcePath, workspacePath string) ([]string, error) {
	sourcePath = filepath.Clean(sourcePath)
	workspacePath = filepath.Clean(workspacePath)
	if sourcePath == workspacePath {
		return nil, nil
	}
	srcRoot, err := os.OpenRoot(sourcePath)
	if err != nil {
		return nil, err
	}
	defer func() { _ = srcRoot.Close() }()
	dstRoot, err := os.OpenRoot(workspacePath)
	if err != nil {
		return nil, err
	}
	defer func() { _ = dstRoot.Close() }()

	trees := dependencyTreeCopy{sourceWorkspace: sourcePath, targetWorkspace: workspacePath}
	var copied []string
	err = fs.WalkDir(srcRoot.FS(), ".", func(rel string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			// An unreadable folder cannot be copied; the suite will report
			// whatever it actually needs.
			return nil //nolint:nilerr // best effort, see the caller's docs
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		if !entry.IsDir() || rel == "." {
			return nil
		}
		if entry.Name() == ".git" || entry.Name() == ".ao" {
			return filepath.SkipDir
		}
		if strings.Count(rel, "/") >= forkDependencyDepth {
			return filepath.SkipDir
		}
		if !forkDependencyDirs[entry.Name()] {
			return nil
		}
		// Root-scoped, so a path assembled during the walk cannot resolve
		// outside the trees it was opened from.
		if _, statErr := dstRoot.Lstat(rel); statErr == nil {
			return filepath.SkipDir
		}
		if mkErr := dstRoot.MkdirAll(filepath.Dir(rel), 0o750); mkErr != nil {
			return fmt.Errorf("copy dependency %q: %w", rel, mkErr)
		}
		source := filepath.Join(sourcePath, rel)
		target := filepath.Join(workspacePath, rel)
		if copyErr := trees.copyTree(ctx, source, target); copyErr != nil {
			return fmt.Errorf("copy dependency %q: %w", rel, copyErr)
		}
		copied = append(copied, rel)
		return filepath.SkipDir
	})
	if err != nil {
		return copied, err
	}
	return copied, nil
}

// dependencyTreeCopy copies dependency trees out of one checkout into another
// and rebases the links it meets, which are what a workspace-local package
// layout is built from.
type dependencyTreeCopy struct {
	sourceWorkspace string
	targetWorkspace string
}

// copyTree copies one dependency tree into target, preserving its layout.
func (c dependencyTreeCopy) copyTree(ctx context.Context, source, target string) error {
	srcRoot, err := os.OpenRoot(source)
	if err != nil {
		return err
	}
	defer func() { _ = srcRoot.Close() }()
	// The target lives in a checkout that does not have this dependency folder
	// yet, and OpenRoot requires an existing directory.
	if err := os.MkdirAll(target, 0o750); err != nil {
		return err
	}
	dstRoot, err := os.OpenRoot(target)
	if err != nil {
		return err
	}
	defer func() { _ = dstRoot.Close() }()

	return fs.WalkDir(srcRoot.FS(), ".", func(rel string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		if rel == "." {
			return nil
		}
		if entry.Type().IsRegular() {
			return copyDependencyFile(srcRoot, dstRoot, rel)
		}
		// Anything else may be a link. A Windows junction does not report the
		// symlink bit and is not reported as a directory either, so a link is
		// recognised by asking for its target. Recreating it is required:
		// following it would copy whatever it points at, and a workspace-local
		// package junction would then bring the worker's uncommitted files in.
		if _, linkErr := srcRoot.Readlink(rel); linkErr == nil {
			if err := c.copyLink(srcRoot, rel, source, target); err != nil {
				return err
			}
			if entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if entry.IsDir() {
			return dstRoot.MkdirAll(rel, 0o750)
		}
		return nil
	})
}

// copyLink recreates one link inside the copy. A link that named source inside
// this checkout is rewritten to name the copy's own file instead: that is what
// keeps a workspace-local package pointing at the committed content under test
// rather than at the worker's possibly-dirty checkout.
func (c dependencyTreeCopy) copyLink(srcRoot *os.Root, rel, source, target string) error {
	stored, err := srcRoot.Readlink(rel)
	if err != nil {
		return err
	}
	linkPath := filepath.Join(target, rel)
	rebased := c.rebase(stored, filepath.Dir(filepath.Join(source, rel)), filepath.Dir(linkPath), source, target)
	return createDependencyLink(linkPath, rebased)
}

// rebase maps a link target onto the copy when it names a path inside the
// dependency tree being copied or inside the checkout that contains it. A link
// pointing anywhere else (a global store, an absolute system path) is kept
// verbatim: it is outside both trees, so it cannot reach the worker's
// workspace or this checkout.
func (c dependencyTreeCopy) rebase(stored, sourceDir, targetDir, source, target string) string {
	resolved := normalizeLinkTarget(stored)
	if !filepath.IsAbs(resolved) {
		resolved = filepath.Join(sourceDir, resolved)
	}
	resolved = filepath.Clean(resolved)

	roots := []struct{ from, to string }{
		{from: source, to: target},
		{from: c.sourceWorkspace, to: c.targetWorkspace},
	}
	for _, root := range roots {
		rel, ok := withinTree(root.from, resolved)
		if !ok {
			continue
		}
		mapped := filepath.Join(root.to, rel)
		if rebased, relErr := filepath.Rel(targetDir, mapped); relErr == nil {
			return rebased
		}
		return mapped
	}
	return stored
}

// normalizeLinkTarget strips the Windows NT object-path prefix a junction
// reports, so a junction target compares like the ordinary path a package
// manager writes.
func normalizeLinkTarget(stored string) string {
	if runtime.GOOS != "windows" {
		return stored
	}
	for _, prefix := range []string{`\\?\UNC\`, `\\?\`} {
		if strings.HasPrefix(stored, prefix) {
			rest := strings.TrimPrefix(stored, prefix)
			if prefix == `\\?\UNC\` {
				return `\\` + rest
			}
			return rest
		}
	}
	return stored
}

// withinTree reports path's location under root when it is inside it. The root
// itself counts, so a link that names a whole tree is rebased like any other:
// leaving it verbatim would keep it pointing at the worker's checkout.
func withinTree(root, path string) (string, bool) {
	rel, err := filepath.Rel(root, path)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", false
	}
	if filepath.IsAbs(rel) {
		return "", false
	}
	return rel, true
}

// createDependencyLink creates one directory link at linkPath. A symbolic link
// is preferred. On Windows, where creating one needs a privilege the daemon may
// not hold, a directory junction serves for an unprivileged user on the same
// volume; it needs an absolute target, so a relative one is resolved first.
func createDependencyLink(linkPath, stored string) error {
	symlinkErr := os.Symlink(stored, linkPath)
	if symlinkErr == nil {
		return nil
	}
	if runtime.GOOS != "windows" {
		return symlinkErr
	}
	resolved := stored
	if !filepath.IsAbs(resolved) {
		resolved = filepath.Join(filepath.Dir(linkPath), resolved)
	}
	resolved = filepath.Clean(resolved)
	if info, statErr := os.Stat(resolved); statErr != nil || !info.IsDir() {
		return symlinkErr
	}
	// Each argument is passed separately: a single quoted command string would
	// be escaped again by Go, and mklink would receive a path it cannot parse.
	cmd := aoprocess.Command("cmd", "/d", "/c", "mklink", "/J", linkPath, resolved)
	out, junctionErr := cmd.CombinedOutput()
	if junctionErr == nil {
		return nil
	}
	return fmt.Errorf("%w (junction: %s)", symlinkErr, strings.TrimSpace(string(out)))
}

// copyDependencyFile streams one file so a large dependency tree is never
// buffered whole in memory. Both sides are root-scoped, so the paths cannot
// escape the trees they were opened from.
func copyDependencyFile(srcRoot, dstRoot *os.Root, rel string) error {
	in, err := srcRoot.Open(rel)
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()
	info, err := in.Stat()
	if err != nil {
		return err
	}
	out, err := dstRoot.OpenFile(rel, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, info.Mode().Perm())
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return err
	}
	return out.Close()
}
