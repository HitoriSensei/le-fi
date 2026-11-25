// le-fi: send/receive a file across NATs using a Cloudflare Quick Tunnel,
// without requiring the separate `cloudflared` binary to be installed.
// This links cloudflared's own Go packages directly into this binary and
// drives them in-process.
//
// See `le-fi -h` (or just run it with nothing piped) for full usage.
package main

import (
	"fmt"
	"os"

	"golang.org/x/term"
)

func main() {
	args := os.Args[1:]

	if len(args) > 0 && (args[0] == "-h" || args[0] == "--help") {
		printHelp()
		os.Exit(0)
	}

	args, password, _, err := extractPassword(args)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}

	stdinPiped := !term.IsTerminal(int(os.Stdin.Fd()))
	stdoutPiped := !term.IsTerminal(int(os.Stdout.Fd()))

	switch len(args) {
	case 0:
		os.Exit(dispatchNoArgs(stdinPiped, stdoutPiped, password))
	case 1:
		os.Exit(dispatchOneArg(args[0], stdinPiped, stdoutPiped, password))
	case 2:
		os.Exit(dispatchTwoArgs(args[0], args[1], password))
	default:
		printHelp()
		os.Exit(1)
	}
}

// dispatchNoArgs handles `le-fi`, `le-fi > out`, and `le-fi < file`.
func dispatchNoArgs(stdinPiped, stdoutPiped bool, password string) int {
	switch {
	case !stdinPiped && !stdoutPiped:
		// Bare invocation, nothing piped either way - almost certainly
		// someone just running it to see what it does, not an actual
		// transfer. Show help instead of silently opening a real tunnel.
		printHelp()
		return 0
	case stdoutPiped && !stdinPiped:
		return runReceiveServer(password)
	case stdinPiped && !stdoutPiped:
		return runSendFromStdin(password)
	default:
		fmt.Fprintln(os.Stderr, "error: both stdin and stdout are redirected - not sure whether to send or receive.")
		fmt.Fprintln(os.Stderr, "Use 'le-fi > out' to receive, or 'le-fi < file' to send.")
		return 1
	}
}

// dispatchOneArg handles `le-fi filetosend`, `le-fi directory/`,
// `le-fi <channel> > out`, and `le-fi <channel> < file`.
func dispatchOneArg(arg string, stdinPiped, stdoutPiped bool, password string) int {
	if info, err := os.Stat(arg); err == nil {
		if info.IsDir() {
			return runSendFromDirectory(arg, password)
		}
		return runSendFromFile(arg, password)
	}

	// Doesn't exist as a local path - treat it as a channel to connect to.
	channel := arg
	switch {
	case stdoutPiped:
		return runClientReceive(channel, password)
	case stdinPiped:
		return runClientSend(channel, os.Stdin, password)
	default:
		fmt.Fprintf(os.Stderr, "error: %q isn't a file or directory here, and no direction was given.\n", arg)
		fmt.Fprintf(os.Stderr, "Use 'le-fi %s > out' to receive from that channel, or 'le-fi %s < file' to send to it.\n", arg, arg)
		return 1
	}
}

// dispatchTwoArgs handles `le-fi <channel> filetosend`.
func dispatchTwoArgs(channel, path, password string) int {
	f, err := os.Open(path)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error opening file:", err)
		return 1
	}
	defer f.Close()
	return runClientSend(channel, f, password)
}

func printHelp() {
	fmt.Print(`le-fi - send or receive a file across NATs, no port forwarding

USAGE:
  Receive a file, waiting for someone to push it to you:

      le-fi > received_file.bin

  Opening the printed link in a browser shows a drop-zone page the other
  person can use even without any of this - drag a file on, done.

  Send a file, waiting for someone to pull it from you:

      le-fi < file_to_send.bin
      le-fi file_to_send.bin
      le-fi a_directory/            (streamed as a .tar.gz)

  The printed link can also just be opened directly in a browser to
  download the file.

  If you already have a channel name from the other party:

      le-fi <channel-name> > out.bin          receive from them
      le-fi <channel-name> < file_to_send.bin  send to them
      le-fi <channel-name> file_to_send.bin    (same, no piping needed)

  Get the direction backwards? le-fi will tell you and suggest the fix.

  Require a password to download what you send, or to upload to you:

      le-fi file_to_send.bin -p <password>
      le-fi -p <password> > received_file.bin

  Opening the link asks for a login, any username is accepted.
  A wrong password doesn't end the transfer, the other side can retry.
  Use a password-protected channel:

      le-fi <channel-name> > out.bin              (asks for the password)
      le-fi <channel-name> -p <password> > out.bin
      le-fi <channel-name> -p <password> < file_to_send.bin

FLAGS:
  -p, --password <password>
                Without a channel name: require this password from
                the other side.
                With a channel name: use this password, if omitted
                le-fi asks for it.
  -h, --help    Show this help and exit.

Uses a Cloudflare Quick Tunnel (the same free, unauthenticated tunnels
behind trycloudflare.com) to open a temporary public HTTPS endpoint to
this machine - works through symmetric NAT/CGNAT, no account, no router
config. Links cloudflared's own Go packages directly into this binary and
drives them in-process, so no separate cloudflared executable needs to be
installed anywhere.

ATTRIBUTION:
  This tool statically links cloudflared, Copyright (c) Cloudflare, Inc.,
  licensed under the Apache License, Version 2.0. Source and full license
  text: https://github.com/cloudflare/cloudflared
  This is an independent tool, not an official Cloudflare product.
`)
}
