package auditor

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// git runs a read-only git command in dir.
func git(ctx context.Context, dir string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("git %s: %v: %s", strings.Join(args, " "), err, strings.TrimSpace(errb.String()))
	}
	return out.String(), nil
}

// ScanWorktrees lists repoDir's worktrees under root (read-only: no fetch,
// it compares against the clone's existing origin/master ref). A worktree
// is Merged when its HEAD is an ancestor of origin/master, Clean when
// `git status --porcelain` is empty.
func ScanWorktrees(ctx context.Context, repoDir, root string) ([]Worktree, error) {
	out, err := git(ctx, repoDir, "worktree", "list", "--porcelain")
	if err != nil {
		return nil, err
	}
	var all []Worktree
	var cur *Worktree
	sc := bufio.NewScanner(strings.NewReader(out))
	for sc.Scan() {
		line := sc.Text()
		switch {
		case strings.HasPrefix(line, "worktree "):
			all = append(all, Worktree{Path: strings.TrimPrefix(line, "worktree ")})
			cur = &all[len(all)-1]
		case cur == nil:
		case strings.HasPrefix(line, "HEAD "):
			cur.Head = strings.TrimPrefix(line, "HEAD ")
		case strings.HasPrefix(line, "branch "):
			cur.Branch = strings.TrimPrefix(strings.TrimPrefix(line, "branch "), "refs/heads/")
		case strings.HasPrefix(line, "prunable"):
			cur.Prunable = true
		}
	}
	root = filepath.Clean(root)
	var res []Worktree
	for _, w := range all {
		if !strings.HasPrefix(filepath.Clean(w.Path), root+"/") {
			continue // the main checkout and anything outside the root
		}
		if w.Prunable {
			res = append(res, w)
			continue
		}
		if _, err := git(ctx, repoDir, "merge-base", "--is-ancestor", w.Head, "origin/master"); err == nil {
			w.Merged = true
		}
		if st, err := git(ctx, w.Path, "--no-optional-locks", "status", "--porcelain"); err == nil && strings.TrimSpace(st) == "" {
			w.Clean = true
		}
		if ts, err := git(ctx, repoDir, "log", "-1", "--format=%ct", w.Head); err == nil {
			if n, err := strconv.ParseInt(strings.TrimSpace(ts), 10, 64); err == nil {
				w.LastCommit = time.Unix(n, 0).UTC()
			}
		}
		res = append(res, w)
	}
	return res, nil
}

// DiffStat returns the numstat of head against master in the factory's
// own bare mirror (mirrorDir, created on first use from url; never the
// operator checkout). It is how the factory checks an auto-fix branch's
// REAL diff (CheckDiff) before the branch may enter the merge queue.
func DiffStat(ctx context.Context, mirrorDir, url, branch, head string) ([]FileStat, error) {
	if _, err := os.Stat(filepath.Join(mirrorDir, "HEAD")); err != nil {
		if err := os.MkdirAll(filepath.Dir(mirrorDir), 0o755); err != nil {
			return nil, err
		}
		if _, err := git(ctx, filepath.Dir(mirrorDir), "init", "--quiet", "--bare", mirrorDir); err != nil {
			return nil, err
		}
	}
	if _, err := git(ctx, mirrorDir, "fetch", "--quiet", "--no-tags", url,
		"+refs/heads/master:refs/audit/master", "+refs/heads/"+branch+":refs/audit/fix"); err != nil {
		return nil, err
	}
	got, err := git(ctx, mirrorDir, "rev-parse", "refs/audit/fix")
	if err != nil {
		return nil, err
	}
	if !strings.HasPrefix(strings.TrimSpace(got), strings.ToLower(head)) {
		return nil, fmt.Errorf("branch %s is at %s, not the reported head %s", branch, strings.TrimSpace(got), head)
	}
	out, err := git(ctx, mirrorDir, "diff", "--numstat", "-M", "refs/audit/master...refs/audit/fix")
	if err != nil {
		return nil, err
	}
	return ParseNumstat(out), nil
}

// ParseNumstat parses `git diff --numstat` output ("-" = binary -> -1). A
// rename "a => b" (or "dir/{a => b}") yields both paths so the denylist
// sees each side.
func ParseNumstat(out string) []FileStat {
	var fs []FileStat
	for _, line := range strings.Split(out, "\n") {
		parts := strings.SplitN(line, "\t", 3)
		if len(parts) != 3 {
			continue
		}
		num := func(s string) int {
			if s == "-" {
				return -1
			}
			n, _ := strconv.Atoi(s)
			return n
		}
		a, d := num(parts[0]), num(parts[1])
		for i, p := range renameSides(parts[2]) {
			if i > 0 { // count the lines once
				a, d = 0, 0
			}
			fs = append(fs, FileStat{Path: p, Added: a, Deleted: d})
		}
	}
	return fs
}

func renameSides(p string) []string {
	if !strings.Contains(p, " => ") {
		return []string{p}
	}
	if i, j := strings.Index(p, "{"), strings.Index(p, "}"); i >= 0 && j > i {
		pre, post := p[:i], p[j+1:]
		ab := strings.SplitN(p[i+1:j], " => ", 2)
		if len(ab) == 2 {
			return []string{
				filepath.Clean(pre + ab[0] + post),
				filepath.Clean(pre + ab[1] + post),
			}
		}
	}
	ab := strings.SplitN(p, " => ", 2)
	return []string{ab[0], ab[1]}
}
