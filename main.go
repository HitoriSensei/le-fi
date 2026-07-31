// le-fi: send/receive a piped file across NATs using a Cloudflare Quick
// Tunnel, without requiring the separate `cloudflared` binary to be
// installed. This links cloudflared's own Go packages directly into this
// binary and drives them in-process.
//
// Receiver (always starts the process):
//
//	le-fi > out.bin
//
// Sender, if they also have this utility:
//
//	le-fi <channel-name> < file_to_send.bin
//
// Sender, if they only have curl or wget, uses one of the printed commands.
package main

import (
	"context"
	"fmt"
	"hash"
	"hash/crc32"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"regexp"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/urfave/cli/v2"
	"golang.org/x/sys/unix"
	"golang.org/x/term"

	"github.com/cloudflare/cloudflared/cmd/cloudflared/cliutil"
	"github.com/cloudflare/cloudflared/cmd/cloudflared/tunnel"
)

var tunnelHostnameRe = regexp.MustCompile(`https://([a-z0-9-]+\.trycloudflare\.com)`)

// The trailer used to carry the sender's running CRC-32 checksum, computed
// while the body streams and only known once the last byte has been read -
// exactly what HTTP trailers exist for, so no separate request or in-memory
// buffering is needed.
const crc32TrailerHeader = "X-Crc32"

// crcTeeReader wraps an io.Reader, feeding every byte read into a running
// CRC-32 checksum, and invokes onEOF exactly once with the final sum as
// soon as the underlying reader reports io.EOF - which happens while the
// HTTP client is still in the middle of writing the request, in time for
// the computed value to go out as a trailer.
type crcTeeReader struct {
	r     io.Reader
	crc   hash.Hash32
	onEOF func(sum uint32)
	done  bool
}

func (t *crcTeeReader) Read(p []byte) (int, error) {
	n, err := t.r.Read(p)
	if n > 0 {
		t.crc.Write(p[:n])
	}
	if err == io.EOF && !t.done {
		t.done = true
		if t.onEOF != nil {
			t.onEOF(t.crc.Sum32())
		}
	}
	return n, err
}

// ---------------------------------------------------------------------
// Live transfer progress: spinner + elapsed time + byte counter while
// waiting/receiving, a final checkmark/cross line once it settles.
// ---------------------------------------------------------------------

type xferPhase int32

const (
	phaseWaiting xferPhase = iota
	phaseReceiving
	phaseDone
	phaseFailed
	phaseCancelled
)

// xferResult carries the outcome of a single upload attempt from the HTTP
// handler back to the main goroutine: how much was received, whether the
// connection failed outright, and whether a CRC-32 trailer was present and
// matched.
type xferResult struct {
	bytes      int64
	err        error // non-nil: the connection dropped/errored mid-transfer
	crcChecked bool  // a checksum trailer was present at all
	crcOK      bool  // meaningful only if crcChecked
	expected   string
	computed   string
}

type progress struct {
	phase       atomic.Int32
	bytes       atomic.Int64
	waitStart   time.Time // set once, before the spinner goroutine starts
	xferStartNs atomic.Int64
}

func newProgress() *progress {
	return &progress{waitStart: time.Now()}
}

func (p *progress) markReceiving() {
	if p.phase.CompareAndSwap(int32(phaseWaiting), int32(phaseReceiving)) {
		p.xferStartNs.Store(time.Now().UnixNano())
	}
}

func (p *progress) addBytes(n int64) {
	p.bytes.Add(n)
}

func (p *progress) xferElapsed() time.Duration {
	start := p.xferStartNs.Load()
	if start == 0 {
		return 0
	}
	return time.Since(time.Unix(0, start))
}

// countingReader wraps an io.Reader, adding each chunk read to a progress
// counter and flipping the phase to "receiving" the moment bytes start
// flowing (covers the case where headers arrive but the body is still
// being streamed in).
type countingReader struct {
	r io.Reader
	p *progress
}

func (cr *countingReader) Read(buf []byte) (int, error) {
	n, err := cr.r.Read(buf)
	if n > 0 {
		cr.p.markReceiving()
		cr.p.addBytes(int64(n))
	}
	return n, err
}

// runSpinner renders live progress to ui until stop() is called. On a real
// terminal it redraws a single animated line in place; otherwise (output
// redirected to a file/pipe) it falls back to one plain line per update,
// at a slower interval, so a log file doesn't fill up with thousands of
// near-duplicate lines.
func runSpinner(ui *os.File, p *progress) (stop func()) {
	isTTY := term.IsTerminal(int(ui.Fd()))
	frames := []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}
	interval := 100 * time.Millisecond
	if !isTTY {
		interval = time.Second
	}

	stopCh := make(chan struct{})
	doneCh := make(chan struct{})
	go func() {
		defer close(doneCh)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		i := 0
		for {
			select {
			case <-stopCh:
				return
			case <-ticker.C:
				renderFrame(ui, frames[i%len(frames)], p, isTTY)
				i++
			}
		}
	}()
	return func() {
		close(stopCh)
		<-doneCh
	}
}

