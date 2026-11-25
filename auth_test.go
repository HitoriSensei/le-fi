// Tests for password-protected sends (`-p`). Same approach as
// main_test.go: the real binary, run as subprocesses over localhost.
package main

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/creack/pty"
)

// getWithAuth does a GET, with basic auth when user is non-empty.
func getWithAuth(t *testing.T, url, user, pass string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		t.Fatalf("building request: %v", err)
	}
	if user != "" {
		req.SetBasicAuth(user, pass)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET failed: %v", err)
	}
	return resp
}

// expectUnauthorized checks a request was turned away with a basic auth
// challenge.
func expectUnauthorized(t *testing.T, url, user, pass string) {
	t.Helper()
	resp := getWithAuth(t, url, user, pass)
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401 for user=%q pass=%q, got %d", user, pass, resp.StatusCode)
	}
	if got := resp.Header.Get("WWW-Authenticate"); !strings.HasPrefix(got, "Basic ") {
		t.Errorf("expected a Basic WWW-Authenticate challenge, got %q", got)
	}
}

// expectStillRunning checks the sender is still up and serving (a
// zombie still answers signal 0, so ask over HTTP instead): an
// unauthenticated request should still get its 401 challenge.
func expectStillRunning(t *testing.T, s *spawned, port int) {
	t.Helper()
	time.Sleep(200 * time.Millisecond)
	resp, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/", port))
	if err != nil {
		t.Fatalf("expected the sender to still be running: %v\nstderr: %s", err, s.stderr.String())
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected the sender to still be waiting behind a password, got HTTP %d", resp.StatusCode)
	}
}

func writeTempFile(t *testing.T, name string, data []byte) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, data, 0o644); err != nil {
		t.Fatalf("failed to write %s: %v", name, err)
	}
	return p
}

func TestSendFile_PasswordBasicAuth(t *testing.T) {
	port := freePort(t)
	payload := randomBytes(t, 512*1024)
	srcPath := writeTempFile(t, "secret.bin", payload)

	send := spawnPtyStdin(t, []string{srcPath, "-p", "s3cret"}, io.Discard, localEnv(port)...)
	defer send.cleanup()
	waitForPort(t, port, 5*time.Second)
	url := fmt.Sprintf("http://127.0.0.1:%d/", port)

	// No credentials, then a pile of wrong ones: all 401, none fatal.
	expectUnauthorized(t, url, "", "")
	for i := 0; i < 10; i++ {
		expectUnauthorized(t, url, "anyone", fmt.Sprintf("wrong-%d", i))
	}
	expectStillRunning(t, send, port)

	// Any username with the right password gets the file.
	resp := getWithAuth(t, url, "whatever-name", "s3cret")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 with the right password, got %d", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("failed to read body: %v", err)
	}
	if !bytes.Equal(body, payload) {
		t.Fatalf("received content does not match source file")
	}

	if err := send.cmd.Wait(); err != nil {
		t.Fatalf("sender exited with error: %v\nstderr: %s", err, send.stderr.String())
	}
	stderr := send.stderr.String()
	if !strings.Contains(stderr, "curl -u le-fi") || strings.Contains(stderr, "s3cret") {
		t.Errorf("expected curl hint with -u and no echoed password, got:\n%s", stderr)
	}
}

// Piped stdin only gets one transfer attempt - a failed login must not
// use it up.
func TestSendStdin_PasswordWrongAuthDoesNotConsume(t *testing.T) {
	port := freePort(t)
	payload := randomBytes(t, 100*1024)

	send := spawnPtyStdout(t, []string{"--password=pw"}, bytes.NewReader(payload), localEnv(port)...)
	defer send.cleanup()
	waitForPort(t, port, 5*time.Second)
	url := fmt.Sprintf("http://127.0.0.1:%d/", port)

	expectUnauthorized(t, url, "", "")
	expectUnauthorized(t, url, "x", "nope")
	expectStillRunning(t, send, port)

	resp := getWithAuth(t, url, "x", "pw")
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK || !bytes.Equal(body, payload) {
		t.Fatalf("expected the payload after a correct login, got HTTP %d, %d bytes", resp.StatusCode, len(body))
	}
	if err := send.cmd.Wait(); err != nil {
		t.Fatalf("sender exited with error: %v\nstderr: %s", err, send.stderr.String())
	}
}

