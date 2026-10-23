# apple-container-compose

Docker Compose for [Apple container](https://github.com/apple/container).

`container-compose` reads standard `compose.yaml` / `docker-compose.yml` files and runs them on
Apple's lightweight-VM container runtime, using the same CLI surface as `docker compose`.

```console
$ container-compose up -d
 ✔ Network myapp_default  Created
 ✔ Volume myapp_dbdata  Created
 ✔ Container myapp-db-1  Healthy
 ✔ Container myapp-web-1  Started
$ container-compose ps
NAME          IMAGE                                  COMMAND                  SERVICE   CREATED          STATUS          PORTS
myapp-db-1    docker.io/library/postgres:16-alpine   "docker-entrypoint.s…"   db        10 seconds ago   Up 9 seconds
myapp-web-1   docker.io/library/nginx:alpine         "/docker-entrypoint.…"   web       10 seconds ago   Up 2 seconds    0.0.0.0:8080->80/tcp
```

## Install

Requires an Apple silicon Mac running macOS 26+, with Apple container 1.x.

```sh
# Homebrew: also installs Apple container (the `container` formula)
brew install pedrospdc/tap/container-compose

# or the release binary, with a checksum check
curl -fsSL https://raw.githubusercontent.com/pedrospdc/apple-container-compose/main/install.sh | sh

# or from source (Go 1.24+)
make build && make install

# or with Nix
nix profile install github:pedrospdc/apple-container-compose
```

Then start the runtime once (and after each reboot): `container system start`.

**`docker-compose` alias.** The installer and `make install` offer to create a `docker-compose`
command when none exists, so Makefiles and scripts that call `docker-compose` keep working.
Homebrew can't ask during install, so run it yourself:

```sh
container-compose alias          # asks first; does nothing if docker-compose already exists
container-compose alias --remove
```

It's a symlink rather than a shell alias because `make` and scripts run `/bin/sh`, which doesn't
see interactive aliases. `make install-plugin` additionally enables `container compose …`.

AI agents: see [AGENTS.md](AGENTS.md) for non-interactive usage and repository conventions.

## Commands

Every `docker compose` command is available, with the same flags. Global flags (`-f`, `-p`,
`--profile`, `--env-file`, `--project-directory`, `--dry-run`, ...) go before the command, as in
Docker Compose.

| Command | Status |
|---|---|
| `up`, `down`, `create`, `start`, `stop`, `restart`, `rm`, `kill`, `scale` | ✅ |
| `run`, `exec`, `cp`, `logs`, `ps`, `ls`, `top`, `port`, `images`, `volumes`, `stats` | ✅ |
| `build`, `pull`, `push`, `config` (`convert`), `version`, `wait`, `watch`, `commit`, `export` | ✅ |
| `events` | ✅ polled (the runtime has no event stream) |
| `attach` | ⚠️ streams output; stdin can't be attached to a running container |
| `pause`, `unpause` | ❌ the runtime has no freezer support |
| `publish` | ❌ the runtime can't push OCI artifacts |

The compose file is parsed by [compose-go](https://github.com/compose-spec/compose-go), the same
loader Docker Compose uses. Interpolation, `.env`, `env_file`, `extends`, `include`, profiles,
multiple `-f` overrides, `COMPOSE_*` variables, and validation all behave identically.

## How it maps to Apple container

| Compose | Apple container |
|---|---|
| service replicas | `container create/start`, named `<project>-<service>-<n>`, tracked with the `com.docker.compose.*` labels |
| networks | `container network create <project>_<name>` (`internal`, `ipam.subnet` supported) |
| named volumes | `container volume create <project>_<name>` (`driver_opts.size` → volume size) |
| bind mounts, secrets, configs | `--volume` bind mounts (files and directories) |
| `ports`, `environment`, `user`, `working_dir`, `entrypoint`, `command`, `cap_add/drop`, `privileged`, `read_only`, `init`, `ulimits`, `shm_size`, `tmpfs`, `dns*`, `platform`, `cpus`/`mem_limit`/`deploy.resources.limits`, `mac_address` | native flags |
| `lscr.io/*` images | Apple's registry client rejects lscr.io's cross-domain auth, so they're pulled from their ghcr.io mirror and tagged with the requested name |
| `depends_on` (`service_started`, `service_healthy`, `service_completed_successfully`, `required`, `restart`) | handled by compose |
| `healthcheck` (and image `HEALTHCHECK`) | probes run by compose via `container exec` |
| `sysctls` | kernel command line (`--kernel-arg sysctl.*`): each container has its own kernel. Docker's defaults (`ip_unprivileged_port_start=0`, `ping_group_range`) are applied too |
| `restart` / `deploy.restart_policy` | supervised by `up` when attached; by a background `container-compose` supervisor (one per project, exits on `down`) when detached |
| `post_start` / `pre_stop` hooks, `develop.watch` | `container exec` / `container copy` |
| `build` | `container build` (args, target, secrets, ssh, labels, inline Dockerfiles) |

### Service discovery

Apple container only resolves container names for DNS domains registered with
`sudo container system dns create`, and it rejects single-label aliases. So every project gets a
small CoreDNS sidecar (`<project>-compose-dns`, 256 MB) attached to all project networks. It
answers service names, container names, `aliases`, `links`, `extra_hosts` and
`host.docker.internal` per network, and forwards everything else to the network gateway. Records
are updated as containers start and stop. Set `COMPOSE_DNS=0` to disable it.

### Differences from Docker

* **Memory**: containers are VMs; without `mem_limit` they get the runtime default (1 GB). Apple
  container's minimum is 200 MiB, so smaller limits are rounded up.
* **Volumes are disk images**: an Apple volume is an ext4 image attached to one VM at a time.
  Compose therefore uses a real volume when a single container mounts it, and a host directory
  (`~/Library/Application Support/apple-container-compose/volumes/<name>`, removed by
  `down -v`) when several containers share it. Fresh volumes get their `lost+found` removed and
  are seeded with the image's content at the mount point (Docker's copy-up), so `postgres`,
  `mysql` & co. initialise normally.
* **Bind mounts** go through virtiofs. Files work, but `chown` of the mount root fails with
  `EPERM` and ownership changes below it aren't persisted, which breaks entrypoints that
  `chown -R` a bind-mounted data directory. Prefer named volumes for database data.
* **Host paths that can't exist on macOS** (e.g. `/mnt/docker-volumes/app`, under the
  root-owned `/mnt`) are backed by a named volume `<project>_host_<dir>_<hash>` (or a shared
  directory under `host-paths/` when several containers mount them). Like host data, these
  survive `down -v`. Linux-only paths (`/proc`, `/sys`, `/dev`, `/var/run/docker.sock`,
  `/etc/timezone`, …) are skipped with a warning.
