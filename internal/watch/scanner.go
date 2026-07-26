// Package watch owns project-file discovery, hashing, and filesystem change
// observation. It is the only package allowed to import fsnotify.
package watch

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/fingerskier/langer/internal/procx"
	"github.com/fingerskier/langer/protocol"
)

// Scanner is the single definition of SPEC §3.4 project-file scope.
type Scanner interface {
	RepositoryNamespace(ctx context.Context, root string) (string, error)
	List(ctx context.Context, root string) ([]string, error)
	InScope(root, rel string) bool
	Hash(abs string) (string, error)
}

// NewScanner returns a scanner that resolves and runs only a system Git
// executable. Git is an optional scope oracle; no network operation is made.
func NewScanner(resolver procx.Resolver, runner procx.Runner) Scanner {
	return &scanner{resolver: resolver, runner: runner}
}

type scanner struct {
	resolver procx.Resolver
	runner   procx.Runner
}

var deniedDirectories = map[string]struct{}{
	"node_modules": {},
	"vendor":       {},
	"target":       {},
	".venv":        {},
	"venv":         {},
	"dist":         {},
	"build":        {},
	".git":         {},
	"__pycache__":  {},
	".tox":         {},
	".mypy_cache":  {},
}

func (s *scanner) RepositoryNamespace(ctx context.Context, root string) (string, error) {
	canonical, err := canonicalRoot(root)
	if err != nil {
		return "", err
	}
	if err := ctx.Err(); err != nil {
		return "", protocol.NewErrorf(protocol.ErrNotReady, "repository namespace cancelled: %v", err)
	}

	gitPath, ok := s.gitPath(canonical)
	if !ok {
		return canonical, nil
	}
	if !s.isGitRepository(ctx, gitPath, canonical) {
		if err := ctx.Err(); err != nil {
			return "", protocol.NewErrorf(protocol.ErrNotReady, "repository namespace cancelled: %v", err)
		}
		return canonical, nil
	}

	out, err := s.runner.Output(ctx, procx.Spec{
		Path: gitPath,
		Args: []string{"-C", canonical, "config", "--get", "remote.origin.url"},
		Dir:  canonical,
	})
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return "", protocol.NewErrorf(protocol.ErrNotReady, "repository namespace cancelled: %v", ctxErr)
		}
		return canonical, nil
	}
	if namespace := normalizeRemoteSlug(string(out)); namespace != "" {
		return namespace, nil
	}
	return canonical, nil
}

func (s *scanner) List(ctx context.Context, root string) ([]string, error) {
	canonical, err := canonicalRoot(root)
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, protocol.NewErrorf(protocol.ErrNotReady, "workspace scan cancelled: %v", err)
	}

	if gitPath, ok := s.gitPath(canonical); ok && s.isGitRepository(ctx, gitPath, canonical) {
		out, err := s.runner.Output(ctx, procx.Spec{
			Path: gitPath,
			Args: []string{"-C", canonical, "ls-files", "--cached", "--others", "--exclude-standard", "-z"},
			Dir:  canonical,
		})
		if err != nil {
			return nil, protocol.AsError(err)
		}
		return s.filterGitFiles(canonical, out), nil
	}
	if err := ctx.Err(); err != nil {
		return nil, protocol.NewErrorf(protocol.ErrNotReady, "workspace scan cancelled: %v", err)
	}
	return s.walk(ctx, canonical)
}

