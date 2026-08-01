# le-fi

*(a "file" walks into the portal on the right; "le" teleports out the left — le-fi)*

Just want to send or receive a file? Grab a release and read
`USER_GUIDE.txt` instead — this doc is for building it and poking
around under the hood.

Send or receive a file (or a whole directory) across NATs using a
Cloudflare Quick Tunnel — **without the `cloudflared` binary installed
anywhere.** This links cloudflared's own Go packages directly into this
binary and drives them in-process (`cli.App.RunContext(...)`), instead of
shelling out to a separate executable. The receiving end also serves a
tiny browser drop-zone UI for people who don't have any tooling at all.

Verified against real Cloudflare infrastructure: real quick tunnels
provisioned, byte-perfect transfers confirmed via curl, wget, a browser,
and the tool's own client/server modes in both directions.

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
hard way while building this tool). The four lines are already in this
`go.mod` — if you ever bump the cloudflared version, check
[cloudflared's go.mod](https://github.com/cloudflare/cloudflared/blob/master/go.mod)
for changes to these first.

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

Each zip contains the binary plus `USER_GUIDE.txt` — no source tree.

No extra secrets are needed — it uses the Gitea-provided `GITEA_TOKEN`,
scoped to this repo by the workflow's `permissions: contents: write`. The
release itself is created once in a separate job that the four platform
builds then upload their assets to, so the parallel matrix legs don't race
each other trying to create the same release.

The build step downloads Go 1.26 directly (cloudflared's go.mod requires
it) and caches `~/go/pkg/mod` / `~/.cache/go-build` per platform via
`actions/cache@v5` keyed on `go.sum`, since cloudflared pulls in a sizeable
transitive dependency tree (quic-go, grpc, OpenTelemetry, Sentry, etc.)
that's slow to re-fetch from scratch on every run.

Windows isn't built yet — the stderr-silencing trick (see below) currently
relies on `golang.org/x/sys/unix`, which needs a Unix-style equivalent
worked out for Windows before that target can be added.

## Telemetry

None, by design. cloudflared does link in Sentry (crash reporting) and a
local metrics server as part of its own codebase, so this was verified
directly rather than assumed:

- **Sentry**: `sentry.Init()` runs with Cloudflare's real DSN (it's
  hardcoded in cloudflared, not something we can remove), but the only
  code paths that would actually call `sentry.CaptureException` are gated
  behind conditions this tool never triggers (FIPS mode, and file-based
  TLS cert reloading - neither applies to quick tunnels). Confirmed with
  an actual network trace (`strace -e trace=network`) during a real run:
  Sentry's real IP never appears anywhere in the connection log.
- **Metrics server**: cloudflared always starts a local Prometheus-style
  metrics endpoint. It's forced to `127.0.0.1` explicitly in the tunnel
  invocation here - cloudflared's own docs note it can default to binding
  all interfaces in some virtualized/containerized environments, so this
  isn't left to auto-detection.
- **Management diagnostics** (`/debug/pprof` etc., reachable only via a
  request routed *through* the tunnel by Cloudflare's dashboard, not
  something cloudflared calls out for) are explicitly disabled -
  `--management-diagnostics=false` - even though upstream defaults this
  to `true`.
- Every other outbound connection observed in the trace resolves to
  `api.trycloudflare.com` (quick-tunnel provisioning) or Cloudflare's own
  tunnel-edge discovery/connection infrastructure - both required for the
  tunnel to function at all.
- **Isolation from any existing cloudflared setup on this machine.**
  Since cloudflared's code runs in-process, sharing this same OS
  environment, it would otherwise auto-discover a real
  `~/.cloudflared/config.yml` or pick up any `TUNNEL_*` env var left over
  from someone's own separate cloudflared use - silently pulling in their
  account credentials, ingress rules, or settings that have nothing to do
  with an anonymous quick tunnel. `clearCloudflaredEnv()` (`tunnel.go`)
  unsets every environment variable cloudflared's own flag definitions
  recognize (extracted directly from its source, not guessed), and
  `--config ""` / `--origincert ""` are passed explicitly to override
  auto-discovery outright. Verified for real, not just by reading the
  code: ran the tool against a deliberately hostile fake
  `~/.cloudflared/config.yml` (bogus ingress rules, wrong URL, a metrics
  port on all interfaces) plus conflicting `TUNNEL_*` env vars, including
  one for the metrics port - it still came up as a normal, correct quick
  tunnel, and the hostile port had nothing listening on it.

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