func TestClientReceive_PasswordFlag(t *testing.T) {
	port := freePort(t)
	payload := randomBytes(t, 256*1024)
	srcPath := writeTempFile(t, "f.bin", payload)

	send := spawnPtyStdin(t, []string{srcPath, "-p", "right"}, io.Discard, localEnv(port)...)
	defer send.cleanup()
	waitForPort(t, port, 5*time.Second)
	target := fmt.Sprintf("127.0.0.1:%d", port)

	// Wrong -p: the receive fails, the sender doesn't.
	var badOut bytes.Buffer
	bad := spawnPtyStdin(t, []string{target, "-p", "wrong"}, &badOut, "LEFI_LOCAL=1")
	if err := bad.cmd.Wait(); err == nil {
		t.Fatalf("expected a wrong -p to fail the receive")
	}
	bad.ptmx.Close()
	if !strings.Contains(bad.stderr.String(), "Wrong password") {
		t.Errorf("expected a wrong-password message, got:\n%s", bad.stderr.String())
	}
	if badOut.Len() != 0 {
		t.Errorf("expected nothing written on a failed login, got %d bytes", badOut.Len())
	}
	expectStillRunning(t, send, port)

	var out bytes.Buffer
	good := spawnPtyStdin(t, []string{"-p", "right", target}, &out, "LEFI_LOCAL=1")
	if err := good.cmd.Wait(); err != nil {
		t.Fatalf("receive with the right -p failed: %v\nstderr: %s", err, good.stderr.String())
	}
	good.ptmx.Close()
	if !bytes.Equal(out.Bytes(), payload) {
		t.Fatalf("received content does not match source file")
	}
	if err := send.cmd.Wait(); err != nil {
		t.Fatalf("sender exited with error: %v\nstderr: %s", err, send.stderr.String())
	}
}

// With no -p and no controlling terminal to prompt on, the receive must
// fail cleanly - and the sender must keep waiting.
func TestClientReceive_PasswordNoTTY(t *testing.T) {
	port := freePort(t)
	payload := randomBytes(t, 1024)
	srcPath := writeTempFile(t, "f.bin", payload)

	send := spawnPtyStdin(t, []string{srcPath, "-p", "pw"}, io.Discard, localEnv(port)...)
	defer send.cleanup()
	waitForPort(t, port, 5*time.Second)

	cmd := exec.Command(binPath, fmt.Sprintf("127.0.0.1:%d", port))
	cmd.Env = append(os.Environ(), "LEFI_LOCAL=1")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true} // no controlling tty
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err == nil {
		t.Fatalf("expected the receive to fail with no password and no terminal")
	}
	if !strings.Contains(stderr.String(), "no terminal") {
		t.Errorf("expected a no-terminal message, got:\n%s", stderr.String())
	}
	if stdout.Len() != 0 {
		t.Errorf("expected nothing on stdout, got %d bytes", stdout.Len())
	}
	expectStillRunning(t, send, port)
}

// syncBuffer is a bytes.Buffer safe to fill from one goroutine and read
// from another.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func waitForOutput(t *testing.T, buf *syncBuffer, want string, count int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if strings.Count(buf.String(), want) >= count {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %d× %q on the terminal, got:\n%s", count, want, buf.String())
}