func renderFrame(ui *os.File, frame string, p *progress, isTTY bool) {
	var line string
	switch xferPhase(p.phase.Load()) {
	case phaseReceiving:
		line = fmt.Sprintf("%s receiving data... (%s, %s received)",
			frame, formatDuration(p.xferElapsed()), formatBytes(p.bytes.Load()))
	default: // phaseWaiting
		line = fmt.Sprintf("%s waiting for transfer to begin... (%s)",
			frame, formatDuration(time.Since(p.waitStart)))
	}
	writeStatusLine(ui, line, isTTY)
}

func writeStatusLine(ui *os.File, line string, isTTY bool) {
	if isTTY {
		fmt.Fprintf(ui, "\r\033[K%s", line)
	} else {
		fmt.Fprintln(ui, line)
	}
}

// printFinal writes the terminal (in both senses) status line: a colored
// symbol on a real terminal, plain text otherwise.
func printFinal(ui *os.File, symbol, colorCode, message string) {
	isTTY := term.IsTerminal(int(ui.Fd()))
	if isTTY {
		fmt.Fprintf(ui, "\r\033[K\033[%sm%s\033[0m %s\n", colorCode, symbol, message)
	} else {
		fmt.Fprintf(ui, "%s %s\n", symbol, message)
	}
}

func formatDuration(d time.Duration) string {
	d = d.Round(time.Second)
	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
	m := d / time.Minute
	s := (d - m*time.Minute) / time.Second
	return fmt.Sprintf("%dm%02ds", m, s)
}

func formatBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for nn := n / unit; nn >= unit; nn /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}

func main() {
	args := os.Args[1:]

	if len(args) > 0 && (args[0] == "-h" || args[0] == "--help") {
		printHelp()
		os.Exit(0)
	}

	if len(args) == 0 {
		// No channel argument. If nothing is piped in either direction
		// either, this is almost certainly someone just running the bare
		// command to see what it does, not an actual receive - show help
		// instead of silently opening a real tunnel.
		if term.IsTerminal(int(os.Stdin.Fd())) && term.IsTerminal(int(os.Stdout.Fd())) {
			printHelp()
			os.Exit(0)
		}
		os.Exit(runReceiver())
	}

	os.Exit(runSender(args[0]))
}

func printHelp() {
	fmt.Print(`le-fi - send/receive a piped file across NATs, no port forwarding

Uses a Cloudflare Quick Tunnel (the same free, unauthenticated tunnels
behind trycloudflare.com) to open a temporary public HTTPS endpoint to
this machine - works through symmetric NAT/CGNAT, no account, no router
config. Links cloudflared's own Go packages directly into this binary and
drives them in-process, so no separate cloudflared executable needs to be
installed anywhere.

USAGE:
  Receiving end always starts the process:

      le-fi > received_file.bin

  This brings up a tunnel and prints the exact command(s) for the sending
  party to use, plus the bare channel name.

  Sending end, if they also have this utility:

      le-fi <channel-name> < file_to_send.bin

  Sending end, if they only have curl or wget - the receiver's output
  already prints the exact ready-to-use command, e.g.:

      curl -T - https://<channel-name>.trycloudflare.com/ < file_to_send.bin
      wget --method=PUT --body-file=file_to_send.bin https://<channel-name>.trycloudflare.com/ -O -

EXAMPLE SESSION:
  Receiving machine:
      $ le-fi > photos.zip
      Tunnel is up (no account, no port forwarding - works through any NAT).
      ...
      Channel name:
        idol-car-indication-behavior

  Sending machine (has le-fi too):
      $ le-fi idol-car-indication-behavior < photos.zip

FLAGS:
  -h, --help    Show this help and exit.

ATTRIBUTION:
  This tool statically links cloudflared, Copyright (c) Cloudflare, Inc.,
  licensed under the Apache License, Version 2.0. Source and full license
  text: https://github.com/cloudflare/cloudflared
  This is an independent tool, not an official Cloudflare product.
`)
}

// ---------------------------------------------------------------------
// Sender mode: le-fi <channel-name> < file_to_send.bin
// No cloudflared library needed on this side at all - just a plain HTTP PUT.
// ---------------------------------------------------------------------

