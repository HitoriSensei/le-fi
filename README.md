# le-fi

Just want to send or receive a file? Grab a release and read
`USER_GUIDE.txt` instead — this doc is for building it and poking
around under the hood.

Sends or receives a file (or a directory) across NATs using a Cloudflare
Quick Tunnel, without the `cloudflared` binary installed anywhere.
Imports cloudflared's own Go packages directly and drives them in-process
(`cli.App.RunContext(...)`) instead of shelling out to a separate
executable. The receiving end also serves a small browser drop-zone UI
for people with no tooling installed.

## Installing

**Linux:** grab `le-fi-<tag>-linux-amd64.zip` or `-linux-arm64.zip` from
[Releases](https://git.piesekpokojowy.uk/Hitori/le-fi/releases).

**macOS:** Homebrew is recommended - a downloaded binary will likely get
blocked by Gatekeeper (see the macOS section below for why):

```bash
brew tap HitoriSensei/le-fi
brew install le-fi
```

Or grab the `darwin-amd64`/`darwin-arm64` zip from Releases and clear the
quarantine flag before using it: `xattr -d com.apple.quarantine le-fi`.

## Build

```bash
go build -o le-fi .
```

Release builds add size/speed flags:

```bash
CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o le-fi .
```

`-s -w` strips debug symbols (~30% smaller), `-trimpath` drops local
filesystem paths from the binary, `CGO_ENABLED=0` gives a fully static
binary with no libc dependency.

Requires Go 1.26+ (or just a modern Go with `GOTOOLCHAIN=auto`, the
default since Go 1.21 — it'll fetch 1.26 for you automatically).

The first build will download a sizeable dependency tree (cloudflared
pulls in quic-go, grpc, OpenTelemetry, Sentry, etc.) — that's a one-time
cost, cached afterward by the Go module cache.

### Why the `replace` lines in go.mod matter

cloudflared's own `go.mod` carries four `replace` directives — most
importantly a Cloudflare-maintained fork of `urfave/cli` and a fork of
`quic-go`. **Go does not propagate a dependency's `replace` directives to
downstream modules that import it.** Without mirroring them here, the
build fails with `undefined: altsrc.ApplyInputSource` (found this the
hard way). The four lines are already in this
`go.mod` — if the cloudflared version is ever bumped, the
[cloudflared's go.mod](https://github.com/cloudflare/cloudflared/blob/master/go.mod) needs to be checked for change in those lines.

## Usage

Run `./le-fi -h` (or `--help`) any time for this from the binary itself.
Running it bare with no arguments and nothing piped either way also shows
this help instead of silently opening a real tunnel.

### Receive a file

```bash
le-fi > out.bin
```

Waits for someone to push a file to you. Prints a curl/wget command, a
`le-fi` command, and a plain link that opens a full-page drop zone in a
browser - drag a file on, done, no tooling needed on their end at all.
The drop zone shows your username@hostname so whoever's sending knows
who they're sending to.

Single-shot: closes after one transfer attempt, successful or not.

### Send a file

```bash
le-fi < file_to_send.bin      # from a pipe
le-fi file_to_send.bin        # from a path, real filename used
le-fi a_directory/            # streamed as a .tar.gz, no temp file
```

Waits for someone to pull the file from you - a curl/wget download, a
browser opening the link directly, or another `le-fi` on the other end.
Unlike receiving, this **stays up and waits for a retry** if a transfer
drops mid-way; it only closes after one actually succeeds. Sending from a
pipe buffers to a temp file first (streamed, not held in memory) so a
failed attempt can be retried without needing to rewind stdin.

### Talk to a channel you already have

```bash
le-fi <channel-name> > out.bin           # pull from them
le-fi <channel-name> < file_to_send.bin  # push to them
le-fi <channel-name> file_to_send.bin    # same, no piping needed
```

Get the direction backwards - e.g. try to push to something that's
actually offering to send - and le-fi detects it (via a response header
both ends set) and tells you the fix instead of failing silently:

```
The other end is offering to SEND a file, not receive one.
Did you mean: le-fi <channel-name> > out.bin
...or just open https://<channel-name>.trycloudflare.com in a browser.
```

The tool exits automatically once a transfer is fully resolved, and tears
down the tunnel cleanly (no lingering processes).

## Testing

```bash
go test ./...
```

Two env vars, for tests and local debugging - not for real transfers,
since they skip the tunnel entirely:

- `LEFI_LOCAL=1` — skip Cloudflare, talk plain HTTP over localhost. The
  sender's positional argument becomes `host:port` instead of a channel
  name (e.g. `le-fi 127.0.0.1:9000 < file`).
- `LEFI_PORT=<port>` — receiver binds this exact port instead of a random
  one.

Tests spawn the real binary as a subprocess with a genuine pseudo-terminal
attached to whichever stream needs to look interactive (stdin for
receive-like modes, stdout for send-like modes) rather than piped -
matching what a real shell invocation looks like, which the dispatch
logic in `main.go` depends on to tell the six usage patterns apart.
`github.com/creack/pty` provides that; it's a test-only dependency and
never gets linked into the shipped binary.

`.gitea/workflows/test.yml` runs `go vet` and `go test -race` on every
push to `main`, then builds the actual release-flagged binary
(`CGO_ENABLED=0 -trimpath -ldflags="-s -w"`) and re-runs the whole suite
against that real artifact via `LEFI_TEST_BIN=<path> go test ./...`.

## macOS

Newer macOS versions will very likely block a downloaded release binary
outright. Gatekeeper requires software to be notarized through a paid
Apple Developer ID ($99/year) before it'll run without a warning, and
this project isn't paying Apple for that - see Installing above for the
Homebrew/`xattr` workarounds.

## Homebrew

A tap lives at `github.com/HitoriSensei/homebrew-le-fi` - GitHub only, no
Gitea copy, since a tap just needs to be somewhere `brew tap`'s short
form resolves (GitHub) and its formula's `url` field points back at this
repo's own release tarball regardless. Builds from source on install -
`depends_on "go" => :build`, same `CGO_ENABLED=0` + stripped/trimmed
flags as the release CI. Formula installs never set the quarantine
attribute Gatekeeper checks for, so macOS's Apple-signing question
doesn't come up at all for this path.

The formula updates automatically on every release. Install command is
in the Installing section above.

## Releases

Pushing a tag matching `v*` (e.g. `git tag v0.1.0 && git push origin v0.1.0`)
triggers `.gitea/workflows/release.yml`, which builds `linux/amd64`,
`linux/arm64`, `darwin/amd64`, and `darwin/arm64` (all cross-compiled on
`ubuntu-latest` — Go needs no per-target toolchain for these) and publishes
each as its own release asset:

- `le-fi-<tag>-linux-amd64.zip`
- `le-fi-<tag>-linux-arm64.zip`
- `le-fi-<tag>-darwin-amd64.zip`
- `le-fi-<tag>-darwin-arm64.zip`

Each zip contains the binary plus `USER_GUIDE.txt`.

The build step downloads Go 1.26 directly (cloudflared's go.mod requires
it) and caches `~/go/pkg/mod` / `~/.cache/go-build` per platform via
`actions/cache@v5` keyed on `go.sum`, since cloudflared pulls in a sizeable
transitive dependency tree (quic-go, grpc, OpenTelemetry, Sentry, etc.)
that's slow to re-fetch from scratch on every run.

The darwin builds are unsigned and unnotarized — see the macOS section
above.

Windows isn't built yet — the stderr-silencing trick (see below) currently
relies on `golang.org/x/sys/unix`, which needs a Unix-style equivalent
worked out for Windows before that target can be added.

## Telemetry

None, by design.

## Notes / limitations

- One transfer per run, then it exits — except send-servers (`le-fi <
  file`, `le-fi file`, `le-fi dir/`), which stay up and wait for a retry
  if an attempt drops mid-transfer, only closing after one actually
  succeeds. Run it again for another transfer either way (new random
  channel name each time).
- No auth on the endpoint while the tunnel is up — anyone who learns the
  URL before the transfer finishes could push data to it. Fine for a
  quick ad-hoc handoff between two parties who already know the address,
  not something to leave running unattended.
- The freshly-printed `trycloudflare.com` URL can take a few seconds to
  become reachable on Cloudflare's edge. If the very first send attempt
  gets a 530/1033 error, just retry — it clears up quickly.
- IPv4 only, Unix-only (uses `golang.org/x/sys/unix.Dup2` to silence
  cloudflared's hardcoded-on console logging while keeping the tool's own
  status output clean — no Windows equivalent wired up yet). Note this had
  to be `x/sys/unix` rather than the standard `syscall` package: the plain
  `syscall.Dup2` doesn't exist on `linux/arm64` — that syscall table only
  has `dup3`, not `dup2` — which broke the arm64 cross-build until switched
  over; `x/sys/unix` abstracts over that difference.
