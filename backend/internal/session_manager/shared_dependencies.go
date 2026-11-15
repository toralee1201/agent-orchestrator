package sessionmanager

import (
	"context"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
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
	err := filepath.WalkDir(sourcePath, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			// An unreadable folder cannot be linked; the suite will report
			// whatever it actually needs.
			return nil //nolint:nilerr // best effort, see the callers' docs
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		if !entry.IsDir() || path == sourcePath {
			return nil
		}
		rel, relErr := filepath.Rel(sourcePath, path)
		if relErr != nil {
			return nil //nolint:nilerr // a path outside the source is not ours to link
		}
		if entry.Name() == ".git" || entry.Name() == ".ao" {
			return filepath.SkipDir
		}
		if strings.Count(rel, string(filepath.Separator)) >= forkDependencyDepth {
			return filepath.SkipDir
		}
		if !forkDependencyDirs[entry.Name()] {
			return nil
		}
		target := filepath.Join(workspacePath, rel)
		if _, statErr := os.Lstat(target); statErr == nil {
			return filepath.SkipDir
		}
		if mkErr := os.MkdirAll(filepath.Dir(target), 0o750); mkErr != nil {
			return fmt.Errorf("link dependency %q: %w", rel, mkErr)
		}
		if linkErr := linkDependencyDir(path, target); linkErr != nil {
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
	cmd := exec.Command("cmd", "/d", "/c", "mklink", "/J", target, source)
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
func copyDependencyDir(source, target string) error {
	return filepath.WalkDir(source, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, relErr := filepath.Rel(source, path)
		if relErr != nil {
			return relErr
		}
		dst := filepath.Join(target, rel)
		if entry.IsDir() {
			return os.MkdirAll(dst, 0o750)
		}
		if entry.Type()&os.ModeSymlink != 0 {
			linkTarget, readErr := os.Readlink(path)
			if readErr != nil {
				return readErr
			}
			return os.Symlink(linkTarget, dst)
		}
		return copyDependencyFile(path, dst)
	})
}

// copyDependencyFile streams one file so a large dependency tree is never
// buffered whole in memory.
func copyDependencyFile(source, target string) error {
	in, err := os.Open(source)
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()
	info, err := in.Stat()
	if err != nil {
		return err
	}
	out, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, info.Mode().Perm())
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return err
	}
	return out.Close()
}
