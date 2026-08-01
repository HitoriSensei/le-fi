package main

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"
)

// sourceProvider opens a fresh, from-the-start reader over whatever's
// being sent. Called once per GET attempt, so a failed attempt can be
// retried without needing to rewind a non-seekable source.
type sourceProvider func() (io.ReadCloser, error)

// runSendServer implements the GET-servable sending modes: brings up a
// tunnel, waits for a GET, streams the source as the response body. On a
// dropped connection it stays up and waits for another attempt - only a
// successful transfer closes it.
func runSendServer(filename string, size int64, provider sourceProvider) int {
	ui, restore := silenceStderrKeepingCopy()
	defer restore()

	done := make(chan xferResult, 1)
	prog := newProgress("sending")

	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Le-Fi-Role", "sender")

		if r.Method == http.MethodPost || r.Method == http.MethodPut {
			http.Error(w, "this end is offering to SEND a file, not receive one\n", http.StatusNotAcceptable)
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			http.Error(w, "use GET\n", http.StatusMethodNotAllowed)
			return
		}

		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", filename))
		if size >= 0 {
			w.Header().Set("Content-Length", strconv.FormatInt(size, 10))
		}

		if r.Method == http.MethodHead {
			w.WriteHeader(http.StatusOK)
			return
		}

		src, err := provider()
		if err != nil {
			http.Error(w, "error opening source\n", http.StatusInternalServerError)
			return
		}
		defer src.Close()

		prog.markActive()
		counted := &countingWriter{w: w, p: prog}
		n, err := io.Copy(counted, src)
		done <- xferResult{bytes: n, err: err}
	})

	sess, err := startTunnel(ui, mux)
	if err != nil {
		fmt.Fprintln(ui, "error:", err)
		return 1
	}

	fmt.Fprintln(ui, "If the remote party does NOT have this utility, they can receive with:")
	fmt.Fprintln(ui)
	fmt.Fprintf(ui, "  curl -OJ %s\n", sess.url)
	fmt.Fprintf(ui, "  wget --content-disposition %s\n", sess.url)
	fmt.Fprintln(ui)
	fmt.Fprintln(ui, "...or just open this link in a browser to download it directly:")
	fmt.Fprintln(ui)
	fmt.Fprintf(ui, "  %s\n", sess.url)
	fmt.Fprintln(ui)
	fmt.Fprintln(ui, "If the remote party also has this utility, they can receive with:")
	fmt.Fprintln(ui)
	fmt.Fprintf(ui, "  le-fi %s > %s\n", sess.channel, filename)
	fmt.Fprintln(ui)
	fmt.Fprintln(ui, "Channel name:")
	fmt.Fprintln(ui)
	fmt.Fprintf(ui, "  %s\n", sess.channel)
	fmt.Fprintln(ui)

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)

	stopSpinner := runSpinner(ui, prog)
	exitCode := 0

loop:
	for {
		select {
		case res := <-done:
			if res.err != nil {
				// Failed mid-transfer (dropped connection etc) - stay up
				// and wait for a retry, per spec: this server only
				// closes after a successful transfer.
				prog.reset()
				continue loop
			}
			stopSpinner()
			printFinal(ui, "✓", "32", fmt.Sprintf(
				"Sent %s in %s.", formatBytes(res.bytes), formatDuration(prog.xferElapsed())))
			break loop
		case <-sigCh:
			stopSpinner()
			phase := "waiting for a connection"
			if prog.isActive() {
				phase = fmt.Sprintf("sending (%s so far)", formatBytes(prog.bytes.Load()))
			}
			printFinal(ui, "✗", "33", fmt.Sprintf("cancelled after %s, %s.", formatDuration(time.Since(prog.waitStart)), phase))
			sess.shutdown()
			return 1
		}
	}

	sess.shutdown()
	return exitCode
}

// runSendFromStdin implements `le-fi < file_to_send`. stdin isn't
// seekable, so to support retrying a failed attempt it's buffered to a
// temp file first (streamed, bounded memory - not held in RAM) and served
// from there.
func runSendFromStdin() int {
	fmt.Fprintln(os.Stderr, "Reading input...")

	tmp, err := os.CreateTemp("", "le-fi-send-*")
	if err != nil {
		fmt.Fprintln(os.Stderr, "error creating temp file:", err)
		return 1
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)

	n, err := io.Copy(tmp, os.Stdin)
	closeErr := tmp.Close()
	if err != nil {
		fmt.Fprintln(os.Stderr, "error buffering stdin:", err)
		return 1
	}
	if closeErr != nil {
		fmt.Fprintln(os.Stderr, "error buffering stdin:", closeErr)
		return 1
	}

	filename := fmt.Sprintf("%s-%s.bin", slugify(currentUsername()), time.Now().Format("2006-01-02"))
	provider := func() (io.ReadCloser, error) {
		return os.Open(tmpPath)
	}
	return runSendServer(filename, n, provider)
}

// runSendFromFile implements `le-fi filetosend` (a real, existing file
// path, no piping involved).
func runSendFromFile(path string) int {
	info, err := os.Stat(path)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	filename := info.Name()
	size := info.Size()
	provider := func() (io.ReadCloser, error) {
		return os.Open(path)
	}
	return runSendServer(filename, size, provider)
}

// runSendFromDirectory implements `le-fi directory/`: the directory is
// stream-compressed to tar.gz (tar's sequential, no-central-directory
// format plus gzip both write straight through with no seeking needed,
// so this works for a directory of any size with nothing buffered to
// disk or memory) and sent as that single file.
func runSendFromDirectory(path string) int {
	base := dirBaseName(path)
	filename := base + ".tar.gz"

	provider := func() (io.ReadCloser, error) {
		pr, pw := io.Pipe()
		go func() {
			err := streamTarGz(pw, path)
			_ = pw.CloseWithError(err)
		}()
		return pr, nil
	}
	// Compressed size isn't known ahead of time - no Content-Length.
	return runSendServer(filename, -1, provider)
}
