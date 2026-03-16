# Skylens

Skylens is a distributed drone-detection platform built around lightweight field sensors (**TAPs**) and a central **Node**. TAPs collect and decode wireless telemetry, publish detections over NATS, and the Node correlates, stores, classifies, and presents that data through a browser dashboard.

This repository currently reflects the **Azure/RHEL 9 Node deployment path** implemented by `scripts/install.sh`.

---

## What this version does

At a high level, the current stack works like this:

1. **TAP sensors** capture Wi-Fi / Remote ID / DJI-related traffic in the field.
2. TAPs publish detections to **NATS** using the protobuf schema in `proto/skylens.proto`.
3. The **Skylens Node** (`cmd/skylens-node`) consumes those detections.
4. The Node enriches and correlates detections using the logic in `internal/intel`, `internal/processor`, `internal/receiver`, and `internal/storage`.
5. State is persisted in **PostgreSQL**, hot data / cache flows through **Redis**, and live updates are pushed to the UI over **WebSocket**.
6. The browser dashboard is served by the Go API server and exposed publicly through **nginx with HTTPS**.

---

## Current deployment model

The primary installation path for this repo is:

- **Host OS:** RHEL 9 / Rocky 9 / compatible EL9 image in Azure
- **Installer:** `scripts/install.sh`
- **Reverse proxy:** nginx
- **TLS:** self-signed certificate generated during install
- **Database:** PostgreSQL 16
- **Cache / session support:** Redis
- **Message bus:** NATS
- **Application binary:** `skylens-node`
- **Service manager:** systemd

This is **not** a Docker-first deployment in its current form. The active install flow is a native host install.

---

## What `scripts/install.sh` actually sets up

The installer is opinionated and assumes a known layout on the target host.

### Expected source location

The script expects the repo to already exist at:

```bash
/home/tdcadmin/skylens
```

It also expects the service account `tdcadmin` to already exist.

### What the installer does

When run as root, `scripts/install.sh`:

- installs required packages for nginx, Redis, PostgreSQL, firewall management, and build tooling
- installs or enables **PostgreSQL 16**
- installs **NATS Server**
- verifies that a **Go compiler is already present**
- syncs the repo into `/opt/skylens`
- builds `cmd/skylens-node` and installs it to:

```bash
/usr/local/bin/skylens-node
```

- creates a runtime config at:

```bash
/etc/skylens/config.yaml
```

- creates application directories under:

```bash
/opt/skylens
/etc/skylens
/var/lib/skylens
/var/log/skylens
```

- initializes PostgreSQL and creates the `skylens` database/user
- enables and starts:
  - `postgresql-16`
  - `redis`
  - `nats-server`
  - `nginx`
  - `skylens-node`
- generates a self-signed TLS certificate under:

```bash
/etc/skylens/certs/
```

- creates nginx proxy rules for:
  - `https://<host>/` → Go HTTP server on `127.0.0.1:8080`
  - `https://<host>/ws` → Go WebSocket server on `127.0.0.1:8081/ws`
- opens firewall access for HTTP, HTTPS, and NATS
- patches the deployed dashboard so WebSockets work cleanly behind nginx/TLS
- removes Tailscale UI elements from the deployed dashboard

---

## Runtime architecture

```text
TAPs / sensors
   │
   └── protobuf detections over NATS
                │
                ▼
        skylens-node (Go)
        ├── API server
        ├── WebSocket broadcaster
        ├── detection/state engine
        ├── spoof / trust logic
        ├── TAK integration hooks
        └── persistence layer
             ├── PostgreSQL
             └── Redis
                │
                ▼
            nginx (HTTPS)
                │
                ▼
           Browser dashboard
```

---

## Main components in this repo

### Node application

- `cmd/skylens-node/` — main Go entrypoint for the central node
- `internal/api/` — HTTP API, dashboard serving, WebSocket handling
- `internal/auth/` — login, RBAC, JWT/cookie auth, admin endpoints
- `internal/intel/` — model identification, fingerprinting, decoding helpers
- `internal/processor/` — correlation, spoof detection, trust/state logic
- `internal/receiver/` — NATS ingestion path
- `internal/storage/` — PostgreSQL-backed persistence and auth schema setup
- `internal/tak/` — TAK output support
- `proto/` — protobuf contract used between TAPs and Node

### TAP side

- `tap/` — Rust TAP sensor implementation and TAP deployment assets

### Ops / install

- `scripts/install.sh` — primary Azure/EL9 installer
- `scripts/uninstall.sh` — removes the installed Node stack from a host
- `scripts/preflight-check.sh` — post-install validation helper
- `scripts/live-monitor.sh` — operational monitoring helper

---

## Prerequisites for the current install path

Before running the installer, make sure the target VM has:

- EL9-based OS (RHEL 9 / Rocky 9 / AlmaLinux 9)
- a pre-created user named `tdcadmin`
- this repository checked out at `/home/tdcadmin/skylens`
- `go` already installed and on disk (`/usr/local/go/bin/go`, `/usr/bin/go`, or `/bin/go`)
- outbound internet access for package installation

---

## Azure install flow

### 1. Clone the repo onto the VM

```bash
git clone <your-repo-url> /home/tdcadmin/skylens
cd /home/tdcadmin/skylens
```

