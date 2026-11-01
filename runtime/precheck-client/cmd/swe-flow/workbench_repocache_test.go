package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func runFixtureGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	command := exec.Command("git", append([]string{"-C", dir}, args...)...)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %s: %v", args, output, err)
	}
	return strings.TrimSpace(string(output))
}

func TestRepoMirrorPathKeysByRepository(t *testing.T) {
	cache := newRepoMirrorCache("/state")
	path, err := cache.mirrorPath("https://github.com/MarketCalls/OpenAlgo.git")
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join("/state", "repos", "marketcalls", "openalgo.git"); path != want {
		t.Fatalf("mirror path = %q, want %q", path, want)
	}
	if _, err := cache.mirrorPath("https://gitlab.com/a/b"); err == nil {
		t.Fatal("non-GitHub repository accepted")
	}
}

// seedCheckout creates a task checkout of fixture whose public remote is
// repoURL, which is what the cache looks for when seeding itself.
func seedCheckout(t *testing.T, workRoot, name, fixture, repoURL string) string {
	t.Helper()
	checkout := filepath.Join(workRoot, "sources", name)
	if err := os.MkdirAll(filepath.Dir(checkout), 0700); err != nil {
		t.Fatal(err)
	}
	runFixtureGit(t, "", "clone", "--quiet", fixture, checkout)
	runFixtureGit(t, checkout, "remote", "set-url", "origin", repoURL)
	return checkout
}

func TestRepoMirrorSeedsFromExistingCheckoutAndClonesLocally(t *testing.T) {
	parent := t.TempDir()
	fixture := gitFixture(t, parent)
	baseline := runFixtureGit(t, fixture, "rev-parse", "HEAD")
	const repoURL = "https://github.com/fixture/project"
	workRoot := filepath.Join(parent, "work")
	seedCheckout(t, workRoot, "5837-project", fixture, repoURL)

	cache := &repoMirrorCache{
		root:    filepath.Join(parent, "cache", "repos"),
		refresh: func(context.Context, string, []string) error { return nil },
	}
	target := filepath.Join(workRoot, "sources", "5838-project")
	if err := os.MkdirAll(target, 0700); err != nil {
		t.Fatal(err)
	}
	if err := cache.clone(context.Background(), workRoot, repoURL, target, ""); err != nil {
		t.Fatal(err)
	}
	if head := runFixtureGit(t, target, "rev-parse", "HEAD"); head != baseline {
		t.Fatalf("HEAD = %q, want %q", head, baseline)
	}
	if remote := runFixtureGit(t, target, "remote", "get-url", "upstream"); remote != repoURL {
		t.Fatalf("upstream = %q, want %q", remote, repoURL)
	}
	if status := runFixtureGit(t, target, "status", "--porcelain=v1", "--untracked-files=all"); status != "" {
		t.Fatalf("clone is not clean: %s", status)
	}
	content, err := os.ReadFile(filepath.Join(target, "README.md"))
	if err != nil || string(content) != "public source\n" {
		t.Fatalf("README = %q, %v", content, err)
	}
	mirror, err := cache.mirrorPath(repoURL)
	if err != nil {
		t.Fatal(err)
	}
	if !mirrorReady(mirror) {
		t.Fatalf("mirror missing at %s", mirror)
	}
	// The second preparation of the same project must reuse the mirror rather
	// than the seed checkout or the network.
	second := filepath.Join(workRoot, "sources", "5839-project")
	if err := os.MkdirAll(second, 0700); err != nil {
		t.Fatal(err)
	}
	if err := cache.clone(context.Background(), workRoot, repoURL, second, ""); err != nil {
		t.Fatal(err)
	}
	if head := runFixtureGit(t, second, "rev-parse", "HEAD"); head != baseline {
		t.Fatalf("second HEAD = %q, want %q", head, baseline)
	}
}

