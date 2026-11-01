package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"
)

// Prechecks of one project all start from the same upstream repository, and a
// full clone of a large repository easily outweighs everything else a
// preparation does. A bare mirror per repository turns every pull after the
// first into a local copy, so the network only carries what changed.
//
// Mirrors live in the workbench's private state, never inside the work root:
// a mirror must never be mistaken for a task directory or appear in a listing.
const (
	repoMirrorDirectory = "repos"
	// Waiting for a mirror another process is already building is cheaper than
	// downloading the same repository a second time.
	repoMirrorLockTimeout = 30 * time.Minute
	repoMirrorLockPoll    = 2 * time.Second
)

type repoMirrorCache struct {
	root string
	// refresh brings a mirror up to date with its remote. It is a field so
	// tests can exercise the cache without a network.
	refresh func(ctx context.Context, mirror string, env []string) error
	// defaultBranch reports the remote's default branch. A mirror seeded from
	// a task checkout inherits that checkout's detached HEAD, which would
	// otherwise pin every later clone to the seed's baseline instead of the
	// branch head.
	defaultBranch func(ctx context.Context, mirror string, env []string) (string, error)
}

func newRepoMirrorCache(privateRoot string) *repoMirrorCache {
	return &repoMirrorCache{
		root: filepath.Join(privateRoot, repoMirrorDirectory),
		refresh: func(ctx context.Context, mirror string, env []string) error {
			// Branches and tags only: a mirror that also tracks every pull
			// request spends minutes refreshing refs that never become a
			// baseline.
			return gitRun(ctx, env, "-C", mirror, "fetch", "--prune", "--quiet", "origin",
				"+refs/heads/*:refs/heads/*", "+refs/tags/*:refs/tags/*")
		},
		defaultBranch: func(ctx context.Context, mirror string, env []string) (string, error) {
			output, err := gitRunOutput(ctx, env, "-C", mirror, "ls-remote", "--symref", "origin", "HEAD")
			if err != nil {
				return "", err
			}
			return parseDefaultBranch(output), nil
		},
	}
}

// mirrorPath derives the cache location from the repository identity, so the
// same project maps to the same mirror regardless of which precheck asks.
func (c *repoMirrorCache) mirrorPath(repoURL string) (string, error) {
	owner, repo, err := githubRepoParts(repoURL)
	if err != nil {
		return "", err
	}
	return filepath.Join(c.root, strings.ToLower(owner), strings.ToLower(repo)+".git"), nil
}

// clone materialises repoURL into target from the local mirror.
func (c *repoMirrorCache) clone(ctx context.Context, workRoot, repoURL, target, proxyURL string) error {
	env := append(gitProxyEnv(os.Environ(), proxyURL), "GIT_TERMINAL_PROMPT=0")
	mirror, err := c.ensureMirror(ctx, workRoot, repoURL, env)
	if err != nil {
		return err
	}
	if err := gitCloneFromMirror(ctx, mirror, target, repoURL, env); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("从项目缓存复制源码失败：%s", gitDetail(err))
	}
	return nil
}

// ensureMirror returns a usable mirror, building it once per repository. An
// interrupted download keeps its partial mirror, so a retry continues where
// the previous attempt stopped instead of starting from zero.
func (c *repoMirrorCache) ensureMirror(ctx context.Context, workRoot, repoURL string, env []string) (string, error) {
	path, err := c.mirrorPath(repoURL)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return "", fmt.Errorf("无法建立项目缓存目录")
	}
	unlock, err := acquireRepoLock(ctx, path+".lock")
	if err != nil {
		return "", err
	}
	defer unlock()
	if mirrorReady(path) {
		if err := c.refresh(ctx, path, env); err != nil {
			if ctx.Err() != nil {
				return "", ctx.Err()
			}
			return "", fmt.Errorf("项目缓存更新失败：%s", gitDetail(err))
		}
		c.ensureDefaultHead(ctx, path, env)
		return path, nil
	}
	partial := path + ".partial"
	if !mirrorReady(partial) {
		// The partial mirror is either absent or unusable; rebuild it in place
		// so the next interrupted attempt still finds valid objects.
		if err := os.RemoveAll(partial); err != nil {
			return "", fmt.Errorf("无法清理未完成的项目缓存")
		}
		seeded := false
		if seed := seedRepository(workRoot, repoURL); seed != "" {
			if err := gitRun(ctx, env, "clone", "--mirror", "--quiet", "--", seed, partial); err == nil {
				// The cache must remember GitHub as its origin, not the seed
				// checkout that happened to be on this machine.
				if err := gitRun(ctx, env, "-C", partial, "remote", "set-url", "origin", repoURL); err != nil {
					_ = os.RemoveAll(partial)
				} else {
					seeded = true
				}
			} else if ctx.Err() != nil {
				return "", ctx.Err()
			} else {
				_ = os.RemoveAll(partial)
			}
		}
		if !seeded {
			if err := gitRun(ctx, env, "clone", "--mirror", "--quiet", "--", repoURL, partial); err != nil {
				if ctx.Err() != nil {
					return "", ctx.Err()
				}
				return "", fmt.Errorf("无法建立项目缓存：%s", gitDetail(err))
			}
		}
	}
	if err := c.refresh(ctx, partial, env); err != nil {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		return "", fmt.Errorf("项目缓存更新失败：%s", gitDetail(err))
	}
	c.ensureDefaultHead(ctx, partial, env)
	if err := os.Rename(partial, path); err != nil {
		return "", fmt.Errorf("无法写入项目缓存")
	}
	return path, nil
}

