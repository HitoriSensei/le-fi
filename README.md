# le-fi

*(a "file" walks into the portal on the right; "le" teleports out the left — le-fi)*

Send/receive a piped file across NATs using a Cloudflare Quick Tunnel —
**without the `cloudflared` binary installed anywhere.** This links
cloudflared's own Go packages directly into this binary and drives them
in-process (`cli.App.RunContext(...)`), instead of shelling out to a
separate executable.

Verified against real Cloudflare infrastructure: real quick tunnels
provisioned, byte-perfect transfers confirmed via curl, wget, and the
tool's own sender mode.

## Build

```bash
go build -o le-fi .
```

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

Run `./le-fi -h` (or `--help`) any time for this same usage info from
the binary itself. Running it bare with no arguments and nothing piped in
either direction (i.e. just `./le-fi` typed directly into a terminal)
also shows this help instead of silently opening a real tunnel.

**Receiving end always starts the process:**

```bash
./le-fi > out.bin
```

This prints something like:

```
Tunnel is up (no account, no port forwarding - works through any NAT).

If the remote party does NOT have this utility, they can send with:

  curl -T - https://idol-car-indication-behavior.trycloudflare.com/ < file_to_be_send.bin
  wget --method=PUT --body-file=file_to_be_send.bin https://idol-car-indication-behavior.trycloudflare.com/ -O -

If the remote party also has this utility, they can send with:

  le-fi idol-car-indication-behavior < file_to_be_send.bin

Channel name:

  idol-car-indication-behavior

Waiting for the transfer... (Ctrl+C to cancel)
```

**Sending end**, three options depending on what they have:

- No utility, has curl → paste the curl line.
- No utility, has wget → paste the wget line.
- Also has this utility → `./le-fi <channel-name> < file_to_send.bin`
  (this path needs no cloudflared library at all — it's a plain HTTPS PUT
  to `https://<channel-name>.trycloudflare.com/`).

The tool exits automatically once the file is fully received, and tears
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

`.gitea/workflows/test.yml` runs `go vet` and `go test -race` on every
push to `main`.

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

Each zip contains **just the binary** — no README, no source tree.

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

## Notes / limitations

- Single-shot: one file per run, then it exits. Run it again for another
  transfer (you'll get a new random channel name each time).
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
