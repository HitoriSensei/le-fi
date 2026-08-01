package main

import (
	"fmt"
	"io"
	"os"
	"sync/atomic"
	"time"

	"golang.org/x/term"
)

// ---------------------------------------------------------------------
// Live transfer progress: spinner + elapsed time + byte counter while
// waiting/active, a final checkmark/cross line once it settles. Used by
// both the receiving side (bytes flowing in) and the sending side (bytes
// flowing out) - the verb shown ("receiving"/"sending") is just a label.
// ---------------------------------------------------------------------

type xferPhase int32

const (
	phaseWaiting xferPhase = iota
	phaseActive
	phaseDone
	phaseFailed
	phaseCancelled
)

// xferResult carries the outcome of a single transfer attempt from the
// HTTP handler back to the main goroutine: how much moved, whether the
// connection failed outright, and whether a CRC-32 trailer was present
// and matched.
type xferResult struct {
	bytes      int64
	err        error // non-nil: the connection dropped/errored mid-transfer
	crcChecked bool  // a checksum trailer was present at all
	crcOK      bool  // meaningful only if crcChecked
	expected   string
	computed   string
}

type progress struct {
	verb        string // "receiving" or "sending", for the status line
	phase       atomic.Int32
	bytes       atomic.Int64
	waitStart   time.Time // set once, before the spinner goroutine starts
	xferStartNs atomic.Int64
}

func newProgress(verb string) *progress {
	return &progress{verb: verb, waitStart: time.Now()}
}

func (p *progress) markActive() {
	if p.phase.CompareAndSwap(int32(phaseWaiting), int32(phaseActive)) {
		p.xferStartNs.Store(time.Now().UnixNano())
	}
}

func (p *progress) isActive() bool {
	return xferPhase(p.phase.Load()) == phaseActive
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

// reset returns a failed attempt back to "waiting", for send-servers that
// stay up and accept a retry after a dropped connection.
func (p *progress) reset() {
	p.bytes.Store(0)
	p.xferStartNs.Store(0)
	p.waitStart = time.Now()
	p.phase.Store(int32(phaseWaiting))
}

// countingReader wraps an io.Reader, adding each chunk read to a progress
// counter and flipping the phase to "active" the moment bytes start
// flowing.
type countingReader struct {
	r io.Reader
	p *progress
}

func (cr *countingReader) Read(buf []byte) (int, error) {
	n, err := cr.r.Read(buf)
	if n > 0 {
		cr.p.markActive()
		cr.p.addBytes(int64(n))
	}
	return n, err
}

// countingWriter mirrors countingReader for the sending direction.
type countingWriter struct {
	w io.Writer
	p *progress
}

func (cw *countingWriter) Write(buf []byte) (int, error) {
	n, err := cw.w.Write(buf)
	if n > 0 {
		cw.p.markActive()
		cw.p.addBytes(int64(n))
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
	case phaseActive:
		line = fmt.Sprintf("%s %s data... (%s, %s so far)",
			frame, p.verb, formatDuration(p.xferElapsed()), formatBytes(p.bytes.Load()))
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