// ensureDefaultHead points a mirror's HEAD at the remote default branch. A
// mirror built from a seeded checkout starts detached at that checkout's
// baseline; without this the next clone would check out an old commit instead
// of the branch head. Failures are ignored: the refresh already succeeded, and
// a detached HEAD is still a valid cache for baseline-pinned prechecks.
func (c *repoMirrorCache) ensureDefaultHead(ctx context.Context, mirror string, env []string) {
	if output, err := gitRunOutput(ctx, env, "-C", mirror, "symbolic-ref", "--quiet", "HEAD"); err == nil {
		if strings.HasPrefix(strings.TrimSpace(output), "refs/heads/") {
			return
		}
	}
	if c.defaultBranch == nil {
		return
	}
	ref, err := c.defaultBranch(ctx, mirror, env)
	if err != nil || !strings.HasPrefix(ref, "refs/heads/") {
		return
	}
	// Never point HEAD at a branch the mirror does not carry: that would leave
	// every later clone with an unborn HEAD.
	if _, err := gitRunOutput(ctx, env, "-C", mirror, "show-ref", "--verify", "--quiet", ref); err != nil {
		return
	}
	_ = gitRun(ctx, env, "-C", mirror, "symbolic-ref", "HEAD", ref)
}

// parseDefaultBranch reads the branch from git ls-remote --symref output,
// whose first line is "ref: refs/heads/main	HEAD".
func parseDefaultBranch(output string) string {
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "ref: ") {
			continue
		}
		fields := strings.Fields(strings.TrimPrefix(line, "ref: "))
		if len(fields) > 0 && strings.HasPrefix(fields[0], "refs/heads/") {
			return fields[0]
		}
	}
	return ""
}

// mirrorReady reports whether dir looks like a usable bare repository.
func mirrorReady(dir string) bool {
	for _, name := range []string{"HEAD", "config"} {
		info, err := os.Stat(filepath.Join(dir, name))
		if err != nil || !info.Mode().IsRegular() {
			return false
		}
	}
	info, err := os.Stat(filepath.Join(dir, "objects"))
	return err == nil && info.IsDir()
}

// seedRepository finds an existing checkout of the same repository under the
// work root. Seeding from it makes the first cache build local, which matters
// most for the large repositories this cache exists for. A dirty working tree
// is fine: only the object database is copied.
func seedRepository(workRoot, repoURL string) string {
	if workRoot == "" {
		return ""
	}
	entries, err := os.ReadDir(filepath.Join(workRoot, "sources"))
	if err != nil {
		return ""
	}
	type candidate struct {
		path    string
		updated time.Time
	}
	candidates := make([]candidate, 0, len(entries))
	for _, entry := range entries {
		if !entry.IsDir() || strings.HasPrefix(entry.Name(), ".") {
			continue
		}
		checkout := filepath.Join(workRoot, "sources", entry.Name())
		if !mirrorReady(filepath.Join(checkout, ".git")) {
			continue
		}
		if repositoryRemote(checkout) != repoURL {
			continue
		}
		info, err := os.Stat(filepath.Join(checkout, ".git"))
		if err != nil {
			continue
		}
		candidates = append(candidates, candidate{path: checkout, updated: info.ModTime()})
	}
	if len(candidates) == 0 {
		return ""
	}
	sort.Slice(candidates, func(i, j int) bool { return candidates[i].updated.After(candidates[j].updated) })
	return candidates[0].path
}

