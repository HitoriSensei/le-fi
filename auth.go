package main

import (
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"

	"golang.org/x/term"
)

// ---------------------------------------------------------------------
// Optional password protection for the sending side (`-p <password>`).
//
// The sender answers every GET/HEAD with an HTTP basic auth challenge
// until the right password comes in. Any username is accepted - only the
// password is checked - so a browser's login prompt, `curl -u`, `wget
// --user` and another le-fi all work the same way.
//
// A wrong (or missing) password is just a 401 for that one request: it
// never counts as a transfer attempt and never ends the send, so the
// other side can keep trying for as long as the sender stays up.
// ---------------------------------------------------------------------

// basicAuthRealm is shown by browsers in their login prompt.
const basicAuthRealm = "le-fi"

// basicAuthUser is the username le-fi itself sends, and the one shown in
// the curl/wget hints. The server accepts any username.
const basicAuthUser = "le-fi"

// errNoPasswordValue is returned when -p is the last argument.
var errNoPasswordValue = errors.New("-p needs a password after it, e.g. 'le-fi file.txt -p hunter2'")

// extractPassword pulls `-p <pw>`, `--password <pw>` and
// `--password=<pw>` out of args, wherever they appear, and returns the
// remaining positional args. set reports whether a password flag was
// given at all.
func extractPassword(args []string) (rest []string, password string, set bool, err error) {
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "-p" || a == "--password":
			if i+1 >= len(args) {
				return nil, "", false, errNoPasswordValue
			}
			password, set = args[i+1], true
			i++
		case strings.HasPrefix(a, "--password="):
			password, set = strings.TrimPrefix(a, "--password="), true
		default:
			rest = append(rest, a)
		}
	}
	if set && password == "" {
		return nil, "", false, errors.New("the password given with -p can't be empty")
	}
	return rest, password, set, nil
}

// passwordMatches compares in constant time. Both sides are hashed first
// so the comparison doesn't leak the password's length either.
func passwordMatches(got, want string) bool {
	g := sha256.Sum256([]byte(got))
	w := sha256.Sum256([]byte(want))
	return subtle.ConstantTimeCompare(g[:], w[:]) == 1
}

// checkBasicAuth returns true when no password is configured, or when the
// request carries basic auth with the right password (any username).
// Otherwise it writes a 401 challenge and returns false. The caller must
// return straight away without touching any transfer state, so a failed
// attempt never affects the send.
func checkBasicAuth(w http.ResponseWriter, r *http.Request, password string) bool {
	if password == "" {
		return true
	}
	if _, got, ok := r.BasicAuth(); ok && passwordMatches(got, password) {
		return true
	}
	w.Header().Set("WWW-Authenticate", fmt.Sprintf(`Basic realm=%q, charset="UTF-8"`, basicAuthRealm))
	http.Error(w, "this file is password-protected\n", http.StatusUnauthorized)
	return false
}

// promptPassword asks for a password on the controlling terminal, with
// echo off. It goes through /dev/tty rather than stdin/stderr because a
// receiving le-fi always has stdout redirected and may well have stdin
// redirected too. Returns an error when there's no terminal to ask on
// (cron, CI, ssh without a tty, ...).
func promptPassword(prompt string) (string, error) {
	tty, err := os.OpenFile("/dev/tty", os.O_RDWR, 0)
	if err != nil {
		return "", fmt.Errorf("no terminal to ask for a password on: %w", err)
	}
	defer tty.Close()
	if !term.IsTerminal(int(tty.Fd())) {
		return "", errors.New("no terminal to ask for a password on")
	}
	fmt.Fprint(tty, prompt)
	pw, err := term.ReadPassword(int(tty.Fd()))
	fmt.Fprintln(tty)
	if err != nil {
		return "", err
	}
	return string(pw), nil
}

// maxPasswordPrompts is how many times le-fi asks for the password before
// giving up. The other end keeps waiting either way, so a fresh run can
// always try again.
const maxPasswordPrompts = 3

// resolvePassword probes url with HEAD requests until it's either not
// password-protected or accepts the password, which comes from -p when
// given and from a terminal prompt otherwise. HEAD never starts a
// transfer, so wrong passwords cost the other end nothing.
//
// wantRole is the X-Le-Fi-Role the other end should have. On a mismatch
// it returns straight away and lets the real request report the wrong
// direction, rather than asking for a password that's of no use.
// usage is the command line shown when there's no terminal to ask on.
// Returns the password to use and false when le-fi should exit with an
// error (already printed).
func resolvePassword(url, password, wantRole, usage string) (string, bool) {
	fromFlag := password != ""
	for prompts := 0; ; {
		req, err := http.NewRequest(http.MethodHead, url, nil)
		if err != nil {
			fmt.Fprintln(os.Stderr, "error building request:", err)
			return "", false
		}
		if password != "" {
			req.SetBasicAuth(basicAuthUser, password)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			fmt.Fprintln(os.Stderr, "error connecting:", err)
			return "", false
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized || resp.Header.Get("X-Le-Fi-Role") != wantRole {
			return password, true
		}

		switch {
		case fromFlag:
			fmt.Fprintf(os.Stderr, "Wrong password (from -p). The %s is still waiting, run again with the correct one.\n", wantRole)
			return "", false
		case prompts >= maxPasswordPrompts:
			fmt.Fprintf(os.Stderr, "Wrong password. The %s is still waiting, run again to retry.\n", wantRole)
			return "", false
		}
		if prompts > 0 {
			fmt.Fprintln(os.Stderr, "Wrong password, try again.")
		}
		pw, perr := promptPassword("The other end is password-protected. Password: ")
		if perr != nil {
			fmt.Fprintln(os.Stderr, "The other end is password-protected and there's no terminal to ask for the password.")
			fmt.Fprintf(os.Stderr, "Use: %s\n", usage)
			return "", false
		}
		password = pw
		prompts++
	}
}
