package azure

import (
	"bufio"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// dockerignore is a .dockerignore matcher.
//
// The rules are the same as .gitignore with one difference that matters: a
// later pattern wins, so a `!` negation only re-includes a path if nothing
// after it excludes it again. Matching is done against slash-separated paths
// relative to the context root, and a directory match excludes everything
// under it.
type dockerignore struct {
	patterns []ignorePattern
}

type ignorePattern struct {
	pattern string
	negate  bool
}

func loadDockerignore(root string) (*dockerignore, error) {
	file, err := os.Open(filepath.Join(root, ".dockerignore"))
	if err != nil {
		if os.IsNotExist(err) {
			return &dockerignore{}, nil
		}
		return nil, err
	}
	defer file.Close()

	ignore := &dockerignore{}
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}

		negate := strings.HasPrefix(line, "!")
		line = strings.TrimPrefix(line, "!")
		line = strings.Trim(line, "/")
		if line == "" {
			continue
		}

		ignore.patterns = append(ignore.patterns, ignorePattern{pattern: line, negate: negate})
	}
	return ignore, scanner.Err()
}

// matches reports whether a slash-separated relative path is excluded.
func (d *dockerignore) matches(name string) bool {
	// .git is never a build input and is frequently large. Docker excludes it
	// implicitly; so does this.
	if name == ".git" || strings.HasPrefix(name, ".git/") {
		return true
	}

	excluded := false
	for _, entry := range d.patterns {
		if entry.match(name) {
			excluded = !entry.negate
		}
	}
	return excluded
}

func (p ignorePattern) match(name string) bool {
	if ok, _ := path.Match(p.pattern, name); ok {
		return true
	}

	// A pattern matching a parent directory excludes everything beneath it.
	for parent := path.Dir(name); parent != "." && parent != "/"; parent = path.Dir(parent) {
		if ok, _ := path.Match(p.pattern, parent); ok {
			return true
		}
	}

	// `**/x` and bare `x` both mean "x at any depth", which path.Match cannot
	// express on its own.
	base := strings.TrimPrefix(p.pattern, "**/")
	if !strings.Contains(base, "/") {
		for _, segment := range strings.Split(name, "/") {
			if ok, _ := path.Match(base, segment); ok {
				return true
			}
		}
	}

	return false
}
