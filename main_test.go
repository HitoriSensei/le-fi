// Integration tests for le-fi. These build and run the actual compiled
// binary as subprocesses, using LEFI_LOCAL and LEFI_PORT so everything
// runs over plain localhost - no Cloudflare tunnel, no outbound network
// access needed.
//
// A real pseudo-terminal is attached to whichever stream (stdin for
// receive-like modes, stdout for send-like modes) needs to look
// interactive rather than piped, matching how a real shell invocation
// looks to the dispatch logic in main.go.
package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/rand"
	"fmt"
	"hash/crc32"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/creack/pty"
)

var binPath string

// TestMain normally builds its own plain debug binary to test. If
// LEFI_TEST_BIN is set, it uses that binary instead - letting CI run this
// exact same test suite against the real release artifact.
func TestMain(m *testing.M) {
	if pre := os.Getenv("LEFI_TEST_BIN"); pre != "" {
		abs, err := filepath.Abs(pre)
		if err != nil {
			fmt.Println("invalid LEFI_TEST_BIN path:", err)
			os.Exit(1)
		}
		if _, err := os.Stat(abs); err != nil {
			fmt.Println("LEFI_TEST_BIN does not exist:", err)
			os.Exit(1)
		}
		binPath = abs
		os.Exit(m.Run())
	}

	dir, err := os.MkdirTemp("", "le-fi-test-bin-")
	if err != nil {
		fmt.Println("failed to create temp dir:", err)
		os.Exit(1)
	}
	defer os.RemoveAll(dir)

	binPath = filepath.Join(dir, "le-fi-test-bin")
	build := exec.Command("go", "build", "-o", binPath, ".")
	build.Stdout = os.Stderr
	build.Stderr = os.Stderr
	if err := build.Run(); err != nil {
		fmt.Println("failed to build test binary:", err)
		os.Exit(1)
	}

	os.Exit(m.Run())
}

// ---------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------

func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to find a free port: %v", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()
	return port
}

func waitForPort(t *testing.T, port int, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 200*time.Millisecond)
		if err == nil {
			conn.Close()
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("nothing listening on port %d within %s", port, timeout)
}

func randomBytes(t *testing.T, n int) []byte {
	t.Helper()
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		t.Fatalf("failed to generate random payload: %v", err)
	}
	return buf
}

type spawned struct {
	cmd    *exec.Cmd
	stderr *bytes.Buffer
	ptmx   *os.File // owned by us; close during cleanup
}

func (s *spawned) cleanup() {
	if s.cmd.Process != nil {
		_ = s.cmd.Process.Kill()
	}
	_ = s.cmd.Wait()
	if s.ptmx != nil {
		_ = s.ptmx.Close()
	}
}

// spawnPtyStdin starts le-fi with a real pty attached to stdin (so it
// looks like an interactive terminal, not a pipe) and captures stdout to
// a buffer - matching real `le-fi > out` / `le-fi <channel> > out` usage
// from a shell.
func spawnPtyStdin(t *testing.T, args []string, stdout io.Writer, env ...string) *spawned {
	t.Helper()
	ptmx, tty, err := pty.Open()
	if err != nil {
		t.Fatalf("failed to open pty: %v", err)
	}
	defer tty.Close()

	cmd := exec.Command(binPath, args...)
	cmd.Env = append(os.Environ(), env...)
	cmd.Stdin = tty
	cmd.Stdout = stdout
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("failed to start %v: %v", args, err)
	}
	return &spawned{cmd: cmd, stderr: &stderr, ptmx: ptmx}
}

// spawnPtyStdout starts le-fi with stdin fed from stdinData and a real
// pty attached to stdout (so it looks interactive, not redirected) -
// matching real `le-fi < file` / `le-fi <channel> < file` usage.
func spawnPtyStdout(t *testing.T, args []string, stdinData io.Reader, env ...string) *spawned {
	t.Helper()
	ptmx, tty, err := pty.Open()
	if err != nil {
		t.Fatalf("failed to open pty: %v", err)
	}
	defer tty.Close()

	cmd := exec.Command(binPath, args...)
	cmd.Env = append(os.Environ(), env...)
	cmd.Stdin = stdinData
	cmd.Stdout = tty
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("failed to start %v: %v", args, err)
	}
	// Drain the pty's master side so the child never blocks writing to
	// it, even though our send modes don't normally write anything there.
	go io.Copy(io.Discard, ptmx)
	return &spawned{cmd: cmd, stderr: &stderr, ptmx: ptmx}
}

func localEnv(port int) []string {
	return []string{"LEFI_LOCAL=1", fmt.Sprintf("LEFI_PORT=%d", port)}
}