// With no -p but a real terminal, le-fi asks for the password - and asks
// again after a wrong one.
func TestClientReceive_PasswordPrompt(t *testing.T) {
	port := freePort(t)
	payload := randomBytes(t, 64*1024)
	srcPath := writeTempFile(t, "f.bin", payload)

	send := spawnPtyStdin(t, []string{srcPath, "-p", "letmein"}, io.Discard, localEnv(port)...)
	defer send.cleanup()
	waitForPort(t, port, 5*time.Second)

	ptmx, tty, err := pty.Open()
	if err != nil {
		t.Fatalf("failed to open pty: %v", err)
	}
	defer ptmx.Close()

	cmd := exec.Command(binPath, fmt.Sprintf("127.0.0.1:%d", port))
	cmd.Env = append(os.Environ(), "LEFI_LOCAL=1")
	cmd.Stdin = tty
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	// Make the pty the child's controlling terminal, so /dev/tty is it.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true, Ctty: 0}
	if err := cmd.Start(); err != nil {
		t.Fatalf("failed to start client: %v", err)
	}
	tty.Close()
	defer func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }()

	var term syncBuffer
	go io.Copy(&term, ptmx)

	waitForOutput(t, &term, "Password:", 1)
	_, _ = ptmx.Write([]byte("nope\r"))
	waitForOutput(t, &term, "Password:", 2)
	_, _ = ptmx.Write([]byte("letmein\r"))

	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("receive failed: %v\nstderr: %s", err, stderr.String())
		}
	case <-time.After(10 * time.Second):
		t.Fatalf("receive timed out\nterminal: %s\nstderr: %s", term.String(), stderr.String())
	}
	if !bytes.Equal(stdout.Bytes(), payload) {
		t.Fatalf("received content does not match source file")
	}
	if strings.Contains(term.String(), "letmein") || strings.Contains(term.String(), "nope") {
		t.Errorf("password was echoed to the terminal:\n%s", term.String())
	}
	if err := send.cmd.Wait(); err != nil {
		t.Fatalf("sender exited with error: %v\nstderr: %s", err, send.stderr.String())
	}
}

func TestPasswordFlag_Dangling(t *testing.T) {
	if out, err := exec.Command(binPath, "file.txt", "-p").CombinedOutput(); err == nil || !strings.Contains(string(out), "needs a password") {
		t.Errorf("expected a dangling -p to fail with a hint, got err=%v out=%s", err, out)
	}
}

// putWithAuth uploads body to a receive server, with basic auth when
// user is non-empty.
func putWithAuth(t *testing.T, url, user, pass string, body []byte) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPut, url, bytes.NewReader(body))
	if err != nil {
		t.Fatalf("building request: %v", err)
	}
	if user != "" {
		req.SetBasicAuth(user, pass)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("PUT failed: %v", err)
	}
	return resp
}

// Both the upload page and the upload need the password, and failed
// logins don't use up the receiver's single transfer.
func TestReceiveServer_Password(t *testing.T) {
	port := freePort(t)
	payload := randomBytes(t, 256*1024)

	var out bytes.Buffer
	recv := spawnPtyStdin(t, []string{"-p", "pw"}, &out, localEnv(port)...)
	defer recv.cleanup()
	waitForPort(t, port, 5*time.Second)
	url := fmt.Sprintf("http://127.0.0.1:%d/", port)

	expectUnauthorized(t, url, "", "")
	expectUnauthorized(t, url, "x", "wrong")
	page := getWithAuth(t, url, "x", "pw")
	pageBody, _ := io.ReadAll(page.Body)
	page.Body.Close()
	if page.StatusCode != http.StatusOK || !strings.Contains(string(pageBody), "<html") {
		t.Fatalf("expected the upload page with the right password, got HTTP %d", page.StatusCode)
	}

	for _, user := range []string{"", "x"} {
		resp := putWithAuth(t, url, user, "wrong", payload)
		resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("expected 401 for an upload with user=%q and a wrong password, got %d", user, resp.StatusCode)
		}
	}
	expectStillRunning(t, recv, port)

	resp := putWithAuth(t, url, "anyone", "pw", payload)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 for an upload with the right password, got %d", resp.StatusCode)
	}
	if err := recv.cmd.Wait(); err != nil {
		t.Fatalf("receiver exited with error: %v\nstderr: %s", err, recv.stderr.String())
	}
	if !bytes.Equal(out.Bytes(), payload) {
		t.Fatalf("received content does not match the upload")
	}
	if !strings.Contains(recv.stderr.String(), "curl -u le-fi -T") {
		t.Errorf("expected a curl hint with -u, got:\n%s", recv.stderr.String())
	}
}