// WatchDirectories returns the smallest ignore-aware recursive directory set
// that still observes creation of the first project file in an empty
// directory. It is intentionally an optional extension to Scanner: Watcher
// uses it when available and otherwise falls back to project-file ancestors.
func (s *scanner) WatchDirectories(
	ctx context.Context,
	root string,
	projectFiles []string,
) ([]string, error) {
	canonical, err := canonicalRoot(root)
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, protocol.NewErrorf(protocol.ErrNotReady, "watch directory scan cancelled: %v", err)
	}

	ignored := make(map[string]struct{})
	if gitPath, ok := s.gitPath(canonical); ok && s.isGitRepository(ctx, gitPath, canonical) {
		out, err := s.runner.Output(ctx, procx.Spec{
			Path: gitPath,
			Args: []string{
				"-C", canonical,
				"ls-files", "--others", "--ignored", "--exclude-standard", "--directory", "-z",
			},
			Dir: canonical,
		})
		if err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return nil, protocol.NewErrorf(
					protocol.ErrNotReady,
					"watch directory scan cancelled: %v",
					ctxErr,
				)
			}
			return nil, protocol.AsError(err)
		}
		ignored = ignoredDirectoryRoots(out)
	}

	required := requiredDirectoryAncestors(projectFiles)
	seen := make(map[string]struct{})
	err = filepath.WalkDir(canonical, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if path == canonical {
			seen["."] = struct{}{}
			return nil
		}
		if entry.Type()&os.ModeSymlink != 0 {
			if entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if !entry.IsDir() {
			return nil
		}

		rel, err := filepath.Rel(canonical, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(filepath.Clean(rel))
		if !s.InScope(canonical, rel) {
			return filepath.SkipDir
		}
		if insideIgnoredDirectory(rel, ignored) {
			if _, needed := required[rel]; !needed {
				return filepath.SkipDir
			}
		}
		seen[rel] = struct{}{}
		return nil
	})
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, protocol.NewErrorf(
				protocol.ErrNotReady,
				"watch directory scan cancelled: %v",
				ctxErr,
			)
		}
		return nil, protocol.NewErrorf(
			protocol.ErrInternal,
			"walking watch directories under %s: %v",
			canonical,
			err,
		)
	}
	return sortedKeys(seen), nil
}

