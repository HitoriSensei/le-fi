// Integration tests for le-fi. These build and run the actual compiled
// binary as subprocesses (both sender and receiver roles), using
// LEFI_LOCAL and LEFI_PORT so the whole thing runs over plain localhost -
// no Cloudflare tunnel, no DNS, no outbound network access at all. That's
// the point: these need to pass in a CI runner with no internet.
package main

import (
	"bytes"
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
)

var binPath string

func TestMain(m *testing.M) {
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

// freePort asks the OS for an ephemeral port, then releases it immediately
// so the receiver subprocess can bind it via LEFI_PORT. Small TOCTOU race
// in principle; fine for tests on a quiet CI runner.
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
	t.Fatalf("receiver never started listening on port %d within %s", port, timeout)
}

func randomBytes(t *testing.T, n int) []byte {
	t.Helper()
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		t.Fatalf("failed to generate random payload: %v", err)
	}
	return buf
}

type receiver struct {
	cmd    *exec.Cmd
	stdout *bytes.Buffer
	stderr *bytes.Buffer
}

// startReceiver launches the compiled binary in local-mode receiver mode
// bound to a specific port, and waits until it's actually accepting
// connections before returning.
func startReceiver(t *testing.T, port int) *receiver {
	t.Helper()
	cmd := exec.Command(binPath)
	cmd.Env = append(os.Environ(),
		"LEFI_LOCAL=1",
		fmt.Sprintf("LEFI_PORT=%d", port),
	)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("failed to start receiver: %v", err)
	}
	waitForPort(t, port, 5*time.Second)
	return &receiver{cmd: cmd, stdout: &stdout, stderr: &stderr}
}

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

func TestLocalTransfer_BothHaveUtility_ChecksumVerified(t *testing.T) {
	port := freePort(t)
	recv := startReceiver(t, port)
	defer recv.cmd.Process.Kill()

	payload := randomBytes(t, 3*1024*1024) // 3MB

	sendCmd := exec.Command(binPath, fmt.Sprintf("127.0.0.1:%d", port))
	sendCmd.Env = append(os.Environ(), "LEFI_LOCAL=1")
	sendCmd.Stdin = bytes.NewReader(payload)
	var sendErr bytes.Buffer
	sendCmd.Stderr = &sendErr
	if err := sendCmd.Run(); err != nil {
		t.Fatalf("sender failed: %v\nstderr: %s", err, sendErr.String())
	}

	if err := recv.cmd.Wait(); err != nil {
		t.Fatalf("receiver exited with error: %v\nstderr: %s", err, recv.stderr.String())
	}

	if !bytes.Equal(recv.stdout.Bytes(), payload) {
		t.Fatalf("received data does not match sent data (got %d bytes, want %d)",
			recv.stdout.Len(), len(payload))
	}
	if !strings.Contains(recv.stderr.String(), "checksum verified") {
		t.Errorf("expected checksum verification message, got:\n%s", recv.stderr.String())
	}
	if !strings.Contains(sendErr.String(), "Transfer complete") {
		t.Errorf("expected sender to report success, got:\n%s", sendErr.String())
	}
}

// TestLocalTransfer_CurlStyle_NoChecksum simulates a sender that doesn't
// have this utility (curl/wget) by making a plain PUT with no CRC32
// trailer, straight from net/http rather than shelling out to curl. The
// receiver must still succeed, just without verification.
func TestLocalTransfer_CurlStyle_NoChecksum(t *testing.T) {
	port := freePort(t)
	recv := startReceiver(t, port)
	defer recv.cmd.Process.Kill()

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
	if !bytes.Equal(recv.stdout.Bytes(), payload) {
		t.Fatalf("received data does not match sent data")
	}
	if !strings.Contains(recv.stderr.String(), "no checksum to verify") {
		t.Errorf("expected 'no checksum to verify' message, got:\n%s", recv.stderr.String())
	}
}

// TestLocalTransfer_ChecksumMismatch crafts a request with a deliberately
// wrong CRC32 trailer (replicating le-fi's own sender-side trailer
// mechanism) and confirms the receiver detects and reports the mismatch
// rather than silently accepting corrupted data.
func TestLocalTransfer_ChecksumMismatch(t *testing.T) {
	port := freePort(t)
	recv := startReceiver(t, port)
	defer recv.cmd.Process.Kill()

	payload := randomBytes(t, 512*1024)
	realSum := crc32.ChecksumIEEE(payload)
	wrongSum := realSum ^ 0xFFFFFFFF

	var req *http.Request
	body := io.NopCloser(bytes.NewReader(payload)) // avoid the *bytes.Reader
	// fast-path in NewRequest, which sets a known Content-Length and
	// thereby silently disables chunked encoding - trailers require it.
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

// TestLocalTransfer_ConnectionDropped kills curl mid-upload (throttled so
// there's time to do so) and confirms the receiver reports a dropped
// connection with the partial progress made, rather than hanging or
// silently exiting.
func TestLocalTransfer_ConnectionDropped(t *testing.T) {
	if _, err := exec.LookPath("curl"); err != nil {
		t.Skip("curl not available")
	}

	port := freePort(t)
	recv := startReceiver(t, port)
	defer recv.cmd.Process.Kill()

	payload := randomBytes(t, 40*1024*1024) // 40MB, throttled below to give time to kill it
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

// TestLocalTransfer_SecondUploadRejected confirms the receiver is
// single-shot: a second upload attempt after the first succeeds must be
// refused, not silently accepted or appended.
func TestLocalTransfer_SecondUploadRejected(t *testing.T) {
	port := freePort(t)
	recv := startReceiver(t, port)
	defer recv.cmd.Process.Kill()

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
	// request should simply fail to connect - confirms it didn't linger
	// accepting more uploads.
	if _, err := put(); err == nil {
		t.Errorf("expected second upload to fail (receiver should have exited), but it succeeded")
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