// ---------------------------------------------------------------------
// Help
// ---------------------------------------------------------------------

func TestHelpFlag(t *testing.T) {
	for _, flag := range []string{"-h", "--help"} {
		out, err := exec.Command(binPath, flag).CombinedOutput()
		if err != nil {
			t.Fatalf("%s: unexpected error: %v\noutput: %s", flag, err, out)
		}
		text := string(out)
		for _, want := range []string{"USAGE", "ATTRIBUTION", "Apache License"} {
			if !strings.Contains(text, want) {
				t.Errorf("%s: expected output to contain %q, got:\n%s", flag, want, text)
			}
		}
	}
}

// ---------------------------------------------------------------------
// Pattern A: le-fi > out (receive server)
// ---------------------------------------------------------------------

func TestReceiveServer_GetServesUploadPage(t *testing.T) {
	port := freePort(t)
	var recvStdout bytes.Buffer
	recv := spawnPtyStdin(t, nil, &recvStdout, localEnv(port)...)
	defer recv.cleanup()
	waitForPort(t, port, 5*time.Second)

	resp, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/", port))
	if err != nil {
		t.Fatalf("GET failed: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	if got := resp.Header.Get("Content-Type"); !strings.HasPrefix(got, "text/html") {
		t.Errorf("expected text/html, got %q", got)
	}
	if got := resp.Header.Get("X-Le-Fi-Role"); got != "receiver" {
		t.Errorf("expected X-Le-Fi-Role: receiver, got %q", got)
	}
	if !strings.Contains(string(body), "Sending to") {
		t.Errorf("expected upload page to show target identity, got:\n%s", body)
	}
	if !strings.Contains(string(body), `id="dropzone"`) {
		t.Errorf("expected upload page to contain the drop zone element")
	}
}

func TestReceiveServer_ChecksumVerified(t *testing.T) {
	port := freePort(t)
	var recvStdout bytes.Buffer
	recv := spawnPtyStdin(t, nil, &recvStdout, localEnv(port)...)
	defer recv.cleanup()
	waitForPort(t, port, 5*time.Second)

	payload := randomBytes(t, 3*1024*1024)
	send := spawnPtyStdout(t, []string{fmt.Sprintf("127.0.0.1:%d", port)}, bytes.NewReader(payload), "LEFI_LOCAL=1")
	if err := send.cmd.Wait(); err != nil {
		t.Fatalf("sender failed: %v\nstderr: %s", err, send.stderr.String())
	}
	send.ptmx.Close()

	if err := recv.cmd.Wait(); err != nil {
		t.Fatalf("receiver exited with error: %v\nstderr: %s", err, recv.stderr.String())
	}
	if !bytes.Equal(recvStdout.Bytes(), payload) {
		t.Fatalf("received data does not match sent data (got %d bytes, want %d)", recvStdout.Len(), len(payload))
	}
	if !strings.Contains(recv.stderr.String(), "checksum verified") {
		t.Errorf("expected checksum verification message, got:\n%s", recv.stderr.String())
	}
	if !strings.Contains(send.stderr.String(), "Transfer complete") {
		t.Errorf("expected sender to report success, got:\n%s", send.stderr.String())
	}
}

func TestReceiveServer_CurlStyleNoChecksum(t *testing.T) {
	port := freePort(t)
	var recvStdout bytes.Buffer
	recv := spawnPtyStdin(t, nil, &recvStdout, localEnv(port)...)
	defer recv.cleanup()
	waitForPort(t, port, 5*time.Second)

	payload := randomBytes(t, 1*1024*1024)
	req, err := http.NewRequest(http.MethodPut, fmt.Sprintf("http://127.0.0.1:%d/", port), bytes.NewReader(payload))
	if err != nil {
		t.Fatalf("failed to build request: %v", err)
	}
	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, body)
	}

	if err := recv.cmd.Wait(); err != nil {
		t.Fatalf("receiver exited with error: %v\nstderr: %s", err, recv.stderr.String())
	}
	if !bytes.Equal(recvStdout.Bytes(), payload) {
		t.Fatalf("received data does not match sent data")
	}
	if !strings.Contains(recv.stderr.String(), "no checksum to verify") {
		t.Errorf("expected 'no checksum to verify' message, got:\n%s", recv.stderr.String())
	}
}