func runSender(channel string) int {
	url := fmt.Sprintf("https://%s.trycloudflare.com/", channel)

	var req *http.Request
	body := &crcTeeReader{
		r:   os.Stdin,
		crc: crc32.NewIEEE(),
		onEOF: func(sum uint32) {
			// req is already assigned by the time reading finishes - the
			// HTTP client only starts reading the body after NewRequest
			// returns, so this closure always sees the real request.
			req.Trailer.Set(crc32TrailerHeader, fmt.Sprintf("%08x", sum))
		},
	}

	var err error
	req, err = http.NewRequest(http.MethodPut, url, body)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error building request:", err)
		return 1
	}
	req.Header.Set("Content-Type", "application/octet-stream")
	// Pre-announce the trailer key; the value is filled in by crcTeeReader
	// once the body is fully read. Chunked encoding (implied here since we
	// never set ContentLength) is required for HTTP/1.1 trailers.
	req.Trailer = http.Header{crc32TrailerHeader: nil}

	client := &http.Client{Timeout: 0}
	resp, err := client.Do(req)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error sending file:", err)
		return 1
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)

	if resp.StatusCode >= 300 {
		fmt.Fprintf(os.Stderr, "transfer failed: HTTP %d\n%s\n", resp.StatusCode, respBody)
		return 1
	}
	fmt.Fprintln(os.Stderr, "Transfer complete.")
	return 0
}

// ---------------------------------------------------------------------
// Receiver mode: le-fi > out.bin
// ---------------------------------------------------------------------

func runReceiver() int {
	// Keep a working copy of the real stderr for OUR OWN status messages,
	// then silence the process's stderr so cloudflared's (hardcoded-on)
	// internal terminal logging doesn't clutter the output. cloudflared's
	// own logs still go to a logfile we read from to recover the tunnel
	// hostname.
	ui, restore := silenceStderrKeepingCopy()
	defer restore()

	logPath, err := os.CreateTemp("", "le-fi-*.log")
	if err != nil {
		fmt.Fprintln(ui, "error creating temp logfile:", err)
		return 1
	}
	logPath.Close()
	defer os.Remove(logPath.Name())

	receivedOnce := false
	done := make(chan xferResult, 1)
	prog := newProgress()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		fmt.Fprintln(ui, "error opening local listener:", err)
		return 1
	}
	localPort := ln.Addr().(*net.TCPAddr).Port

	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if receivedOnce {
			http.Error(w, "already received a file, refusing further uploads\n", http.StatusConflict)
			return
		}
		if r.Method != http.MethodPut && r.Method != http.MethodPost {
			http.Error(w, "use PUT or POST\n", http.StatusMethodNotAllowed)
			return
		}
		receivedOnce = true
		prog.markReceiving()

		crc := crc32.NewIEEE()
		teed := io.TeeReader(r.Body, crc)                  // feeds crc as bytes are read
		counted := &countingReader{r: teed, p: prog}        // updates live progress
		n, err := io.Copy(os.Stdout, counted)
		if err != nil {
			http.Error(w, "error\n", http.StatusInternalServerError)
			done <- xferResult{bytes: n, err: err}
			return
		}

		// r.Trailer is only populated once the body has been fully
		// consumed - which io.Copy just did above. A missing trailer
		// means the sender was curl/wget/etc, not this utility, so no
		// checksum was ever offered and none can be verified.
		expected := r.Trailer.Get(crc32TrailerHeader)
		computed := fmt.Sprintf("%08x", crc.Sum32())

		if expected != "" && !strings.EqualFold(expected, computed) {
			http.Error(w, fmt.Sprintf("checksum mismatch: expected %s, got %s\n", expected, computed), http.StatusUnprocessableEntity)
			done <- xferResult{bytes: n, crcChecked: true, crcOK: false, expected: expected, computed: computed}
			return
		}

		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok\n"))
		done <- xferResult{bytes: n, crcChecked: expected != "", crcOK: true, expected: expected, computed: computed}
	})
	srv := &http.Server{Handler: mux}
	go srv.Serve(ln)

	// Bring up the tunnel in-process via cloudflared's own Go packages.
	bInfo := cliutil.GetBuildInfo("", "le-fi")
	graceShutdownC := make(chan struct{})
	tunnel.Init(bInfo, graceShutdownC)

	app := &cli.App{
		Name:     "cloudflared",
		Commands: tunnel.Commands(),
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	tunnelErrC := make(chan error, 1)
	go func() {
		tunnelErrC <- app.RunContext(ctx, []string{
			"cloudflared", "tunnel",
			"--url", fmt.Sprintf("http://127.0.0.1:%d", localPort),
			"--no-autoupdate",
			"--logfile", logPath.Name(),
		})
	}()

	hostname, err := waitForTunnelHostname(logPath.Name(), tunnelErrC, 20*time.Second)
	if err != nil {
		fmt.Fprintln(ui, "error:", err)
		cancel()
		return 1
	}
	channel := hostname[:len(hostname)-len(".trycloudflare.com")]
	tunnelURL := "https://" + hostname + "/"

	fmt.Fprintln(ui, "Tunnel is up (no account, no port forwarding - works through any NAT).")
	fmt.Fprintln(ui)
	fmt.Fprintln(ui, "If the remote party does NOT have this utility, they can send with:")
	fmt.Fprintln(ui)
	fmt.Fprintf(ui, "  curl -T - %s < file_to_be_send.bin\n", tunnelURL)
	fmt.Fprintf(ui, "  wget --method=PUT --body-file=file_to_be_send.bin %s -O -\n", tunnelURL)
	fmt.Fprintln(ui)
	fmt.Fprintln(ui, "If the remote party also has this utility, they can send with:")
	fmt.Fprintln(ui)
	fmt.Fprintf(ui, "  le-fi %s < file_to_be_send.bin\n", channel)
	fmt.Fprintln(ui)
	fmt.Fprintln(ui, "Channel name:")
	fmt.Fprintln(ui)
	fmt.Fprintf(ui, "  %s\n", channel)
	fmt.Fprintln(ui)

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)

	stopSpinner := runSpinner(ui, prog)
	exitCode := 0

	select {
	case res := <-done:
		stopSpinner()
		switch {
		case res.err != nil:
			printFinal(ui, "✗", "31", fmt.Sprintf(
				"transfer failed after %s (%s received): connection dropped: %v",
				formatDuration(prog.xferElapsed()), formatBytes(res.bytes), res.err))
			exitCode = 1
		case res.crcChecked && !res.crcOK:
			printFinal(ui, "✗", "31", fmt.Sprintf(
				"CHECKSUM MISMATCH after %s, %s received (expected crc32 %s, got %s) - data may be corrupted",
				formatDuration(prog.xferElapsed()), formatBytes(res.bytes), res.expected, res.computed))
			exitCode = 1
		case res.crcChecked:
			printFinal(ui, "✓", "32", fmt.Sprintf(
				"Received %s in %s - checksum verified (crc32 %s).",
				formatBytes(res.bytes), formatDuration(prog.xferElapsed()), res.computed))
		default:
			printFinal(ui, "✓", "32", fmt.Sprintf(
				"Received %s in %s. (no checksum to verify - sender likely used curl/wget)",
				formatBytes(res.bytes), formatDuration(prog.xferElapsed())))
		}
	case <-sigCh:
		stopSpinner()
		phase := "waiting for a connection"
		if xferPhase(prog.phase.Load()) == phaseReceiving {
			phase = fmt.Sprintf("receiving (%s so far)", formatBytes(prog.bytes.Load()))
		}
		printFinal(ui, "✗", "33", fmt.Sprintf("cancelled after %s, %s.", formatDuration(time.Since(prog.waitStart)), phase))
		cancel()
		return 1
	}

	// Give the final HTTP response a moment to flush before tearing down.
	time.Sleep(300 * time.Millisecond)
	cancel()
	_ = srv.Close()
	select {
	case <-tunnelErrC:
	case <-time.After(3 * time.Second):
	}
	return exitCode
}

