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

// CarryInstalledDependencies makes the dependency folders a workspace has
// already installed visible in another checkout by linking them in, instead of
// installing them again. The folders are the git-ignored ones a package
// manager writes (node_modules, .venv), so they are never part of a commit and
// linking them cannot change what a commit contains.
//
// This is the safe alternative to replaying the project's post-create Setup
// steps when an isolated checkout has to run a suite that needs an installed
// dependency tree: it repeats no external side effects, runs no installer, and
// writes nothing outside the target checkout. A folder the target already has
// is left alone. Returns the repo-relative paths that were linked.
func CarryInstalledDependencies(ctx context.Context, sourcePath, workspacePath string) ([]string, error) {
	sourcePath = filepath.Clean(sourcePath)
	workspacePath = filepath.Clean(workspacePath)
	if sourcePath == workspacePath {
		return nil, nil
	}
	var linked []string
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

	err = fs.WalkDir(srcRoot.FS(), ".", func(rel string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			// An unreadable folder cannot be linked; the suite will report
			// whatever it actually needs.
			return nil //nolint:nilerr // best effort, see the callers' docs
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
		// Both checks are root-scoped, so a path built during the walk cannot
		// resolve outside the trees it was opened from.
		if _, statErr := dstRoot.Lstat(rel); statErr == nil {
			return filepath.SkipDir
		}
		if mkErr := dstRoot.MkdirAll(filepath.Dir(rel), 0o750); mkErr != nil {
			return fmt.Errorf("link dependency %q: %w", rel, mkErr)
		}
		if linkErr := linkDependencyDir(filepath.Join(sourcePath, rel), filepath.Join(workspacePath, rel)); linkErr != nil {
			return fmt.Errorf("link dependency %q: %w", rel, linkErr)
		}
		linked = append(linked, rel)
		return filepath.SkipDir
	})
	if err != nil {
		return linked, err
	}
	return linked, nil
}

// linkDependencyDir points target at source without installing anything. A
// symbolic link is preferred. On Windows, where creating one needs a privilege
// the daemon may not hold, a directory junction works for an unelevated user on
// the same volume. Where neither can be created (a filesystem without links),
// the tree is copied: slower and heavier, but still no installer runs and
// nothing outside the target checkout is written.
func linkDependencyDir(source, target string) error {
	symlinkErr := os.Symlink(source, target)
	if symlinkErr == nil {
		return nil
	}
	if runtime.GOOS != "windows" {
		return copyDependencyDir(source, target)
	}
	// Each argument is passed separately: a single quoted command string would
	// be escaped again by Go, and mklink would receive a path it cannot parse.
	cmd := aoprocess.Command("cmd", "/d", "/c", "mklink", "/J", target, source)
	out, junctionErr := cmd.CombinedOutput()
	if junctionErr == nil {
		return nil
	}
	if copyErr := copyDependencyDir(source, target); copyErr != nil {
		return fmt.Errorf("%w (junction: %s; copy: %w)", symlinkErr, strings.TrimSpace(string(out)), copyErr)
	}
	return nil
}

// copyDependencyDir copies a dependency tree into target, preserving the
// relative layout. Called only when no link can be created, and only for a
// git-ignored folder, so the copy never changes what a commit contains.
//
// Every operation goes through a root scoped to source or target, so a path
// built inside the walk cannot resolve outside the tree being copied even if a
// symlink appears mid-walk.
func copyDependencyDir(source, target string) error {
	srcRoot, err := os.OpenRoot(source)
	if err != nil {
		return err
	}
	defer func() { _ = srcRoot.Close() }()
	// The caller links into a folder the checkout does not have yet, so the
	// copy target may not exist. OpenRoot requires an existing directory.
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
		if rel == "." {
			return nil
		}
		if entry.IsDir() {
			return dstRoot.MkdirAll(rel, 0o750)
		}
		if entry.Type()&os.ModeSymlink != 0 {
			linkTarget, readErr := srcRoot.Readlink(rel)
			if readErr != nil {
				return readErr
			}
			return dstRoot.Symlink(linkTarget, rel)
		}
		return copyDependencyFile(srcRoot, dstRoot, rel)
	})
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