func TestReceiveServer_ChecksumMismatch(t *testing.T) {
	port := freePort(t)
	var recvStdout bytes.Buffer
	recv := spawnPtyStdin(t, nil, &recvStdout, localEnv(port)...)
	defer recv.cleanup()
	waitForPort(t, port, 5*time.Second)

	payload := randomBytes(t, 512*1024)
	realSum := crc32.ChecksumIEEE(payload)
	wrongSum := realSum ^ 0xFFFFFFFF

	// io.NopCloser avoids http.NewRequest's *bytes.Reader fast-path,
	// which would set a known Content-Length and thereby silently
	// disable chunked encoding - trailers require it.
	body := io.NopCloser(bytes.NewReader(payload))
	req, err := http.NewRequest(http.MethodPut, fmt.Sprintf("http://127.0.0.1:%d/", port), body)
	if err != nil {
		t.Fatalf("failed to build request: %v", err)
	}
	req.Trailer = http.Header{"X-Crc32": {fmt.Sprintf("%08x", wrongSum)}}

	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("expected HTTP 422 on checksum mismatch, got %d: %s", resp.StatusCode, respBody)
	}

	err = recv.cmd.Wait()
	if err == nil {
		t.Fatalf("expected receiver to exit non-zero on checksum mismatch")
	}
	if !strings.Contains(recv.stderr.String(), "CHECKSUM MISMATCH") {
		t.Errorf("expected CHECKSUM MISMATCH message, got:\n%s", recv.stderr.String())
	}
}

func TestReceiveServer_ConnectionDropped(t *testing.T) {
	if _, err := exec.LookPath("curl"); err != nil {
		t.Skip("curl not available")
	}

	port := freePort(t)
	var recvStdout bytes.Buffer
	recv := spawnPtyStdin(t, nil, &recvStdout, localEnv(port)...)
	defer recv.cleanup()
	waitForPort(t, port, 5*time.Second)

	payload := randomBytes(t, 40*1024*1024)
	payloadPath := filepath.Join(t.TempDir(), "payload.bin")
	if err := os.WriteFile(payloadPath, payload, 0o644); err != nil {
		t.Fatalf("failed to write payload file: %v", err)
	}
	f, err := os.Open(payloadPath)
	if err != nil {
		t.Fatalf("failed to open payload file: %v", err)
	}
	defer f.Close()

	curlCmd := exec.Command("curl", "-sS", "--limit-rate", "4M", "-T", "-",
		fmt.Sprintf("http://127.0.0.1:%d/", port))
	curlCmd.Stdin = f
	if err := curlCmd.Start(); err != nil {
		t.Fatalf("failed to start curl: %v", err)
	}
	time.Sleep(700 * time.Millisecond)
	_ = curlCmd.Process.Kill()
	_ = curlCmd.Wait()

	err = recv.cmd.Wait()
	if err == nil {
		t.Fatalf("expected receiver to exit non-zero after dropped connection")
	}
	if !strings.Contains(recv.stderr.String(), "connection dropped") {
		t.Errorf("expected 'connection dropped' message, got:\n%s", recv.stderr.String())
	}
}

func TestReceiveServer_SecondUploadRejected(t *testing.T) {
	port := freePort(t)
	var recvStdout bytes.Buffer
	recv := spawnPtyStdin(t, nil, &recvStdout, localEnv(port)...)
	defer recv.cleanup()
	waitForPort(t, port, 5*time.Second)

	payload := randomBytes(t, 4096)
	put := func() (*http.Response, error) {
		req, err := http.NewRequest(http.MethodPut, fmt.Sprintf("http://127.0.0.1:%d/", port), bytes.NewReader(payload))
		if err != nil {
			return nil, err
		}
		return (&http.Client{Timeout: 10 * time.Second}).Do(req)
	}

	resp1, err := put()
	if err != nil {
		t.Fatalf("first upload failed: %v", err)
	}
	resp1.Body.Close()
	if resp1.StatusCode != http.StatusOK {
		t.Fatalf("expected first upload to succeed, got %d", resp1.StatusCode)
	}

	if err := recv.cmd.Wait(); err != nil {
		t.Fatalf("receiver exited with error: %v\nstderr: %s", err, recv.stderr.String())
	}

	// The receiver process has now exited (single-shot), so a second
	// request should simply fail to connect.
	if _, err := put(); err == nil {
		t.Errorf("expected second upload to fail (receiver should have exited), but it succeeded")
	}
}

// ---------------------------------------------------------------------
// Patterns B/C/D + E: send-servers and the client that pulls from them
// ---------------------------------------------------------------------