### 2. Run the installer as root

```bash
sudo bash scripts/install.sh
```

### 3. Validate services

```bash
sudo systemctl status postgresql-16
sudo systemctl status redis
sudo systemctl status nats-server
sudo systemctl status nginx
sudo systemctl status skylens-node
```

### 4. Open the dashboard

Browse to:

```text
https://<vm-ip-or-dns-name>/
```

Because the default certificate is self-signed, the browser will warn until you replace it with a trusted certificate.

---

## Default auth behavior

Authentication is enabled by default in the shipped config, and the auth schema seeds a default admin account on first startup:

- **username:** `admin`
- **password:** `admin`

Change that immediately after first login.

---

## Configuration

The installed runtime config lives at:

```bash
/etc/skylens/config.yaml
```

The template in the repo is:

```bash
configs/config.example.yaml
```

Key sections include:

- `server` — HTTP and WebSocket listener ports
- `nats` — NATS connection string
- `database` — PostgreSQL connection settings
- `redis` — Redis connection string
- `detection` — state retention, spoof checks, single-TAP behavior
- `propagation` — RF environment model
- `auth` — JWT secret and auth enablement

---

## Ports used by the current deployment

### Public

- `80/tcp` — HTTP redirect to HTTPS
- `443/tcp` — HTTPS dashboard and API

### Local / backend

- `8080/tcp` — internal Go HTTP server
- `8081/tcp` — internal Go WebSocket server
- `5432/tcp` — PostgreSQL
- `6379/tcp` — Redis
- `4222/tcp` — NATS client port
- `8222/tcp` — NATS monitoring port

In the current install path, nginx fronts the application and the user should access the dashboard over **443**.

---

## Files and directories created on the host

```text
/opt/skylens                 deployed application tree
/etc/skylens/config.yaml     runtime config
/etc/skylens/certs/          TLS certificate and key
/var/lib/skylens/            runtime data / lock files
/var/log/skylens/            install logs
/usr/local/bin/skylens-node  compiled node binary
```

---

## Relevant repository layout

```text
.
├── cmd/
│   ├── skylens-node/        main Node application
│   ├── flight-sim/          optional simulator utility
│   └── intel-updater/       optional intel/OUI update utility
├── configs/
│   └── config.example.yaml
├── internal/
│   ├── api/
│   ├── auth/
│   ├── intel/
│   ├── processor/
│   ├── receiver/
│   ├── storage/
│   └── tak/
├── proto/
│   ├── skylens.proto
│   └── skylens.pb.go
├── scripts/
│   ├── install.sh
│   ├── uninstall.sh
│   ├── preflight-check.sh
│   └── live-monitor.sh
├── tap/
│   └── ... TAP sensor code and docs ...
└── test/
    └── ... test harnesses and validation assets ...
```

---

## What is not the primary path anymore

Based on the current repo state, the main documented path should no longer center on:

- Docker deployment
- Tailscale-managed dashboard access
- the older `scripts/install-node.sh` workflow
- manual service-file copy steps using the static `skylens-node.service`

Those can still exist as legacy/dev assets, but they are not the install path this repo is actively using for Azure.

---

## Cleanup recommendations

### Safe to remove now if you want the repo to match the current Azure install path

These are the easiest wins because `scripts/install.sh` does not rely on them:

- `scripts/install.sh.bak.1773182080`
  - backup artifact, should not live in the main branch
- `scripts/install-node.sh`
  - older installer superseded by `scripts/install.sh`
- `Dockerfile`
  - removable if you are no longer supporting container builds
- `docker/`
  - removable if you are no longer supporting Docker deployment
- `nats-server.conf`
  - current installer generates `/etc/nats-server.conf` on the host
- `skylens-node.service`
  - current installer generates the systemd unit dynamically

### Removable if you are intentionally trimming non-runtime tooling

Only remove these if you are okay losing optional dev/maintenance workflows:

- `cmd/flight-sim/`
  - optional simulator, not required for production Node install
- `cmd/intel-updater/`
  - optional data-maintenance utility, not required for production Node install
- `test/`
  - not needed to run production, but useful to keep for validation
- `docs/TEST_DAY_CHECKLIST.md`
  - operational documentation only

### Keep

These are core to how the current version works:

- `cmd/skylens-node/`
- `configs/`
- `internal/`
- `proto/`
- `scripts/install.sh`
- `scripts/uninstall.sh`
- `scripts/preflight-check.sh`
- `scripts/live-monitor.sh`
- `tap/` if you are still using Skylens TAP sensors
- `docs/whitepaper.md` if you still want concept/briefing material in the repo

---

## Suggested next cleanup pass

If you remove the legacy files above, also update:

- `Makefile` targets that still reference `flight-sim`, `intel-updater`, or `skylens-node.service`
- any docs that still reference Docker, Tailscale, or `install-node.sh`
- `README.md` links to files you decide to delete

---

## Uninstall

To remove the installed Node stack from a host:

```bash
sudo bash scripts/uninstall.sh
```

That script stops services, removes generated configs and binaries, removes PostgreSQL data, and clears the associated firewall rules.

---

## Status

This README is intended to describe the **current Azure-native Node deployment path** represented by `scripts/install.sh`, not every historical or experimental deployment option that still exists in the repo.