func (s *scanner) InScope(root, path string) bool {
	if strings.TrimSpace(path) == "" {
		return false
	}
	if filepath.IsAbs(path) {
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return false
		}
		path = rel
	}
	path = filepath.Clean(filepath.FromSlash(strings.ReplaceAll(path, `\`, `/`)))
	if path == "." || path == ".." || filepath.IsAbs(path) {
		return false
	}
	for _, part := range strings.FieldsFunc(filepath.ToSlash(path), func(r rune) bool { return r == '/' }) {
		if part == ".." {
			return false
		}
		for denied := range deniedDirectories {
			if strings.EqualFold(part, denied) {
				return false
			}
		}
	}
	return true
}

func ignoredDirectoryRoots(out []byte) map[string]struct{} {
	roots := make(map[string]struct{})
	for _, raw := range strings.Split(string(out), "\x00") {
		slashed := strings.ReplaceAll(raw, `\`, "/")
		if !strings.HasSuffix(slashed, "/") {
			continue
		}
		rel := filepath.ToSlash(filepath.Clean(filepath.FromSlash(strings.TrimSuffix(slashed, "/"))))
		if rel == "." || rel == ".." || strings.HasPrefix(rel, "../") {
			continue
		}
		roots[rel] = struct{}{}
	}
	return roots
}

func requiredDirectoryAncestors(projectFiles []string) map[string]struct{} {
	required := map[string]struct{}{".": {}}
	for _, file := range projectFiles {
		rel := filepath.ToSlash(filepath.Clean(filepath.FromSlash(file)))
		if rel == "." || rel == ".." || strings.HasPrefix(rel, "../") || filepath.IsAbs(rel) {
			continue
		}
		for dir := filepath.ToSlash(filepath.Dir(filepath.FromSlash(rel))); dir != "."; {
			required[dir] = struct{}{}
			parent := filepath.ToSlash(filepath.Dir(filepath.FromSlash(dir)))
			if parent == dir {
				break
			}
			dir = parent
		}
	}
	return required
}

func insideIgnoredDirectory(rel string, ignored map[string]struct{}) bool {
	for candidate := rel; candidate != "." && candidate != ""; {
		if _, found := ignored[candidate]; found {
			return true
		}
		slash := strings.LastIndexByte(candidate, '/')
		if slash < 0 {
			break
		}
		candidate = candidate[:slash]
	}
	return false
}

func (*scanner) Hash(abs string) (string, error) {
	f, err := os.Open(abs)
	if err != nil {
		return "", protocol.NewErrorf(protocol.ErrInternal, "opening %s for hashing: %v", abs, err)
	}
	defer f.Close()

	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", protocol.NewErrorf(protocol.ErrInternal, "hashing %s: %v", abs, err)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func (s *scanner) gitPath(root string) (string, bool) {
	if s.resolver == nil || s.runner == nil {
		return "", false
	}
	path, err := s.resolver.Resolve("git", root, false)
	if err != nil || !filepath.IsAbs(path) {
		return "", false
	}
	return path, true
}

func (s *scanner) isGitRepository(ctx context.Context, gitPath, root string) bool {
	out, err := s.runner.Output(ctx, procx.Spec{
		Path: gitPath,
		Args: []string{"-C", root, "rev-parse", "--is-inside-work-tree"},
		Dir:  root,
	})
	return err == nil && strings.TrimSpace(string(out)) == "true"
}

func (s *scanner) filterGitFiles(root string, out []byte) []string {
	seen := make(map[string]struct{})
	for _, raw := range strings.Split(string(out), "\x00") {
		rel := filepath.ToSlash(filepath.Clean(filepath.FromSlash(raw)))
		if raw == "" || !s.InScope(root, rel) {
			continue
		}
		if !regularFileWithoutSymlink(root, rel) {
			continue
		}
		seen[rel] = struct{}{}
	}
	return sortedKeys(seen)
}

// regularFileWithoutSymlink validates every component, not just the leaf.
// Lstat("root/link/file") follows an intermediate `link` directory and can
// otherwise make a tracked-looking path read a file outside the workspace.
func regularFileWithoutSymlink(root, rel string) bool {
	parts := strings.Split(filepath.Clean(filepath.FromSlash(rel)), string(filepath.Separator))
	current := root
	for i, part := range parts {
		if part == "" || part == "." || part == ".." {
			return false
		}
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		if err != nil || info.Mode()&os.ModeSymlink != 0 {
			return false
		}
		if i < len(parts)-1 {
			if !info.IsDir() {
				return false
			}
			continue
		}
		return info.Mode().IsRegular()
	}
	return false
}

func (s *scanner) walk(ctx context.Context, root string) ([]string, error) {
	seen := make(map[string]struct{})
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if path == root {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		if !s.InScope(root, rel) {
			if entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if entry.Type()&os.ModeSymlink != 0 || entry.IsDir() {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if info.Mode().IsRegular() {
			seen[filepath.ToSlash(rel)] = struct{}{}
		}
		return nil
	})
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, protocol.NewErrorf(protocol.ErrNotReady, "workspace scan cancelled: %v", ctxErr)
		}
		return nil, protocol.NewErrorf(protocol.ErrInternal, "walking workspace %s: %v", root, err)
	}
	return sortedKeys(seen), nil
}

func canonicalRoot(root string) (string, error) {
	abs, err := filepath.Abs(root)
	if err != nil {
		return "", protocol.NewErrorf(protocol.ErrInternal, "resolving workspace root %q: %v", root, err)
	}
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		abs = resolved
	}
	return filepath.Clean(abs), nil
}

func normalizeRemoteSlug(remote string) string {
	remote = strings.TrimSpace(remote)
	if remote == "" {
		return ""
	}
	// Git accepts a local Windows path as an origin even when langer is
	// currently running on Unix (for example against a checked-out shared
	// volume). Its drive colon is not scp syntax and must not turn
	// C:\git\org\repo into the misleading portable namespace git/org/repo.
	if len(remote) >= 3 &&
		((remote[0] >= 'A' && remote[0] <= 'Z') || (remote[0] >= 'a' && remote[0] <= 'z')) &&
		remote[1] == ':' && (remote[2] == '/' || remote[2] == '\\') {
		return ""
	}

	var path string
	if strings.Contains(remote, "://") {
		u, err := url.Parse(remote)
		if err != nil || strings.EqualFold(u.Scheme, "file") || u.Host == "" {
			return ""
		}
		path = u.Path
	} else {
		colon := strings.IndexByte(remote, ':')
		if colon <= 0 || strings.ContainsAny(remote[:colon], `/\`) {
			return ""
		}
		path = remote[colon+1:]
	}

	if decoded, err := url.PathUnescape(path); err == nil {
		path = decoded
	}
	path = strings.ReplaceAll(path, `\`, "/")
	path = strings.Trim(path, "/")
	path = strings.TrimSuffix(path, ".git")
	parts := strings.Split(path, "/")
	if len(parts) < 2 {
		return ""
	}
	for _, part := range parts {
		if part == "" || part == "." || part == ".." {
			return ""
		}
	}
	return strings.Join(parts, "/")
}

func sortedKeys(set map[string]struct{}) []string {
	out := make([]string, 0, len(set))
	for value := range set {
		out = append(out, value)
	}
	sort.Strings(out)
	return out
}
