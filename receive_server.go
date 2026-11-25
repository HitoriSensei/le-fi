package main

import (
	"fmt"
	"hash/crc32"
	"io"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"
)

// runReceiveServer implements `le-fi > out.bin`: brings up a tunnel, then
// waits for either a browser (GET, served the upload page) or an upload
// (POST/PUT, written straight to stdout). Single-shot: closes after one
// transfer attempt, successful or not.
//
// When password is non-empty, both the upload page (GET/HEAD) and the
// upload itself (PUT/POST) need HTTP basic auth with that password (any
// username). A failed login is only a 401 for that request, so it never
// uses up the single transfer attempt.
func runReceiveServer(password string) int {
	// Keep a working copy of the real stderr for OUR OWN status messages,
	// then silence the process's stderr so cloudflared's (hardcoded-on)
	// internal terminal logging doesn't clutter the output.
	ui, restore := silenceStderrKeepingCopy()
	defer restore()

	identity := localIdentity()
	pageBytes, err := renderUploadPage(identity)
	if err != nil {
		fmt.Fprintln(ui, "error rendering upload page:", err)
		return 1
	}

	receivedOnce := false
	done := make(chan xferResult, 1)
	prog := newProgress("receiving")

	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Le-Fi-Role", "receiver")

		// Before receivedOnce is touched: a failed login must leave the
		// receive exactly as it was.
		if !checkBasicAuth(w, r, password) {
			return
		}

		if r.Method == http.MethodGet || r.Method == http.MethodHead {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.WriteHeader(http.StatusOK)
			if r.Method == http.MethodGet {
				_, _ = w.Write(pageBytes)
			}
			return
		}

		if receivedOnce {
			http.Error(w, "already received a file, refusing further uploads\n", http.StatusConflict)
			return
		}
		if r.Method != http.MethodPut && r.Method != http.MethodPost {
			http.Error(w, "use PUT or POST\n", http.StatusMethodNotAllowed)
			return
		}
		receivedOnce = true
		prog.markActive()

		crc := crc32.NewIEEE()
		teed := io.TeeReader(r.Body, crc)            // feeds crc as bytes are read
		counted := &countingReader{r: teed, p: prog} // updates live progress
		n, err := io.Copy(os.Stdout, counted)
		if err != nil {
			http.Error(w, "error\n", http.StatusInternalServerError)
			done <- xferResult{bytes: n, err: err}
			return
		}

		// r.Trailer is only populated once the body has been fully
		// consumed - which io.Copy just did above. A missing trailer
		// means the sender was curl/wget/a browser, not this utility, so
		// no checksum was ever offered and none can be verified.
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

	sess, err := startTunnel(ui, mux)
	if err != nil {
		fmt.Fprintln(ui, "error:", err)
		return 1
	}

	fmt.Fprintln(ui, "If the remote party does NOT have this utility, they can send with:")
	fmt.Fprintln(ui)
	if password != "" {
		// Without the password on the command line, curl and wget ask
		// for it, so it isn't shown here.
		fmt.Fprintf(ui, "  curl -u %s -T - %s < file_to_be_send.bin\n", basicAuthUser, sess.url)
		fmt.Fprintf(ui, "  wget --user=%s --ask-password --method=PUT --body-file=file_to_be_send.bin %s -O -\n", basicAuthUser, sess.url)
	} else {
		fmt.Fprintf(ui, "  curl -T - %s < file_to_be_send.bin\n", sess.url)
		fmt.Fprintf(ui, "  wget --method=PUT --body-file=file_to_be_send.bin %s -O -\n", sess.url)
	}
	fmt.Fprintln(ui)
	fmt.Fprintln(ui, "...or just open this link in a browser and drop the file on the page:")
	fmt.Fprintln(ui)
	fmt.Fprintf(ui, "  %s\n", sess.url)
	fmt.Fprintln(ui)
	fmt.Fprintln(ui, "If the remote party also has this utility, they can send with:")
	fmt.Fprintln(ui)
	fmt.Fprintf(ui, "  le-fi %s < file_to_be_send.bin\n", sess.channel)
	fmt.Fprintln(ui)
	if password != "" {
		fmt.Fprintln(ui, "Password-protected: they'll be asked for the -p password, any username works.")
		fmt.Fprintln(ui, "Note: a wrong password doesn't end the receive, they can retry.")
		fmt.Fprintln(ui)
	}
	fmt.Fprintln(ui, "Channel name:")
	fmt.Fprintln(ui)
	fmt.Fprintf(ui, "  %s\n", sess.channel)
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
				"Received %s in %s. (no checksum to verify - sender likely used curl/wget/a browser)",
				formatBytes(res.bytes), formatDuration(prog.xferElapsed())))
		}
	case <-sigCh:
		stopSpinner()
		phase := "waiting for a connection"
		if prog.isActive() {
			phase = fmt.Sprintf("receiving (%s so far)", formatBytes(prog.bytes.Load()))
		}
		printFinal(ui, "✗", "33", fmt.Sprintf("cancelled after %s, %s.", formatDuration(time.Since(prog.waitStart)), phase))
		sess.shutdown()
		return 1
	}

	sess.shutdown()
	return exitCode
}