* **amd64-only images** run under Rosetta automatically.
* **Restart after reboot**: there's no daemon, so `restart: always` containers don't come back
  after a reboot or `container system stop` until you run `up -d` again.
* **Published ports** are forwarded by Apple container itself. On macOS 26+ its helper
  (`container-runtime-linux`) needs the *Local Network* privacy permission; without it,
  connections to `localhost:<port>` are reset (`container system logs` shows
  `connect failed: No route to host`), while the container IP stays reachable.
* **Exit codes** are only known for containers compose started attached (the runtime doesn't
  record them), so `ps` shows `Exited` without a code for others.
* **Not supported** (ignored with a warning): `network_mode: host|service:|container:`
  (the service joins the regular network instead), static IPs, `devices`, `security_opt`,
  `pid`, `ipc`, `gpus`, `cgroup*`, `logs --since/--until`.

## Environment variables

| Variable | Purpose |
|---|---|
| `COMPOSE_FILE`, `COMPOSE_PROJECT_NAME`, `COMPOSE_PROFILES`, `COMPOSE_REMOVE_ORPHANS`, `COMPOSE_IGNORE_ORPHANS` | as in Docker Compose |
| `COMPOSE_DNS=0` | disable the DNS sidecar |
| `COMPOSE_DNS_IMAGE` | CoreDNS image (default `docker.io/coredns/coredns:1.12.1`) |
| `COMPOSE_HELPER_IMAGE` | image used to prepare volumes for shell-less images (default `alpine:3.20`) |
| `COMPOSE_STATE_DIR` | state directory (DNS records, materialised secrets) |
| `COMPOSE_NO_SUPERVISOR=1` | don't spawn the detached restart supervisor |
| `CONTAINER_BIN` | path to the `container` binary |
| `COMPOSE_DEBUG=1` / `--debug` | print every `container` command executed |

## Compatibility testing

Tested against [Haxxnet/Compose-Examples](https://github.com/Haxxnet/Compose-Examples):

* **Parsing**: all 167 compose files pass `config`. In `--dry-run up`, 157 convert cleanly; the other
  10 declare `external` networks/volumes that must be created first (as with Docker).
* **Live**: 39 stacks were brought up with `up -d --wait`, checked after 25 s, then torn down
  with `down -v`. All containers stayed running in 31 of them, including wordpress, ghost,
  nextcloud, paperless-ngx, mattermost, linkwarden, plausible (postgres + clickhouse), excalidraw,
  vaultwarden, bookstack, sonarr/jackett (`lscr.io`) and transfer.zip (built from source).
  The other 8 failed as follows:
  * 4 images are gone or broken upstream (memos, minio, rocketchat, snipe-it).
  * 2 need user-provided files or a Docker host (n8n's init script, telegraf's
    `/var/run/docker.sock` user).
  * mealie: two services share a bind directory, and the entrypoint `chown`s its root (see *Bind
    mounts*).
  * privatebin: needs a writable `/run` on a `read_only` rootfs.

## Development

```sh
make test               # vet + unit tests
make release-snapshot   # local GoReleaser dry run
```

CI (`.github/workflows/ci.yml`) checks formatting, `go mod tidy`, vet, race tests, a CLI smoke test
and the GoReleaser config. To release, push a tag `vX.Y.Z`: `.github/workflows/release.yml`
publishes the GitHub release and updates the Homebrew cask. Details in [AGENTS.md](AGENTS.md).