// repositoryRemote reports the public URL a checkout was cloned from. Both
// remote names are checked because older tasks used origin.
func repositoryRemote(checkout string) string {
	for _, remote := range []string{"upstream", "origin"} {
		output, err := exec.Command("git", "-C", checkout, "config", "--get", "remote."+remote+".url").Output()
		if err != nil {
			continue
		}
		if url := strings.TrimSpace(string(output)); url != "" {
			return url
		}
	}
	return ""
}

// gitCloneFromMirror copies the working tree out of the mirror and restores
// the public URL as upstream, which is what the prepared source is verified
// against.
func gitCloneFromMirror(ctx context.Context, mirror, target, repoURL string, env []string) error {
	if err := gitRun(ctx, env, "clone", "--quiet", "--origin", "upstream", "--", mirror, target); err != nil {
		return err
	}
	return gitRun(ctx, env, "-C", target, "remote", "set-url", "upstream", repoURL)
}

// acquireRepoLock serialises mirror builds across workbench processes without
// ever blocking forever: a lock held by a dead process is released by the
// operating system when flock's descriptor closes.
func acquireRepoLock(ctx context.Context, path string) (func(), error) {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, fmt.Errorf("无法建立项目缓存锁")
	}
	deadline := time.Now().Add(repoMirrorLockTimeout)
	for {
		err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return func() {
				_ = syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
				_ = file.Close()
			}, nil
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) {
			_ = file.Close()
			return nil, fmt.Errorf("无法建立项目缓存锁")
		}
		if time.Now().After(deadline) {
			_ = file.Close()
			return nil, fmt.Errorf("项目缓存正被另一个任务使用，等待超时")
		}
		select {
		case <-ctx.Done():
			_ = file.Close()
			return nil, ctx.Err()
		case <-time.After(repoMirrorLockPoll):
		}
	}
}

// gitRun runs git without a terminal and kills the whole process group when
// the context ends. Killing only the direct child used to leave
// git-remote-https downloading in the background, so an abandoned pull kept
// consuming the bandwidth the next attempt needed.
func gitRun(ctx context.Context, env []string, args ...string) error {
	command, stderr := newGitCommand(ctx, env, args...)
	command.Stdout = io.Discard
	if err := command.Run(); err != nil {
		return gitRunError(ctx, args, stderr, err)
	}
	return nil
}

// gitRunOutput is gitRun for the few commands whose stdout is the answer.
func gitRunOutput(ctx context.Context, env []string, args ...string) (string, error) {
	command, stderr := newGitCommand(ctx, env, args...)
	var stdout bytes.Buffer
	command.Stdout = &stdout
	if err := command.Run(); err != nil {
		return "", gitRunError(ctx, args, stderr, err)
	}
	return stdout.String(), nil
}

func newGitCommand(ctx context.Context, env []string, args ...string) (*exec.Cmd, *limitedBuffer) {
	command := exec.CommandContext(ctx, "git", args...)
	command.Env = env
	stderr := &limitedBuffer{limit: 8 << 10}
	command.Stderr = stderr
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	command.Cancel = func() error {
		if command.Process == nil {
			return nil
		}
		return syscall.Kill(-command.Process.Pid, syscall.SIGKILL)
	}
	command.WaitDelay = 5 * time.Second
	return command, stderr
}

func gitRunError(ctx context.Context, args []string, stderr *limitedBuffer, err error) error {
	if ctxErr := ctx.Err(); ctxErr != nil {
		return ctxErr
	}
	return &gitCommandError{command: "git " + strings.Join(args, " "), detail: stderr.String(), err: err}
}

type gitCommandError struct {
	command string
	detail  string
	err     error
}

func (e *gitCommandError) Error() string {
	if detail := lastLine(e.detail, ""); detail != "" {
		return e.command + ": " + detail
	}
	return e.command + ": " + e.err.Error()
}

func (e *gitCommandError) Unwrap() error { return e.err }

// gitDetail extracts the most useful line from a git failure so the workbench
// can show the real reason instead of a generic hint.
func gitDetail(err error) string {
	var commandErr *gitCommandError
	if errors.As(err, &commandErr) {
		return lastLine(commandErr.detail, commandErr.err.Error())
	}
	if err == nil {
		return ""
	}
	return lastLine(err.Error(), "未知错误")
}

// limitedBuffer keeps only the tail of git's stderr, which is where git puts
// the fatal line, while bounding memory for a verbose clone.
type limitedBuffer struct {
	mu    sync.Mutex
	buf   []byte
	limit int
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.buf = append(b.buf, p...)
	if len(b.buf) > b.limit {
		b.buf = append([]byte(nil), b.buf[len(b.buf)-b.limit:]...)
	}
	return len(p), nil
}

func (b *limitedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return string(b.buf)
}
