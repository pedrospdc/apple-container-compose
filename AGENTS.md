# AGENTS.md

Guidance for AI agents that **use** `container-compose` to run compose projects, and for agents
that **work on** this repository. Humans: see [README.md](README.md).

`container-compose` is Docker Compose for [Apple container](https://github.com/apple/container):
same compose files, same commands and flags as `docker compose`, running each container as a
lightweight VM on macOS.

---

## Part 1: Using container-compose

### Requirements

- Apple silicon Mac, macOS 26 or newer.
- Apple container 1.x, with its services running.

### Install

Pick one (all three are non-interactive except where noted):

```sh
# Homebrew (also installs Apple container as a dependency)
brew install pedrospdc/tap/container-compose

# Release binary. Prompts for the docker-compose alias on a TTY; set ALIAS=yes|no to skip the prompt.
curl -fsSL https://raw.githubusercontent.com/pedrospdc/apple-container-compose/main/install.sh | ALIAS=no sh

# From source (Go 1.24+)
make build   # -> ./bin/container-compose

# Nix
nix profile install github:pedrospdc/apple-container-compose
```

Then start the runtime once and verify:

```sh
container system start           # idempotent; required after reboot
container-compose version        # prints compose and runtime versions
```

Optional:

- `container-compose alias --yes` creates a `docker-compose` symlink for Makefiles and scripts. It
  does nothing if a `docker-compose` already exists; add `--force` to replace it, `--remove` to undo.
- `make install-plugin` (from a checkout) enables `container compose …`.

### Rules for non-interactive use

Agents usually have no TTY and must not block. Follow these:

| Do | Don't |
|---|---|
| `container-compose up -d --wait` (returns once services are running/healthy) | `container-compose up` without `-d` (streams logs until Ctrl-C) |
| `container-compose exec -T <svc> <cmd>` | `exec` without `-T` when there's no TTY |
| `container-compose run --rm -T <svc> <cmd>` | interactive `run` |
| `container-compose logs --tail 100 <svc>` | `logs -f` (never returns) |
| `container-compose ps --format json` / `ls --format json` | parse the table output |
| `container-compose config --quiet` to validate a file | assume a file is valid |
| `container-compose --dry-run --debug up -d` to preview the `container` commands | guess what it will do |
| `container-compose down -v` when finished | leave stacks running |

Global flags (`-f`, `-p`, `--profile`, `--env-file`, `--project-directory`, `--dry-run`,
`--debug`, `--progress quiet`) go **before** the command, as in Docker Compose:
`container-compose -f stack.yml -p demo up -d`.

Exit codes follow Docker Compose. `run`, `exec`, `up --exit-code-from`, and `wait` return the
container's exit code.

### What differs from Docker (read this before debugging)

- **Reaching published ports.** Apple container forwards `-p` ports itself. If
  `curl localhost:<port>` gets "connection reset", the runtime's helper lacks macOS *Local Network*
  permission; `container system logs` shows `connect failed: No route to host`. The container is still
  reachable on its own IP:
  `container inspect <name> | jq -r '.[0].status.networks[0].ipv4Address'` (strip the `/24`).
- **Service names resolve through a DNS sidecar.** Each project runs `<project>-compose-dns`
  (CoreDNS), which `container ls` shows. It is managed by compose; don't remove it by hand.
  Disable it with `COMPOSE_DNS=0`.
- **Volumes are disk images attached to one container at a time.** A volume used by several
  services is stored as a host directory instead, under
  `~/Library/Application Support/apple-container-compose/volumes/`.
- **Bind mounts can't change ownership of their root.** An entrypoint that runs `chown -R` on a
  bind-mounted data dir fails with `Operation not permitted`. Use a named volume instead.
- **Uncreatable Linux host paths** (e.g. `/mnt/docker-volumes/app`) become volumes named
  `<project>_host_*`, which **survive `down -v`** like host data would. Delete them explicitly:
  `container volume ls` → `container volume rm <name>`. Linux system paths (`/proc`, `/sys`,
  `/var/run/docker.sock`, `/etc/timezone`, …) are skipped with a warning.
- **Memory.** Each container is a VM with 1 GB by default (`mem_limit` / `deploy.resources.limits.memory`
  to change, minimum 200 MiB). JVM or Elasticsearch-style services often need more.
- **Restart policies** with `up -d` are applied by a background `container-compose __supervise`
  process per project. It exits on `down`. It does not survive a reboot; rerun `up -d`.
  `COMPOSE_NO_SUPERVISOR=1` disables it.
- **Unsupported** (warned, then ignored): `network_mode: host|service:|container:`, static IPs,
  `devices`, `security_opt`, `pid`, `ipc`, `gpus`, `logs --since/--until`, `pause`/`unpause`, `publish`.
- `ps` only knows exit codes for containers that compose itself started attached.

### Troubleshooting

| Symptom | Cause / fix |
|---|---|
| `apiserver is not running` / `XPC connection error` | `container system start` |
| `minimum memory amount allowed is 200 MiB` | raise `mem_limit` |
| `The storage device attachment is invalid` | a volume is mounted by two containers; usually a one-off `run` while the service is up. Stop the service or use a bind mount |
| `refusing insecure credential exchange` (lscr.io) | handled automatically via the ghcr.io mirror; for other registries, `container registry login` |
| `getaddrinfo ENOTFOUND <service>` | the target service isn't running (`ps -a`, `logs <svc>`); names resolve only while it runs |
| `bind source path does not exist` | `bind.create_host_path: false` and the path is missing |
| `network … declared as external, but could not be found` | `container network create <name>` first |
| Image pull 404/401 | image or tag no longer exists upstream; fix the compose file |

---

## Part 2: Working on this repository

### Layout

```
cmd/container-compose/   main; the binary name decides the usage line (docker-compose / container compose)
internal/ctr/            the ONLY place that runs the `container` CLI; JSON types for its output
internal/compose/        the engine
  project.go             loading via compose-go (same loader as Docker Compose)
  names.go               labels (com.docker.compose.*) and naming rules
  convert.go             service -> `container create` flags; mounts, ports, sysctls, secrets
  engine.go              networks, volumes, images, convergence, start/stop/down/rm/kill/restart
  attach.go              attached starts (exit codes), restart policies, depends_on waits
  health.go              healthchecks, run by compose through `container exec`
  dns.go                 per-project CoreDNS sidecar and hosts files
  supervise.go           detached restart supervisor
  up.go                  up (attached/detached), run, exec
  info.go                ps, ls, images, top, port, events, stats, volumes, wait, cp, pull, push
  logs.go, build.go, watch.go
internal/cli/            cobra commands mirroring `docker compose` (flags must match Docker's)
testdata/                sample compose file + fake `container` for CI
```

### Commands

```sh
make build              # bin/container-compose (version from git describe)
make test               # go vet + unit tests
gofmt -l .              # must print nothing (CI enforces)
make release-snapshot   # full GoReleaser dry run into dist/
```

Unit tests never touch the real runtime: `testEngine()` uses dry-run mode with a fake binary.
Set `COMPOSE_STATE_DIR=$(mktemp -d)` when running tests so they don't write into your real state dir.

### Invariants: don't break these

- State lives in **container labels**, not a database: `com.docker.compose.project/service/
  container-number/oneoff/config-hash` plus `dev.apple-container-compose.*`. Recreation is decided by
  `config-hash` (`ServiceHash`); fields that don't affect the container are excluded from it.
- Shell out only through `ctr.Client`. Mutating calls go through `Exec`/`Stream` so `--dry-run`
  skips them; read-only calls use `Output` and run even in dry-run.
- DNS hosts files must be **written in place** (`os.WriteFile`), never by rename: virtiofs doesn't
  propagate renames into running containers. Wait for CoreDNS's 1s reload (`dnsReloadDelay`) before
  starting a dependent.
- Directory bind mounts see host writes; single-file bind mounts don't. That's why the DNS sidecar
  mounts a directory.
- Exit codes exist only from `container start --attach`. Anything needing them (completion
  dependencies, `--exit-code-from`, restart on-failure) must start the container attached.
- Every user-visible divergence from Docker needs a warning (`e.warnf`, deduplicated) and a
  README entry.

### Verifying behavior against real stacks

Unit tests cover conversion. Behavior has to be checked against the real runtime. The reference
corpus is [Haxxnet/Compose-Examples](https://github.com/Haxxnet/Compose-Examples):

```sh
git clone --depth 1 https://github.com/Haxxnet/Compose-Examples /tmp/ce
cd /tmp/ce/examples/<name> && container-compose up -d --wait --wait-timeout 420
container-compose ps -a        # every service should be running
container-compose logs --tail 50
container-compose down -v
```

Probe HTTP on the container IP (see Part 1) rather than `localhost`. When a stack fails, decide
first whether Docker would fail too (missing user files, dead upstream images, `external`
resources) before changing code. README's "Compatibility testing" section records the last
results; update it when they change.

### Releasing

1. Make sure `main` is green in CI.
2. `git tag vX.Y.Z && git push origin vX.Y.Z`.
3. `.github/workflows/release.yml` runs GoReleaser. It builds `darwin/arm64`, publishes the GitHub
   release with checksums, and pushes `Casks/container-compose.rb` to `pedrospdc/homebrew-tap`
   using the `HOMEBREW_TAP_DEPLOY_KEY` secret (a write deploy key scoped to the tap only).
4. Verify: `brew update && brew upgrade --cask container-compose` (or `brew install pedrospdc/tap/container-compose`)
   and run `container-compose version`.

Commit messages prefixed `docs:`, `test:`, `ci:` or `chore:` are left out of the release changelog.
