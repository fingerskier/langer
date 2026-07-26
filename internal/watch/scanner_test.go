package watch_test

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/fingerskier/langer/internal/procx"
	"github.com/fingerskier/langer/internal/watch"
)

type fakeResolver struct {
	path string
	err  error
}

func (f fakeResolver) Resolve(command, root string, allowWorkspaceLocal bool) (string, error) {
	if command != "git" {
		panic("unexpected command: " + command)
	}
	if allowWorkspaceLocal {
		panic("scanner allowed a workspace-local git binary")
	}
	return f.path, f.err
}

type fakeRunner struct {
	outputs [][]byte
	specs   []procx.Spec
	err     error
}

func (f *fakeRunner) Start(context.Context, procx.Spec) (procx.Process, error) {
	panic("scanner must use Output, not Start")
}

func (f *fakeRunner) Output(_ context.Context, spec procx.Spec) ([]byte, error) {
	f.specs = append(f.specs, spec)
	if f.err != nil {
		return nil, f.err
	}
	if len(f.outputs) == 0 {
		panic("unexpected Output call")
	}
	out := f.outputs[0]
	f.outputs = f.outputs[1:]
	return out, nil
}

func TestScannerInScopeUsesOneProjectFilePredicate(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	sc := watch.NewScanner(fakeResolver{}, &fakeRunner{})

	tests := []struct {
		path string
		want bool
	}{
		{"src/user.ts", true},
		{"src/nested/user.py", true},
		{"node_modules/pkg/index.ts", false},
		{"vendor/pkg/file.go", false},
		{"target/debug/generated.rs", false},
		{".venv/lib/site.py", false},
		{"venv/lib/site.py", false},
		{"dist/app.js", false},
		{"build/generated.py", false},
		{".git/config", false},
		{"src/__pycache__/user.pyc", false},
		{".tox/py/bin/python", false},
		{".mypy_cache/data.json", false},
		{"../escape.ts", false},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.path, func(t *testing.T) {
			t.Parallel()
			if got := sc.InScope(root, tt.path); got != tt.want {
				t.Fatalf("InScope(%q) = %v, want %v", tt.path, got, tt.want)
			}
		})
	}
}

func TestScannerHashIsSHA256OfFileBytes(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	path := filepath.Join(root, "file.ts")
	if err := os.WriteFile(path, []byte("hello\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	sc := watch.NewScanner(fakeResolver{}, &fakeRunner{})
	got, err := sc.Hash(path)
	if err != nil {
		t.Fatalf("Hash: %v", err)
	}
	const want = "5891b5b522d5df086d0ff0b110fbd9d21bb4fc7163af34d08286a2e846f6be03"
	if got != want {
		t.Fatalf("Hash = %q, want %q", got, want)
	}
}

func TestScannerListUsesResolvedSystemGitAndFiltersTrackedDependencies(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writeFiles(t, root,
		"src/a.ts",
		"src/untracked.py",
		"node_modules/committed/index.ts",
	)

	gitPath := filepath.Join(t.TempDir(), "git")
	runner := &fakeRunner{outputs: [][]byte{
		[]byte("true\n"),
		[]byte("src/a.ts\x00src/untracked.py\x00node_modules/committed/index.ts\x00src/a.ts\x00"),
	}}
	sc := watch.NewScanner(fakeResolver{path: gitPath}, runner)

	got, err := sc.List(context.Background(), root)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	want := []string{"src/a.ts", "src/untracked.py"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("List = %#v, want %#v", got, want)
	}

	if len(runner.specs) != 2 {
		t.Fatalf("git calls = %d, want 2", len(runner.specs))
	}
	for _, spec := range runner.specs {
		if spec.Path != gitPath || !filepath.IsAbs(spec.Path) {
			t.Fatalf("git path = %q, want resolved absolute %q", spec.Path, gitPath)
		}
		if spec.Dir != root {
			t.Fatalf("git dir = %q, want %q", spec.Dir, root)
		}
	}
	if got := strings.Join(runner.specs[1].Args, " "); got != "-C "+root+" ls-files --cached --others --exclude-standard -z" {
		t.Fatalf("ls-files args = %q", got)
	}
}

func TestScannerListRejectsTrackedFileBelowSymlinkedDirectory(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	outside := t.TempDir()
	writeFiles(t, outside, "secret.ts")
	if err := os.Symlink(outside, filepath.Join(root, "link")); err != nil {
		t.Skipf("creating directory symlink: %v", err)
	}

	gitPath := filepath.Join(t.TempDir(), "git")
	runner := &fakeRunner{outputs: [][]byte{
		[]byte("true\n"),
		[]byte("link/secret.ts\x00"),
	}}
	sc := watch.NewScanner(fakeResolver{path: gitPath}, runner)

	got, err := sc.List(context.Background(), root)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("List followed a symlinked directory outside the workspace: %#v", got)
	}
}

func TestScannerRepositoryNamespaceNormalizesOriginWithoutNetwork(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		remote string
		want   string
	}{
		{"https", "https://github.com/fingerskier/langer.git\n", "fingerskier/langer"},
		{"ssh URL", "ssh://git@gitlab.example/group/subgroup/repo.git\n", "group/subgroup/repo"},
		{"scp", "git@github.com:fingerskier/langer.git\n", "fingerskier/langer"},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			root := t.TempDir()
			runner := &fakeRunner{outputs: [][]byte{
				[]byte("true\n"),
				[]byte(tt.remote),
			}}
			sc := watch.NewScanner(fakeResolver{path: filepath.Join(t.TempDir(), "git")}, runner)
			got, err := sc.RepositoryNamespace(context.Background(), root)
			if err != nil {
				t.Fatalf("RepositoryNamespace: %v", err)
			}
			if got != tt.want {
				t.Fatalf("RepositoryNamespace = %q, want %q", got, tt.want)
			}
			if len(runner.specs) != 2 {
				t.Fatalf("git calls = %d, want 2", len(runner.specs))
			}
		})
	}
}

