package auditor

import (
	"bytes"
	"io"
	"path/filepath"
	"strconv"
	"strings"
)

// The auditor never runs git itself (spec §6.12): the factory service
// does no git work at all. CI runs git and feeds
// `git diff --numstat [-z] -M <master>...<head>` to the checker
// (cmd/auditcheck), which parses it here and applies CheckDiff.

// ParseDiff parses `git diff --numstat` output in either form: NUL
// separated (-z, preferred: unambiguous for any file name) or the plain
// line form. Binary files have Added/Deleted -1. A rename yields both of
// its sides so the denylist sees each.
func ParseDiff(out []byte) []FileStat {
	if bytes.IndexByte(out, 0) >= 0 {
		return ParseNumstatZ(out)
	}
	return ParseNumstat(string(out))
}

// ReadDiff reads and parses a numstat stream (see ParseDiff).
func ReadDiff(r io.Reader) ([]FileStat, error) {
	b, err := io.ReadAll(io.LimitReader(r, 64<<20))
	if err != nil {
		return nil, err
	}
	return ParseDiff(b), nil
}

func numstatCount(s string) int {
	if s == "-" {
		return -1
	}
	n, err := strconv.Atoi(s)
	if err != nil || n < 0 {
		return -1 // unparseable counts are treated like binary: refused
	}
	return n
}

// ParseNumstatZ parses `git diff --numstat -z` output: each record is
// "added\tdeleted\tpath\0", or for a rename/copy
// "added\tdeleted\t\0src\0dst\0".
func ParseNumstatZ(out []byte) []FileStat {
	var fs []FileStat
	fields := strings.Split(string(out), "\x00")
	for i := 0; i < len(fields); i++ {
		rec := strings.TrimLeft(fields[i], "\n")
		if rec == "" {
			continue
		}
		parts := strings.SplitN(rec, "\t", 3)
		if len(parts) != 3 {
			continue
		}
		a, d := numstatCount(parts[0]), numstatCount(parts[1])
		if parts[2] != "" {
			fs = append(fs, FileStat{Path: parts[2], Added: a, Deleted: d})
			continue
		}
		if i+2 >= len(fields) {
			// Truncated rename record: refuse rather than guess.
			fs = append(fs, FileStat{Path: "<truncated rename record>", Added: -1, Deleted: -1})
			break
		}
		src, dst := fields[i+1], fields[i+2]
		i += 2
		fs = append(fs, FileStat{Path: src, Added: a, Deleted: d}, FileStat{Path: dst})
	}
	return fs
}

// ParseNumstat parses plain `git diff --numstat` output. A rename
// "a => b" (or "dir/{a => b}") yields both paths.
func ParseNumstat(out string) []FileStat {
	var fs []FileStat
	for _, line := range strings.Split(out, "\n") {
		parts := strings.SplitN(line, "\t", 3)
		if len(parts) != 3 {
			continue
		}
		a, d := numstatCount(parts[0]), numstatCount(parts[1])
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