func waitForTunnelHostname(logPath string, tunnelErrC chan error, timeout time.Duration) (string, error) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		select {
		case err := <-tunnelErrC:
			return "", fmt.Errorf("cloudflared tunnel exited early: %w", err)
		default:
		}
		data, err := os.ReadFile(logPath)
		if err == nil {
			if m := tunnelHostnameRe.FindStringSubmatch(string(data)); m != nil {
				return m[1], nil
			}
		}
		time.Sleep(150 * time.Millisecond)
	}
	return "", fmt.Errorf("timed out waiting for a tunnel hostname")
}

// silenceStderrKeepingCopy redirects the process's real stderr (fd 2) to
// /dev/null and returns a duplicate file descriptor still connected to the
// original terminal, so our own status messages stay visible while
// cloudflared's own (always-on) console logging is suppressed.
//
// Uses golang.org/x/sys/unix rather than the standard syscall package:
// syscall.Dup2 is undefined on linux/arm64 (that syscall table only has
// dup3, not dup2 - a real build break found while cross-compiling this
// tool). unix.Dup2 abstracts over that difference on all our targets
// (linux/amd64, linux/arm64, darwin/amd64, darwin/arm64).
func silenceStderrKeepingCopy() (ui *os.File, restore func()) {
	devNull, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err != nil {
		return os.Stderr, func() {}
	}
	origFd, err := unix.Dup(int(os.Stderr.Fd()))
	if err != nil {
		devNull.Close()
		return os.Stderr, func() {}
	}
	if err := unix.Dup2(int(devNull.Fd()), int(os.Stderr.Fd())); err != nil {
		devNull.Close()
		return os.Stderr, func() {}
	}
	return os.NewFile(uintptr(origFd), "stderr-copy"), func() {
		_ = unix.Dup2(origFd, int(os.Stderr.Fd()))
		devNull.Close()
	}
}
