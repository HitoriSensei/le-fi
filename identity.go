package main

import (
	"fmt"
	"os"
	"os/user"
	"regexp"
	"strings"
)

// localIdentity returns a "user@host" string for this machine - shown in
// the browser upload UI so the person sending a file can see who they're
// sending it to, and used to build a filename when sending from a stdin
// pipe (which has no filename of its own).
func localIdentity() string {
	return fmt.Sprintf("%s@%s", currentUsername(), currentHostname())
}

func currentUsername() string {
	if u, err := user.Current(); err == nil && u.Username != "" {
		name := u.Username
		// os/user can return "DOMAIN\user" on some systems; keep just the
		// final component.
		if i := strings.LastIndexAny(name, `\/`); i >= 0 {
			name = name[i+1:]
		}
		return name
	}
	if u := os.Getenv("USER"); u != "" {
		return u
	}
	return "unknown"
}

func currentHostname() string {
	if h, err := os.Hostname(); err == nil && h != "" {
		return h
	}
	return "unknown-host"
}

var slugInvalidRe = regexp.MustCompile(`[^a-z0-9]+`)

// slugify lowercases s and collapses any run of non-alphanumeric
// characters into a single hyphen, trimming hyphens from both ends.
func slugify(s string) string {
	s = strings.ToLower(s)
	s = slugInvalidRe.ReplaceAllString(s, "-")
	return strings.Trim(s, "-")
}