func TestRepoMirrorResumesInterruptedDownload(t *testing.T) {
	parent := t.TempDir()
	fixture := gitFixture(t, parent)
	const repoURL = "https://github.com/fixture/project"
	cache := &repoMirrorCache{
		root:    filepath.Join(parent, "cache", "repos"),
		refresh: func(context.Context, string, []string) error { return nil },
	}
	mirror, err := cache.mirrorPath(repoURL)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(mirror), 0700); err != nil {
		t.Fatal(err)
	}
	if err := exec.Command("git", "clone", "--mirror", "--quiet", fixture, mirror+".partial").Run(); err != nil {
		t.Fatal(err)
	}
	// The seed checkout is absent on purpose: a resumed download must finish
	// from the partial mirror alone.
	if _, err := cache.ensureMirror(context.Background(), filepath.Join(parent, "work"), repoURL, os.Environ()); err != nil {
		t.Fatal(err)
	}
	if !mirrorReady(mirror) {
		t.Fatalf("partial mirror was not promoted to %s", mirror)
	}
	if _, err := os.Stat(mirror + ".partial"); !os.IsNotExist(err) {
		t.Fatalf("partial mirror left behind")
	}
}

func TestCloneProjectSourceFallsBackWhenCacheFails(t *testing.T) {
	workRoot := t.TempDir()
	target := filepath.Join(workRoot, "target")
	if err := os.Mkdir(target, 0700); err != nil {
		t.Fatal(err)
	}
	called := false
	w := &workbenchServer{
		root:            workRoot,
		workRoot:        workRoot,
		repoCache:       &repoMirrorCache{root: filepath.Join(workRoot, "cache")},
		cloneRepo:       func(context.Context, string, string) error { called = true; return nil },
		resolveLanguage: func(context.Context, string) (string, error) { return "", nil },
	}
	// An invalid repository URL cannot be cached, so the direct clone runs.
	if err := w.cloneProjectSource(context.Background(), "not-a-repository", target); err != nil {
		t.Fatal(err)
	}
	if !called {
		t.Fatal("direct clone was not attempted after the cache failed")
	}
}

func TestParseDefaultBranch(t *testing.T) {
	output := "ref: refs/heads/main\tHEAD\nabc123\tHEAD\n"
	if got := parseDefaultBranch(output); got != "refs/heads/main" {
		t.Fatalf("default branch = %q", got)
	}
	if got := parseDefaultBranch("abc123\tHEAD\n"); got != "" {
		t.Fatalf("detached ls-remote = %q", got)
	}
}

// A task checkout is normally detached at its design baseline. Seeding a
// mirror from it must not make every later clone start at that baseline.
func TestRepoMirrorRestoresBranchHeadFromDetachedSeed(t *testing.T) {
	parent := t.TempDir()
	fixture := gitFixture(t, parent)
	first := runFixtureGit(t, fixture, "rev-parse", "HEAD")
	if err := os.WriteFile(filepath.Join(fixture, "README.md"), []byte("public source v2\n"), 0644); err != nil {
		t.Fatal(err)
	}
	runFixtureGit(t, fixture, "add", "README.md")
	runFixtureGit(t, fixture, "-c", "user.name=fixture", "-c", "user.email=fixture@example.test", "commit", "-qm", "second")
	latest := runFixtureGit(t, fixture, "rev-parse", "HEAD")

	const repoURL = "https://github.com/fixture/project"
	workRoot := filepath.Join(parent, "work")
	checkout := seedCheckout(t, workRoot, "5837-project", fixture, repoURL)
	runFixtureGit(t, checkout, "checkout", "--detach", first)
	branch := "refs/heads/" + runFixtureGit(t, fixture, "symbolic-ref", "--short", "HEAD")

	cache := &repoMirrorCache{
		root:          filepath.Join(parent, "cache", "repos"),
		refresh:       func(context.Context, string, []string) error { return nil },
		defaultBranch: func(context.Context, string, []string) (string, error) { return branch, nil },
	}
	target := filepath.Join(workRoot, "sources", "5838-project")
	if err := os.MkdirAll(target, 0700); err != nil {
		t.Fatal(err)
	}
	if err := cache.clone(context.Background(), workRoot, repoURL, target, ""); err != nil {
		t.Fatal(err)
	}
	if head := runFixtureGit(t, target, "rev-parse", "HEAD"); head != latest {
		t.Fatalf("HEAD = %q, want branch head %q", head, latest)
	}
	if ref := runFixtureGit(t, target, "symbolic-ref", "HEAD"); ref != branch {
		t.Fatalf("HEAD ref = %q", ref)
	}
}