func TestClientSend_PasswordFlag(t *testing.T) {
	port := freePort(t)
	payload := randomBytes(t, 256*1024)

	var out bytes.Buffer
	recv := spawnPtyStdin(t, []string{"--password", "right"}, &out, localEnv(port)...)
	defer recv.cleanup()
	waitForPort(t, port, 5*time.Second)
	target := fmt.Sprintf("127.0.0.1:%d", port)

	// Wrong -p, from piped stdin: fails without touching the receiver.
	bad := spawnPtyStdout(t, []string{target, "-p", "wrong"}, bytes.NewReader(payload), "LEFI_LOCAL=1")
	if err := bad.cmd.Wait(); err == nil {
		t.Fatalf("expected a wrong -p to fail the send")
	}
	if !strings.Contains(bad.stderr.String(), "Wrong password") {
		t.Errorf("expected a wrong-password message, got:\n%s", bad.stderr.String())
	}
	expectStillRunning(t, recv, port)

	good := spawnPtyStdout(t, []string{target, "-p", "right"}, bytes.NewReader(payload), "LEFI_LOCAL=1")
	if err := good.cmd.Wait(); err != nil {
		t.Fatalf("send with the right -p failed: %v\nstderr: %s", err, good.stderr.String())
	}
	if err := recv.cmd.Wait(); err != nil {
		t.Fatalf("receiver exited with error: %v\nstderr: %s", err, recv.stderr.String())
	}
	if !bytes.Equal(out.Bytes(), payload) {
		t.Fatalf("received content does not match the upload")
	}
	if !strings.Contains(recv.stderr.String(), "checksum verified") {
		t.Errorf("expected a verified checksum, got:\n%s", recv.stderr.String())
	}
}

// Piped stdin plus a prompt: the password comes from the terminal while
// the file still comes in whole from stdin.
func TestClientSend_PasswordPrompt(t *testing.T) {
	port := freePort(t)
	payload := randomBytes(t, 64*1024)

	var out bytes.Buffer
	recv := spawnPtyStdin(t, []string{"-p", "letmein"}, &out, localEnv(port)...)
	defer recv.cleanup()
	waitForPort(t, port, 5*time.Second)

	ptmx, tty, err := pty.Open()
	if err != nil {
		t.Fatalf("failed to open pty: %v", err)
	}
	defer ptmx.Close()

	cmd := exec.Command(binPath, fmt.Sprintf("127.0.0.1:%d", port))
	cmd.Env = append(os.Environ(), "LEFI_LOCAL=1")
	cmd.Stdin = bytes.NewReader(payload)
	cmd.Stdout = tty
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true, Ctty: 1}
	if err := cmd.Start(); err != nil {
		t.Fatalf("failed to start client: %v", err)
	}
	tty.Close()
	defer func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }()

	var term syncBuffer
	go io.Copy(&term, ptmx)

	waitForOutput(t, &term, "Password:", 1)
	_, _ = ptmx.Write([]byte("nope\r"))
	waitForOutput(t, &term, "Password:", 2)
	_, _ = ptmx.Write([]byte("letmein\r"))

	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("send failed: %v\nstderr: %s", err, stderr.String())
		}
	case <-time.After(10 * time.Second):
		t.Fatalf("send timed out\nterminal: %s\nstderr: %s", term.String(), stderr.String())
	}
	if err := recv.cmd.Wait(); err != nil {
		t.Fatalf("receiver exited with error: %v\nstderr: %s", err, recv.stderr.String())
	}
	if !bytes.Equal(out.Bytes(), payload) {
		t.Fatalf("received content does not match the upload")
	}
}

func TestExtractPassword(t *testing.T) {
	cases := []struct {
		in      []string
		rest    []string
		pw      string
		wantErr bool
	}{
		{[]string{"a.txt"}, []string{"a.txt"}, "", false},
		{[]string{"a.txt", "-p", "x"}, []string{"a.txt"}, "x", false},
		{[]string{"-p", "x", "a.txt"}, []string{"a.txt"}, "x", false},
		{[]string{"--password", "x y", "a.txt"}, []string{"a.txt"}, "x y", false},
		{[]string{"--password=a=b", "a.txt"}, []string{"a.txt"}, "a=b", false},
		{[]string{"a.txt", "-p"}, nil, "", true},
		{[]string{"a.txt", "-p", ""}, nil, "", true},
	}
	for _, c := range cases {
		rest, pw, _, err := extractPassword(c.in)
		if (err != nil) != c.wantErr {
			t.Errorf("%v: err = %v, wantErr %v", c.in, err, c.wantErr)
			continue
		}
		if c.wantErr {
			continue
		}
		if pw != c.pw || strings.Join(rest, "|") != strings.Join(c.rest, "|") {
			t.Errorf("%v: got rest=%v pw=%q, want rest=%v pw=%q", c.in, rest, pw, c.rest, c.pw)
		}
	}
}