func TestScannerRepositoryNamespaceFallsBackToCanonicalRoot(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	runner := &fakeRunner{outputs: [][]byte{[]byte("false\n")}}
	sc := watch.NewScanner(fakeResolver{path: filepath.Join(t.TempDir(), "git")}, runner)

	got, err := sc.RepositoryNamespace(context.Background(), root)
	if err != nil {
		t.Fatalf("RepositoryNamespace: %v", err)
	}
	want, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("RepositoryNamespace = %q, want canonical root %q", got, want)
	}
}

func TestScannerRepositoryNamespaceRejectsLocalOriginPaths(t *testing.T) {
	t.Parallel()
	for _, remote := range []string{
		"/srv/git/group/repo.git",
		"../group/repo.git",
		"file:///srv/git/group/repo.git",
		`C:\git\group\repo.git`,
	} {
		remote := remote
		t.Run(remote, func(t *testing.T) {
			t.Parallel()
			root := t.TempDir()
			runner := &fakeRunner{outputs: [][]byte{
				[]byte("true\n"),
				[]byte(remote + "\n"),
			}}
			sc := watch.NewScanner(fakeResolver{path: filepath.Join(t.TempDir(), "git")}, runner)

			got, err := sc.RepositoryNamespace(context.Background(), root)
			if err != nil {
				t.Fatalf("RepositoryNamespace: %v", err)
			}
			want, err := filepath.EvalSymlinks(root)
			if err != nil {
				t.Fatal(err)
			}
			if got != want {
				t.Fatalf("RepositoryNamespace for local origin %q = %q, want canonical root %q",
					remote, got, want)
			}
		})
	}
}

func TestScannerListFallsBackToWalkOutsideGit(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writeFiles(t, root,
		"src/b.py",
		"src/a.ts",
		"vendor/hidden.go",
		".git/objects/object",
	)

	runner := &fakeRunner{outputs: [][]byte{[]byte("false\n")}}
	sc := watch.NewScanner(fakeResolver{path: filepath.Join(t.TempDir(), "git")}, runner)
	got, err := sc.List(context.Background(), root)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	want := []string{"src/a.ts", "src/b.py"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("List = %#v, want %#v", got, want)
	}
}

func TestScannerListHonoursCancelledContext(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	sc := watch.NewScanner(fakeResolver{}, &fakeRunner{})
	_, err := sc.List(ctx, t.TempDir())
	if err == nil {
		t.Fatal("List succeeded with a cancelled context")
	}
}

func TestScannerWatchDirectoriesSkipsGitIgnoredTreesButKeepsEmptyDirectories(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writeFiles(t, root,
		"src/a.ts",
		"ignored/deep/generated.ts",
		"node_modules/pkg/index.ts",
	)
	if err := os.Mkdir(filepath.Join(root, "empty"), 0o700); err != nil {
		t.Fatal(err)
	}

	runner := &fakeRunner{outputs: [][]byte{
		[]byte("true\n"),
		[]byte("ignored/\x00"),
	}}
	sc := watch.NewScanner(fakeResolver{path: filepath.Join(t.TempDir(), "git")}, runner)
	directoryScanner, ok := sc.(interface {
		WatchDirectories(context.Context, string, []string) ([]string, error)
	})
	if !ok {
		t.Fatal("production scanner does not expose its ignore-aware watch directory oracle")
	}

	got, err := directoryScanner.WatchDirectories(
		context.Background(),
		root,
		[]string{"src/a.ts"},
	)
	if err != nil {
		t.Fatalf("WatchDirectories: %v", err)
	}
	want := []string{".", "empty", "src"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("WatchDirectories = %#v, want %#v", got, want)
	}
	if len(runner.specs) != 2 {
		t.Fatalf("git calls = %d, want 2", len(runner.specs))
	}
	if got := strings.Join(runner.specs[1].Args, " "); got !=
		"-C "+root+" ls-files --others --ignored --exclude-standard --directory -z" {
		t.Fatalf("ignored-directory query args = %q", got)
	}
}

func writeFiles(t *testing.T, root string, names ...string) {
	t.Helper()
	for _, name := range names {
		path := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(name), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

var _ procx.Resolver = fakeResolver{}
var _ procx.Runner = (*fakeRunner)(nil)
