package sync

import (
	"path"
	"path/filepath"
	"slices"
	"strings"
)

// RemotePath returns the Drive folder path segments, relative to the root
// folder, for local directory dir: ~/Documents/Personal → [Documents
// Personal]; paths outside home go under _root (/mnt/data → [_root mnt data]).
func RemotePath(home, dir string) []string {
	home, dir = filepath.Clean(home), filepath.Clean(dir)
	if dir == home {
		return nil
	}
	if within(dir, home) {
		return strings.Split(strings.TrimPrefix(dir, home+"/"), "/")
	}
	if dir == "/" {
		return []string{"_root"}
	}
	return append([]string{"_root"}, strings.Split(strings.TrimPrefix(dir, "/"), "/")...)
}

// within reports whether p is dir or below it.
func within(p, dir string) bool {
	return p == dir || dir == "/" || strings.HasPrefix(p, dir+"/")
}

// NormalizeSources cleans, sorts and de-duplicates source folders and drops
// folders nested inside another source (they are already covered).
func NormalizeSources(srcs []string) []string {
	clean := make([]string, 0, len(srcs))
	for _, s := range srcs {
		if filepath.IsAbs(s) {
			clean = append(clean, filepath.Clean(s))
		}
	}
	slices.Sort(clean)
	var out []string
	for _, s := range clean {
		if !slices.ContainsFunc(out, func(o string) bool { return within(s, o) }) {
			out = append(out, s)
		}
	}
	return out
}

// Excluded reports whether rel (slash-separated, relative to the source
// folder) matches one of the glob patterns:
//   - "name/" matches directories only, "name" matches files and directories;
//   - a pattern without "/" is matched against the base name at any depth
//     ("*.tmp", "node_modules/");
//   - a pattern containing "/" is matched against the whole relative path
//     ("build/*.o", "/top-only.txt").
//
// Blank lines and lines starting with "#" are ignored.
func Excluded(patterns []string, rel string, isDir bool) bool {
	name := path.Base(rel)
	for _, p := range patterns {
		p = strings.TrimSpace(p)
		if p == "" || strings.HasPrefix(p, "#") {
			continue
		}
		dirOnly := strings.HasSuffix(p, "/")
		p = strings.TrimSuffix(p, "/")
		if dirOnly && !isDir || p == "" {
			continue
		}
		target := name
		if strings.Contains(p, "/") {
			target, p = rel, strings.TrimPrefix(p, "/")
		}
		if ok, _ := path.Match(p, target); ok {
			return true
		}
	}
	return false
}
