package main

import (
	"crypto/tls"
	"fmt"
	"hash/crc32"
	"io"
	"net/http"
	"os"
)

// runClientReceive implements `le-fi <channel> > out`: a plain GET-pull
// from a remote send-server. This is also exactly what happens when the
// channel URL is opened directly in a browser.
//
// A password-protected sender gets its password sorted out first (see
// resolvePassword), so a wrong one fails here and the sender stays up.
func runClientReceive(channel, password string) int {
	url := targetURL(channel)

	password, ok := resolvePassword(url, password, "sender",
		fmt.Sprintf("le-fi %s -p <password> > <output file>", channel))
	if !ok {
		return 1
	}

	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error building request:", err)
		return 1
	}
	if password != "" {
		req.SetBasicAuth(basicAuthUser, password)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error connecting:", err)
		return 1
	}
	defer resp.Body.Close()

	if resp.Header.Get("X-Le-Fi-Role") == "receiver" {
		fmt.Fprintln(os.Stderr, "The other end is waiting to RECEIVE a file, not send one.")
		fmt.Fprintf(os.Stderr, "Did you mean: le-fi %s < <file to send>\n", channel)
		return 1
	}
	if resp.StatusCode >= 300 {
		body, _ := io.ReadAll(resp.Body)
		fmt.Fprintf(os.Stderr, "transfer failed: HTTP %d\n%s\n", resp.StatusCode, body)
		return 1
	}

	n, err := io.Copy(os.Stdout, resp.Body)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error receiving file:", err)
		return 1
	}
	fmt.Fprintf(os.Stderr, "Received %s.\n", formatBytes(n))
	return 0
}

// runClientSend implements `le-fi <channel> < file` and
// `le-fi <channel> file`: a POST/PUT-push to a remote receive-server.
// body is os.Stdin for the piped form, or an opened file for the
// two-argument form.
//
// A password-protected receiver gets its password sorted out with a HEAD
// before the PUT, as a 401 to the PUT itself could come after part of a
// piped stdin was already read and lost.
func runClientSend(channel string, body io.Reader, password string) int {
	url := targetURL(channel)

	password, ok := resolvePassword(url, password, "receiver",
		fmt.Sprintf("le-fi %s -p <password> < <file to send>", channel))
	if !ok {
		return 1
	}

	var req *http.Request
	crcBody := &crcTeeReader{
		r:   body,
		crc: crc32.NewIEEE(),
	}

	var err error
	req, err = http.NewRequest(http.MethodPut, url, crcBody)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error building request:", err)
		return 1
	}
	crcBody.onEOF = func(sum uint32) {
		req.Trailer.Set(crc32TrailerHeader, fmt.Sprintf("%08x", sum))
	}
	req.Header.Set("Content-Type", "application/octet-stream")
	if password != "" {
		req.SetBasicAuth(basicAuthUser, password)
	}
	req.Trailer = http.Header{crc32TrailerHeader: nil}

	// Force HTTP/1.1: the request carries a trailer (the CRC-32 checksum,
	// only known once the body's fully read), and some TLS-inspecting
	// corporate proxies/VPN clients mishandle the HTTP/2 trailer HEADERS
	// frame that comes after the DATA frames, killing the connection with
	// an immediate GOAWAY(PROTOCOL_ERROR) before any real response - seen
	// in the wild on macOS behind such a proxy. HTTP/2 buys nothing here
	// (a single PUT, no multiplexing), so just avoid the whole class of
	// bugs: chunked-encoding trailers over HTTP/1.1 are much more widely
	// supported by middleboxes.
	client := &http.Client{
		Timeout: 0,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{NextProtos: []string{"http/1.1"}},
			TLSNextProto:    map[string]func(string, *tls.Conn) http.RoundTripper{},
		},
	}
	resp, err := client.Do(req)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error sending file:", err)
		return 1
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)

	if resp.StatusCode == http.StatusNotAcceptable || resp.Header.Get("X-Le-Fi-Role") == "sender" {
		fmt.Fprintln(os.Stderr, "The other end is offering to SEND a file, not receive one.")
		fmt.Fprintf(os.Stderr, "Did you mean: le-fi %s > <output file>\n", channel)
		fmt.Fprintf(os.Stderr, "...or just open %s in a browser.\n", url)
		return 1
	}
	if resp.StatusCode >= 300 {
		fmt.Fprintf(os.Stderr, "transfer failed: HTTP %d\n%s\n", resp.StatusCode, respBody)
		return 1
	}
	fmt.Fprintln(os.Stderr, "Transfer complete.")
	return 0
}