func TestSendFile_ClientReceive(t *testing.T) {
	port := freePort(t)
	payload := randomBytes(t, 2*1024*1024)
	srcPath := filepath.Join(t.TempDir(), "myphoto.jpg")
	if err := os.WriteFile(srcPath, payload, 0o644); err != nil {
		t.Fatalf("failed to write source file: %v", err)
	}

	send := spawnPtyStdin(t, []string{srcPath}, io.Discard, localEnv(port)...)
	defer send.cleanup()
	waitForPort(t, port, 5*time.Second)

	resp, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/", port))
	if err != nil {
		t.Fatalf("GET failed: %v", err)
	}
	defer resp.Body.Close()
	if got := resp.Header.Get("Content-Disposition"); !strings.Contains(got, "myphoto.jpg") {
		t.Errorf("expected Content-Disposition to reference myphoto.jpg, got %q", got)
	}
	if got := resp.Header.Get("Content-Length"); got != fmt.Sprint(len(payload)) {
		t.Errorf("expected Content-Length %d, got %q", len(payload), got)
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
	if !strings.Contains(send.stderr.String(), "Sent") {
		t.Errorf("expected sender to report success, got:\n%s", send.stderr.String())
	}
}

func TestSendStdin_FilenameSlugified(t *testing.T) {
	port := freePort(t)
	payload := randomBytes(t, 100*1024)

	send := spawnPtyStdout(t, nil, bytes.NewReader(payload), localEnv(port)...)
	defer send.cleanup()
	waitForPort(t, port, 5*time.Second)

	resp, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/", port))
	if err != nil {
		t.Fatalf("GET failed: %v", err)
	}
	defer resp.Body.Close()
	cd := resp.Header.Get("Content-Disposition")
	today := time.Now().Format("2006-01-02")
	if !strings.Contains(cd, today) {
		t.Errorf("expected filename to contain today's date %s, got %q", today, cd)
	}
	if !strings.HasSuffix(cd, ".bin\"") {
		t.Errorf("expected filename to end in .bin, got %q", cd)
	}
	body, _ := io.ReadAll(resp.Body)
	if !bytes.Equal(body, payload) {
		t.Fatalf("received content does not match piped stdin")
	}

	if err := send.cmd.Wait(); err != nil {
		t.Fatalf("sender exited with error: %v\nstderr: %s", err, send.stderr.String())
	}
}

func TestSendDirectory_TarGz(t *testing.T) {
	port := freePort(t)
	srcDir := filepath.Join(t.TempDir(), "mystuff")
	if err := os.MkdirAll(filepath.Join(srcDir, "sub"), 0o755); err != nil {
		t.Fatalf("failed to create source dir: %v", err)
	}
	files := map[string][]byte{
		"top.txt":     []byte("top level file"),
		"sub/nested.bin": randomBytes(t, 5000),
	}
	for rel, content := range files {
		if err := os.WriteFile(filepath.Join(srcDir, rel), content, 0o644); err != nil {
			t.Fatalf("failed to write %s: %v", rel, err)
		}
	}

	send := spawnPtyStdin(t, []string{srcDir}, io.Discard, localEnv(port)...)
	defer send.cleanup()
	waitForPort(t, port, 5*time.Second)

	resp, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/", port))
	if err != nil {
		t.Fatalf("GET failed: %v", err)
	}
	defer resp.Body.Close()
	if got := resp.Header.Get("Content-Disposition"); !strings.Contains(got, "mystuff.tar.gz") {
		t.Errorf("expected filename mystuff.tar.gz, got %q", got)
	}

	gzr, err := gzip.NewReader(resp.Body)
	if err != nil {
		t.Fatalf("response is not valid gzip: %v", err)
	}
	tr := tar.NewReader(gzr)
	found := map[string][]byte{}
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("error reading tar entry: %v", err)
		}
		if hdr.Typeflag != tar.TypeReg {
			continue
		}
		data, err := io.ReadAll(tr)
		if err != nil {
			t.Fatalf("error reading tar entry content: %v", err)
		}
		found[hdr.Name] = data
	}
	for rel, want := range files {
		got, ok := found[rel]
		if !ok {
			t.Errorf("archive missing entry %q (have: %v)", rel, found)
			continue
		}
		if !bytes.Equal(got, want) {
			t.Errorf("entry %q content mismatch", rel)
		}
	}

	if err := send.cmd.Wait(); err != nil {
		t.Fatalf("sender exited with error: %v\nstderr: %s", err, send.stderr.String())
	}
}

func TestSendServer_RetryAfterDrop(t *testing.T) {
	port := freePort(t)
	payload := randomBytes(t, 20*1024*1024)
	srcPath := filepath.Join(t.TempDir(), "big.bin")
	if err := os.WriteFile(srcPath, payload, 0o644); err != nil {
		t.Fatalf("failed to write source file: %v", err)
	}

	send := spawnPtyStdin(t, []string{srcPath}, io.Discard, localEnv(port)...)
	defer send.cleanup()
	waitForPort(t, port, 5*time.Second)

	// Attempt 1: connect, read a little, then abruptly close.
	conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 5*time.Second)
	if err != nil {
		t.Fatalf("dial failed: %v", err)
	}
	_, _ = conn.Write([]byte("GET / HTTP/1.1\r\nHost: 127.0.0.1\r\nConnection: close\r\n\r\n"))
	buf := make([]byte, 65536)
	_, _ = conn.Read(buf)
	conn.Close()

	time.Sleep(500 * time.Millisecond)
	if send.cmd.ProcessState != nil {
		t.Fatalf("send-server exited after a dropped connection; it should stay up for a retry")
	}

	// Attempt 2: full, successful retrieval.
	resp, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/", port))
	if err != nil {
		t.Fatalf("retry GET failed: %v", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("failed reading retry body: %v", err)
	}
	if !bytes.Equal(body, payload) {
		t.Fatalf("retried transfer content mismatch (got %d bytes, want %d)", len(body), len(payload))
	}

	if err := send.cmd.Wait(); err != nil {
		t.Fatalf("sender exited with error after successful retry: %v\nstderr: %s", err, send.stderr.String())
	}
}

// ---------------------------------------------------------------------
// Role mismatch detection
// ---------------------------------------------------------------------

func TestClientReceive_RoleMismatch(t *testing.T) {
	port := freePort(t)
	var recvStdout bytes.Buffer
	recv := spawnPtyStdin(t, nil, &recvStdout, localEnv(port)...)
	defer recv.cleanup()
	waitForPort(t, port, 5*time.Second)

	var clientOut bytes.Buffer
	client := spawnPtyStdin(t, []string{fmt.Sprintf("127.0.0.1:%d", port)}, &clientOut, "LEFI_LOCAL=1")
	err := client.cmd.Wait()
	client.ptmx.Close()
	if err == nil {
		t.Fatalf("expected client to exit non-zero on role mismatch")
	}
	if !strings.Contains(client.stderr.String(), "Did you mean") {
		t.Errorf("expected a did-you-mean suggestion, got:\n%s", client.stderr.String())
	}
	if clientOut.Len() != 0 {
		t.Errorf("expected nothing written to stdout on mismatch, got %d bytes", clientOut.Len())
	}
}

func TestClientSend_RoleMismatch(t *testing.T) {
	port := freePort(t)
	payload := randomBytes(t, 1024)
	srcPath := filepath.Join(t.TempDir(), "file.bin")
	if err := os.WriteFile(srcPath, payload, 0o644); err != nil {
		t.Fatalf("failed to write source file: %v", err)
	}

	send := spawnPtyStdin(t, []string{srcPath}, io.Discard, localEnv(port)...)
	defer send.cleanup()
	waitForPort(t, port, 5*time.Second)

	otherPayload := randomBytes(t, 1024)
	client := spawnPtyStdout(t, []string{fmt.Sprintf("127.0.0.1:%d", port)}, bytes.NewReader(otherPayload), "LEFI_LOCAL=1")
	err := client.cmd.Wait()
	if err == nil {
		t.Fatalf("expected client to exit non-zero on role mismatch")
	}
	if !strings.Contains(client.stderr.String(), "Did you mean") {
		t.Errorf("expected a did-you-mean suggestion, got:\n%s", client.stderr.String())
	}
}

// ---------------------------------------------------------------------
// Pure unit tests for small formatting helpers.
// ---------------------------------------------------------------------

func TestFormatBytes(t *testing.T) {
	cases := []struct {
		n    int64
		want string
	}{
		{0, "0 B"},
		{500, "500 B"},
		{1024, "1.0 KiB"},
		{1536, "1.5 KiB"},
		{1024 * 1024, "1.0 MiB"},
		{1024 * 1024 * 1024, "1.0 GiB"},
	}
	for _, c := range cases {
		if got := formatBytes(c.n); got != c.want {
			t.Errorf("formatBytes(%d) = %q, want %q", c.n, got, c.want)
		}
	}
}

func TestFormatDuration(t *testing.T) {
	cases := []struct {
		d    time.Duration
		want string
	}{
		{0, "0s"},
		{5 * time.Second, "5s"},
		{59 * time.Second, "59s"},
		{60 * time.Second, "1m00s"},
		{90 * time.Second, "1m30s"},
	}
	for _, c := range cases {
		if got := formatDuration(c.d); got != c.want {
			t.Errorf("formatDuration(%s) = %q, want %q", c.d, got, c.want)
		}
	}
}
