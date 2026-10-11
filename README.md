# Compute Provisioning Service (CPS)

CPS is a single-host Linux virtual-machine provisioning service written in Go. It creates KVM-accelerated QEMU virtual machines from pre-installed QCOW2 cloud images, injects first-boot configuration with `cloud-init`, persists inventory and lifecycle events in SQLite, and exposes an HTTP API for provisioning and managing guests.

> **Project scope:** CPS is a learning project and a single-host control plane, not an AWS-equivalent production cloud. It currently has no API authentication, multi-host scheduler, highly available controller, or production hardening. By default, the HTTP API listens on `127.0.0.1:8080`. Do not expose it directly to an untrusted network.

## Table of contents

- [Overview](#overview)
- [Architecture concepts](#architecture-concepts)
  - [KVM, QEMU, QMP, and Libvirt](#kvm-qemu-qmp-and-libvirt)
  - [How a VM is provisioned](#how-a-vm-is-provisioned)
  - [Cloud images, QCOW2, and copy-on-write](#cloud-images-qcow2-and-copy-on-write)
  - [Cloud-init and SSH identity](#cloud-init-and-ssh-identity)
  - [Why CPS uses `exec.Command`](#why-cps-uses-execcommand)
- [Project structure](#project-structure)
- [Requirements and installation](#requirements-and-installation)
- [Configure base images](#configure-base-images)
- [Run the service](#run-the-service)
- [API reference](#api-reference)
- [Provision a VM and poll its status](#provision-a-vm-and-poll-its-status)
- [Connect to a VM](#connect-to-a-vm)
- [Lifecycle actions and states](#lifecycle-actions-and-states)
- [Change VM configuration](#change-vm-configuration)
- [Inspect host resources, inventory, logs, and events](#inspect-host-resources-inventory-logs-and-events)
- [Decommission a VM](#decommission-a-vm)
- [Persistence, reconciliation, and recovery](#persistence-reconciliation-and-recovery)
- [Testing and validation](#testing-and-validation)
- [Current limitations](#current-limitations)
- [Future scope](#future-scope)
- [Technical Q&A](#technical-qa)
  - [Cloud image and boot artifact formats](#1-what-do-the-different-cloud-image-and-boot-artifact-formats-mean)
  - [KVM vs. QEMU](#2-what-is-the-difference-between-kvm-and-qemu-does-cps-use-both)
  - [Why `exec.Command` is used](#3-why-does-cps-use-execcommand-instead-of-controlling-everything-through-qmp)
  - [SSH public-key authentication](#4-how-does-ssh-public-key-authentication-work-and-why-must-the-private-key-stay-local)
  - [Default cloud-init username](#5-how-does-cloud-init-choose-the-default-vm-username)
  - [VM placement and access](#6-where-is-my-vm-running-and-how-do-i-connect-to-it)
  - [Multiple VMs and SSH ports](#7-what-happens-if-i-create-several-vms-will-they-all-use-localhost2222)
  - [Persistence across API restarts](#8-will-my-vms-remain-in-the-inventory-after-cps-restarts)
  - [Pause vs. hibernation](#9-are-pause-and-hibernation-the-same-thing)
  - [Region and availability-zone labels](#10-what-do-region-and-availability-zone-fields-mean-in-cps-today)
  - [How public clouds manage physical hosts](#11-how-do-large-public-clouds-manage-so-many-physical-hosts)
  - [VMware operations vs. cloud systems engineering](#12-why-do-some-infrastructure-jobs-focus-on-vmware-while-cloud-providers-hire-systems-engineers)
  - [Linux, macOS, and Windows compatibility](#13-can-cps-run-on-linux-macos-and-windows)

---

## Overview

CPS provides the following capabilities in its current single-host implementation:

| Capability | What it does |
|---|---|
| VM provisioning | Creates a per-instance QCOW2 overlay, builds a cloud-init seed ISO, and launches QEMU. |
| KVM acceleration | Uses Linux `/dev/kvm` and the host CPU's virtualization extensions for accelerated guest CPU execution. |
| Persistent inventory | Stores instance records, lifecycle states, and audit events in `cps.db`. |
| Request logging | Logs HTTP method, path, status, duration, response size, remote address, and request ID to stdout and `cps.log`. Request bodies and query strings are intentionally not logged. |
| VM diagnostics | Keeps per-instance QEMU, serial-console, and QEMU diagnostic logs where available. |
| Runtime control | Uses QEMU Monitor Protocol (QMP) for supported VM inspection and lifecycle operations. |
| SSH access | Injects a **public** SSH key through cloud-init. The client's private key stays on the client machine. |
| Host and image metadata | Exposes available host capacity, discovered QCOW2 images, aliases, state counts, and inventory through HTTP endpoints. |
| Read-only inventory | Exposes instance records and persisted events without requiring direct SQLite access. |
| Lifecycle operations | Supports start, graceful stop, force stop, pause, resume, and hibernate, subject to QEMU/image support. |
| Offline updates | Allows supported CPU/RAM updates and virtual disk growth while the VM is stopped. |
| Decommissioning | Removes local VM files while preserving a decommissioned inventory record and event history. |

## Architecture concepts

### KVM, QEMU, QMP, and Libvirt

These components do different jobs and are not interchangeable alternatives.

| Component | Responsibility | Mental model |
|---|---|---|
| **KVM** | Linux kernel virtualization support. It uses CPU extensions such as Intel VT-x or AMD-V to accelerate eligible guest CPU instructions. CPS needs access to `/dev/kvm`. | Accelerates guest CPU execution. |
| **QEMU** | A userspace process that creates and runs the virtual machine, allocates guest memory, provides virtual hardware devices, and connects virtual disks and networking to host resources. | Builds and runs the virtual computer. |
| **QMP** | QEMU Monitor Protocol, a local JSON-based control interface exposed by a running QEMU process. CPS uses it for supported runtime queries and actions. | The management socket to a running QEMU VM. |
| **Libvirt** | An optional higher-level management API and service that can manage QEMU/KVM and other virtualization backends. | A VM management layer above QEMU/KVM. |
| **CPS API** | The Go HTTP service that validates client requests, records desired/current state in SQLite, and asks the compute layer to carry out operations. | The project's control plane. |
| **SQLite** | Stores durable instance inventory, lifecycle state, and audit events on the local host. | The control plane's local record book. |

A typical launch looks like this:

```text
Client
  │ HTTP request
  ▼
CPS HTTP API (Go)
  ├── validates request and records lifecycle events in SQLite
  ├── creates a QCOW2 overlay and cloud-init seed ISO
  └── starts QEMU as a detached host process
                 │
                 ├── QMP Unix socket: runtime status and supported actions
                 ├── virtual storage/network devices: provided by QEMU
                 └── guest CPU execution: accelerated through KVM (/dev/kvm)
```

CPS omits Libvirt and starts QEMU directly. It does **not** omit QEMU. QEMU can run with software emulation too, but CPS's native target requires Linux KVM and uses KVM acceleration.

### How a VM is provisioned

| Step | Operation | Result |
|---|---|---|
| 1. Validate | Check image name and request fields, and apply allowed defaults/limits. | A validated launch request. |
| 2. Reserve | Create an instance ID, lifecycle record, instance directory, and host-side SSH port allocation. | A persistent `CREATING` record and working directory. |
| 3. Create disk | Run `qemu-img` to create a per-instance QCOW2 overlay backed by the base image. | `instances/<id>/root.qcow2`. |
| 4. Configure guest | Generate cloud-init `user-data` and `meta-data` with instance identity, SSH public key, and optional user-data. | Guest initialization metadata. |
| 5. Build seed | Package cloud-init files into a `cidata` ISO using the available ISO tool. | `instances/<id>/cidata.iso`. |
| 6. Launch | Start QEMU with KVM, disks, networking/port forwarding, logs, PID tracking, and QMP. | A VM process on the local host. |
| 7. Reconcile | Query process/QMP state and update SQLite as operations complete or fail. | Current inventory and durable lifecycle events. |

Provisioning is asynchronous at the API level. A successful create request may initially return `CREATING`; clients should poll `GET /instances/{id}` until the state is `RUNNING` or `FAILED`.

### Cloud images, QCOW2, and copy-on-write

An installer ISO and a cloud image serve different purposes:

| Artifact | What it contains | CPS use |
|---|---|---|
| Installer ISO, such as a workstation installer | Bootable installer environment that expects someone or some automation to perform installation. | Not a base disk image for the current provisioning workflow. |
| Pre-installed cloud disk, usually `.qcow2` | Installed operating system, bootloader, userspace, and usually cloud-init support. | Appropriate base image for CPS. |
| Cloud-init seed ISO | Small configuration disk containing `user-data` and `meta-data`. | Attached to the VM at boot so cloud-init can configure the instance. |

CPS keeps base images as reusable templates and creates a writable QCOW2 overlay for each VM. The base remains unchanged while guest writes are recorded in the overlay.

```text
[ Base cloud image: images/alpine-cloud.qcow2 ]
                    ▲
                    │ backing-file reference
                    │
[ Per-instance overlay: instances/<id>/root.qcow2 ]
  Reads missing blocks from the base image; stores guest writes here.
```

QCOW2 can use sparse allocation and backing files, so a newly created overlay may occupy much less host disk space than its virtual capacity. Physical disk use increases as the guest writes data. Exact initial overhead and cluster allocation depend on image and QCOW2 settings; do not assume a fixed metadata size.

### Cloud-init and SSH identity

CPS supplies the guest with a cloud-init seed disk. On first boot, a cloud-init-enabled image reads the instance metadata and user-data, configures the guest, and installs the authorized **public** SSH key for the configured account.

SSH public-key login uses a key pair:

| File | Where it belongs | Purpose |
|---|---|---|
| `~/.ssh/id_ed25519` | Client machine only | Private key. Never upload it to CPS or put it in cloud-init. |
| `~/.ssh/id_ed25519.pub` | Sent to CPS in the API request | Public key that cloud-init adds to the guest's `authorized_keys`. |

SSH authentication relies on the client proving possession of the private key with a digital signature. The server checks that signature against the authorized public key. The private key never needs to leave the client machine.

The default SSH username depends on the image and its cloud-init configuration. For example, Fedora Cloud often uses `fedora`, Ubuntu Cloud often uses `ubuntu`, and Alpine cloud images can use `alpine`, but verify the actual selected image. CPS can accept an explicit `ssh_user` where supported by the API.

### Why CPS uses `exec.Command`

Go's `os/exec` package starts a specific executable with an explicit list of arguments. CPS uses it for tools such as `qemu-img`, the ISO generator, and `qemu-system-x86_64`.

| Mechanism | Purpose | CPS usage |
|---|---|---|
| `exec.Command(program, args...)` | Starts a known host executable without asking a shell to interpret a command string. Exit status and combined output can be checked. | Create disks, build cloud-init ISOs, and start QEMU. |
| QMP over a Unix socket | Sends structured runtime-control requests to a QEMU process that is already running. | Inspect VM state and request supported lifecycle operations. |
| Shell command string | Asks a shell to parse operators, expansions, pipes, and redirections. | Not required for normal CPS host tool execution and should not be used to interpolate untrusted request values. |

`exec.Command` is not inherently a hack. It is a normal operating-system process API. The important safeguards are to use a fixed executable, pass arguments separately, validate request data, check errors and exit status, set resource limits, and avoid building shell command strings from client input. QMP complements process creation by enabling runtime control after QEMU starts.

## Project structure

```text
CPS/
├── cmd/
│   └── cpsd/
│       └── main.go              # Flags, logging, wiring, HTTP server lifecycle
├── internal/
│   ├── api/
│   │   ├── middleware.go        # Request logging and persistent HTTP audit events
│   │   └── server.go            # HTTP routes, validation, JSON responses
│   ├── cloudinit/
│   │   ├── cloudinit.go         # Cloud-init user-data generation
│   │   └── cloudinit_test.go
│   ├── compute/
│   │   ├── hypervisor.go        # QEMU lifecycle, disks, ISO generation, logs
│   │   ├── images.go            # QCOW2 image discovery and validation
│   │   ├── qmp.go               # QEMU Monitor Protocol client
│   │   └── qmp_test.go
│   ├── db/
│   │   ├── sqlite.go            # SQLite schema, transactions, inventory, audit trail
│   │   └── sqlite_test.go
│   ├── model/
│   │   └── types.go             # API models and lifecycle states
│   └── service/
│       ├── service.go           # Provisioning, state machine, reconciliation
│       └── service_test.go
├── images/                      # Base *.qcow2 cloud images
├── instances/                   # Generated disks, ISOs, sockets, PID files, and logs
├── cps.db                       # Created at runtime; do not commit
├── cps.log                      # Created at runtime; do not commit
├── go.mod
└── go.sum
```

The exact files in your local checkout may differ if you are still on an earlier commit. Use this layout as the target layout for the implementation documented here.

## Requirements and installation

### Clone the repository

If you have not cloned your GitHub repository yet, replace the example URL with your repository URL. If you already have the project checked out, skip this step and run the remaining commands from that repository's root directory.

```bash
REPO_URL="https://github.com/pth55/Compute-Provisioning-Service.git"
git clone "$REPO_URL" ComputeProvionerService
cd ComputeProvionerService
```

CPS targets Linux x86-64 with KVM. The current implementation invokes `qemu-system-x86_64` and is not a native macOS/Windows hypervisor service.

On Fedora or another compatible RHEL-based system, install the required tools:

```bash
sudo dnf install -y qemu-kvm qemu-img xorriso gcc golang openssh-clients jq
```

Verify CPU virtualization, KVM device access, and required tools:

```bash
egrep -c '(vmx|svm)' /proc/cpuinfo
ls -l /dev/kvm
command -v qemu-system-x86_64 qemu-img xorriso
```

The first command should print a value greater than zero. If `/dev/kvm` is missing or permission-denied, enable hardware virtualization in firmware and verify that the host kernel exposes KVM and that your user can access the device. If you change group membership, log out and back in for it to take effect.

CPS uses the CGO-based `github.com/mattn/go-sqlite3` driver. A working C compiler is required when Go builds this dependency. The Go module declares its own required Go version; install a supported version compatible with `go.mod`.

## Configure base images

Copy bootable, pre-installed **QCOW2 cloud images** into the repository's `images/` directory. The guest image should support cloud-init if you want the injected SSH key and user-data to work.

For example, if you already have the files downloaded, use names such as:

```text
images/
├── alpine-cloud.qcow2
└── fedora-cloud.qcow2
```

The service discovers the actual image names and supported aliases through `GET /images` and `GET /metadata`. Example aliases may include `alpine-cloud`, `fedora-cloud`, `ubuntu-cloud`, and `debian-cloud`, depending on the filenames detected by the implementation. Use the API response as the source of truth. CPS does not automatically download or update base images.

An image is not valid merely because its filename ends in `.qcow2`: it must be readable, bootable for the configured architecture, compatible with QEMU, and configured sufficiently for the intended cloud-init workflow.

## Run the service

Run commands from the repository root, where `go.mod` lives. The configured data directory should contain `images/` so CPS can discover the base images.

```bash
go mod tidy
go test ./...
go run ./cmd/cpsd -addr 127.0.0.1:8080 -data-dir .
```

Do not use `go run main.go` for the refactored layout because the entry point is `cmd/cpsd/main.go`.

Runtime files are stored under the selected data directory:

| Path | Purpose |
|---|---|
| `cps.db` | Persistent instance inventory and audit/event history. |
| `cps.log` | Service and HTTP access logs; also emitted to stdout. |
| `instances/<instance-id>/` | Per-VM disk overlay, cloud-init files/ISO, QMP socket, PID information, and logs. |
| `images/` | Reusable base images. These should not be modified by running guests. |

Configuration can be supplied by environment variables or flags:

| Setting | Example | Meaning |
|---|---|---|
| `CPS_ADDR` | `127.0.0.1:8080` | HTTP listen address. |
| `CPS_DATA_DIR` | `.` | Data directory containing `images/`, with `instances/`, database, and logs. |
| `CPS_REGION` | `local` | Region label recorded in inventory; not a real placement scheduler. |
| `CPS_ZONE` | `local-1a` | Availability-zone label recorded in inventory; not a real availability zone. |
| `CPS_LOG_LEVEL` | `debug` | Logging level: `debug`, `info`, `warn`, or `error`; defaults to `info`. |

The command-line flags `-addr` and `-data-dir` are also supported. Keep the API on loopback unless you add authenticated access control and appropriate network protections. **CPS currently has no authentication or authorization.**

## API reference

All endpoints return JSON except the per-instance logs endpoint. Error responses use a structure similar to:

```json
{
  "error": {
    "code": "invalid_request",
    "message": "Description of the problem"
  }
}
```

| Method | Endpoint | Purpose |
|---|---|---|
| `GET` | `/healthz` | Liveness check for the HTTP API. It does not prove that every guest is healthy. |
| `GET` | `/metadata` | Host CPU/RAM/disk capacity, allocatable headroom, images, and state counts. |
| `GET` | `/images` | Discoverable QCOW2 images and aliases. |
| `GET` | `/inventory` | Combined read-only view of metadata, instance records, and recent events. |
| `GET` | `/events?instance_id=i-...&limit=100` | Read-only audit history. `instance_id` is optional; limit is capped at 500. |
| `GET` | `/instances` | List all instance records, including stopped and decommissioned records. |
| `GET` | `/instances?state=RUNNING` | List instance records filtered by lifecycle state. |
| `POST` | `/instances` | Submit a create request. Returns `202 Accepted` and an initial `CREATING` record while work continues asynchronously. |
| `GET` | `/instances/{id}` | Get one instance, including state and connection metadata. |
| `PUT` | `/instances/{id}` | Change supported CPU/RAM settings or grow the virtual disk while stopped. |
| `POST` | `/instances/{id}/actions` | Run `start`, `stop`, `force_stop`, `pause`, `resume`, or `hibernate`. |
| `POST` | `/instances/{id}/action` | Legacy singular alias for the lifecycle action endpoint. |
| `GET` | `/instances/{id}/logs?type=qemu&lines=200` | Read a tail of the `qemu`, `serial`, or `debug` log; maximum 1,000 lines. |
| `DELETE` | `/instances/{id}` | Decommission the VM and remove local instance files while retaining its inventory history. |

The exact JSON fields returned by each endpoint are defined by the API model. For scripts, use the response's `id`, `status`, `ssh_host`, `ssh_port`, `ssh_user`, `node_name`, `region`, `zone`, and `last_error` fields where present rather than assuming a hard-coded port or guest username.

## Provision a VM and poll its status

Create or reuse an SSH key pair. The private key remains on your client machine; only the public key is sent to CPS.

```bash
[ -f ~/.ssh/id_ed25519.pub ] || ssh-keygen -t ed25519 -N "" -f ~/.ssh/id_ed25519
PUB_KEY="$(cat ~/.ssh/id_ed25519.pub)"
```

Check which images are available before making a create request:

```bash
curl -sS http://127.0.0.1:8080/metadata | jq .
curl -sS http://127.0.0.1:8080/images | jq .
```

Submit a VM creation request. `image_name` must match an image name or alias returned by the API. In this example, `user_data` is a command executed **inside the guest by cloud-init**, not on the CPS host.

```bash
REQUEST="$(jq -n \
  --arg image_name "alpine-cloud" \
  --arg ssh_public_key "$PUB_KEY" \
  --arg user_data "echo 'Instance successfully initialized' > /root/status.log" \
  '{image_name:$image_name,vcpus:2,memory_mb:1024,disk_size_gb:10,ssh_public_key:$ssh_public_key,user_data:$user_data}')"

RESPONSE="$(curl -sS -X POST http://127.0.0.1:8080/instances \
  -H 'Content-Type: application/json' \
  -d "$REQUEST")"
printf '%s\n' "$RESPONSE" | jq .
INSTANCE_ID="$(printf '%s\n' "$RESPONSE" | jq -r '.id // empty')"
test -n "$INSTANCE_ID" || { echo "Create request failed" >&2; exit 1; }
```

Provisioning is asynchronous. Poll the instance endpoint until it reaches a terminal result for creation:

```bash
while :; do
  INSTANCE="$(curl -sS "http://127.0.0.1:8080/instances/$INSTANCE_ID")"
  printf '%s\n' "$INSTANCE" | jq '{id,status,pid,ssh_host,ssh_port,ssh_user,node_name,region,zone,last_error}'
  STATE="$(printf '%s\n' "$INSTANCE" | jq -r '.status')"
  case "$STATE" in
    RUNNING|FAILED|DECOMMISSIONED) break ;;
  esac
  sleep 2
done
```

The instance record tells you the host on which CPS believes it is running, the QEMU PID, disk location, and SSH connection details. In the current implementation, `node_name` refers to the local machine. `region` and `zone` are labels, not the result of a scheduler choosing between remote machines.

## Connect to a VM

Use the connection fields returned by the instance API rather than assuming every VM uses port `2222` or the same guest username.

```bash
INSTANCE="$(curl -sS "http://127.0.0.1:8080/instances/$INSTANCE_ID")"
SSH_HOST="$(printf '%s' "$INSTANCE" | jq -r '.ssh_host')"
SSH_PORT="$(printf '%s' "$INSTANCE" | jq -r '.ssh_port')"
SSH_USER="$(printf '%s' "$INSTANCE" | jq -r '.ssh_user')"

ssh -i ~/.ssh/id_ed25519 \
  -o "UserKnownHostsFile=$HOME/.ssh/known_hosts.cps-$INSTANCE_ID" \
  -o StrictHostKeyChecking=accept-new \
  -p "$SSH_PORT" "$SSH_USER@$SSH_HOST"
```

A unique known-hosts file per instance avoids host-key mismatch errors if a host-side port is later reused for another VM. With QEMU user-mode networking, the SSH destination is a host-side forwarded port, not a public IP assigned directly to the guest. From another machine, connect through an SSH tunnel or implement a separately secured bridge/network path.

If SSH does not connect immediately, wait for cloud-init to finish and inspect the instance's QEMU/serial logs and event history. For the example Alpine user-data, verify inside the guest with an account that has appropriate permissions:

```sh
su -
cat /root/status.log
```

Alpine often does not include `sudo` by default. Do not assume the guest's root account is directly accessible using `sudo`; account and privilege behavior depends on the image and cloud-init configuration.

## Lifecycle actions and states

Use `POST /instances/{id}/actions` with a JSON body:

```bash
curl -sS -X POST "http://127.0.0.1:8080/instances/$INSTANCE_ID/actions" \
  -H 'Content-Type: application/json' \
  -d '{"action":"stop"}' | jq .
```

### Supported actions

| Action | Meaning | Important detail |
|---|---|---|
| `start` | Start a stopped VM or restore a hibernated VM. | Reuses the instance's root disk; hibernation restore requires a valid saved state. |
| `stop` | Request guest OS shutdown through QMP's ACPI power-button event. | Normally transitions through `STOPPING`; poll until `STOPPED`. If the guest ignores ACPI shutdown, use `force_stop`. |
| `force_stop` | Forcefully terminate the verified QEMU process. | Equivalent to cutting power; guest filesystems or application data may be inconsistent. |
| `pause` | Stop guest CPU execution through QMP while retaining guest RAM in host memory. | This is not hibernation; host reboot or process loss destroys the in-memory state. |
| `resume` | Resume a paused VM. | Returns `PAUSED` to `RUNNING`. |
| `hibernate` | Save VM runtime state to a QEMU internal QCOW2 snapshot and exit QEMU. | Depends on QEMU version, device configuration, internal snapshot support, and free disk space. Test it against the exact VM image before relying on it. |

### Lifecycle state reference

| State | Meaning |
|---|---|
| `CREATING` | The request has been accepted and VM artifacts are being created. |
| `STARTING` | CPS is launching or restoring QEMU. |
| `RUNNING` | QEMU/QMP reports a running guest. |
| `STOPPING` | A graceful guest shutdown has been requested and is not yet confirmed complete. |
| `STOPPED` | The VM is stopped but its retained disk and instance record remain available for a later start, where supported. |
| `PAUSED` | QEMU remains alive, but guest execution is paused and its RAM state remains in host memory. |
| `HIBERNATING` | CPS is saving the guest runtime state before stopping QEMU. |
| `HIBERNATED` | The guest runtime state was saved and the QEMU process exited. Restoration depends on the saved snapshot and configuration still being valid. |
| `FAILED` | Provisioning failed or a guest that should be running could not be recovered/reconciled. Inspect logs and events. |
| `UNKNOWN` | CPS cannot confidently determine the actual runtime state. |
| `DECOMMISSIONING` | CPS is stopping the VM and removing its local artifacts. |
| `DECOMMISSIONED` | VM artifacts have been removed and the retained row is historical inventory. It cannot be restarted. |

Every persisted lifecycle change creates an event. The periodic reconciler checks QEMU state and attempts to repair stale database state; the interval and exact behavior are implementation configuration, not a cross-host consensus mechanism.

Graceful shutdown example:

```bash
curl -sS -X POST "http://127.0.0.1:8080/instances/$INSTANCE_ID/actions" \
  -H 'Content-Type: application/json' -d '{"action":"stop"}' | jq .
watch -n 2 "curl -sS http://127.0.0.1:8080/instances/$INSTANCE_ID | jq '{id,status,pid,last_error}'"
```

Pause and resume:

```bash
curl -sS -X POST "http://127.0.0.1:8080/instances/$INSTANCE_ID/actions" \
  -H 'Content-Type: application/json' -d '{"action":"pause"}' | jq .
curl -sS -X POST "http://127.0.0.1:8080/instances/$INSTANCE_ID/actions" \
  -H 'Content-Type: application/json' -d '{"action":"resume"}' | jq .
```

Hibernate and restore, after verifying support with your QEMU build:

```bash
curl -sS -X POST "http://127.0.0.1:8080/instances/$INSTANCE_ID/actions" \
  -H 'Content-Type: application/json' -d '{"action":"hibernate"}' | jq .
curl -sS -X POST "http://127.0.0.1:8080/instances/$INSTANCE_ID/actions" \
  -H 'Content-Type: application/json' -d '{"action":"start"}' | jq .
```

## Change VM configuration

`PUT /instances/{id}` supports offline CPU/RAM updates and virtual disk growth. It intentionally rejects unsupported changes while a VM is running and rejects disk shrinking.

```bash
curl -sS -X PUT "http://127.0.0.1:8080/instances/$INSTANCE_ID" \
  -H 'Content-Type: application/json' \
  -d '{"vcpus":4,"memory_mb":2048,"disk_size_gb":20}' | jq .
```

Increasing virtual disk capacity does not automatically enlarge guest partitions or filesystems. Run the guest-appropriate partition/filesystem expansion procedure after boot if you need the guest to use the newly available capacity.

## Inspect host resources, inventory, logs, and events

### Useful read-only endpoints

```bash
# Host capacity and image catalog
curl -sS http://127.0.0.1:8080/metadata | jq .
curl -sS http://127.0.0.1:8080/images | jq .

# All instances, including stopped and decommissioned rows
curl -sS http://127.0.0.1:8080/instances | jq .
curl -sS 'http://127.0.0.1:8080/instances?state=RUNNING' | jq .
curl -sS http://127.0.0.1:8080/inventory | jq .

# Recent events, optionally scoped to one instance
curl -sS 'http://127.0.0.1:8080/events?limit=100' | jq .
curl -sS "http://127.0.0.1:8080/events?instance_id=$INSTANCE_ID&limit=100" | jq .
```

### Logs

```bash
# All CPS application and HTTP access logs
tail -f cps.log

# Per-instance log files
tail -f "instances/$INSTANCE_ID/qemu.log"
curl -sS "http://127.0.0.1:8080/instances/$INSTANCE_ID/logs?type=qemu&lines=200"
curl -sS "http://127.0.0.1:8080/instances/$INSTANCE_ID/logs?type=serial&lines=200"
curl -sS "http://127.0.0.1:8080/instances/$INSTANCE_ID/logs?type=debug&lines=200"
```

| Log | Contents |
|---|---|
| `cps.log` | CPS HTTP requests, state transitions, resource actions, and internal errors; also emitted to stdout. |
| `qemu.log` | Provisioning/tool output and QEMU startup output. |
| `serial.log` | Guest serial-console output, if the image sends boot/runtime output to its serial console. |
| `qemu-debug.log` | QEMU `guest_errors` diagnostic output, where enabled. |
| SQLite `events` table | Durable HTTP audit events and lifecycle/activity events. |

CPS intentionally does not log SSH public-key payloads, request bodies containing user-data, or URL query strings. The current event history does not have automatic retention/pruning by default; plan a retention strategy before high-volume or long-running use.

## Decommission a VM

```bash
curl -sS -X DELETE "http://127.0.0.1:8080/instances/$INSTANCE_ID" | jq .
```

Decommissioning stops the VM when possible, removes its per-instance files, and retains a `DECOMMISSIONED` row plus the event history in SQLite. A decommissioned instance is historical inventory and cannot be restarted. Create a new instance instead.

## Persistence, reconciliation, and recovery

| Event | Expected behavior | Limitation |
|---|---|---|
| CPS API restart | SQLite state remains on disk. QEMU is launched detached, so stopping/restarting the API does not intentionally stop running guests. On startup, CPS checks its inventory against saved PID/QMP/process information and continues periodic reconciliation. | This does not protect against host or kernel failure. Always stop CPS cleanly and keep the database and instance directories on persistent storage. |
| QEMU exits unexpectedly | A VM that should be running but has no matching process is marked failed or otherwise reconciled according to available evidence. Inspect QEMU/serial/debug logs and `/events`. | A PID alone is not proof of VM identity or health; recovery should verify the command line and recorded disk path. |
| Host reboot or power loss | CPS can reconcile stale database rows on its next start. | An ordinary running VM does not survive host reboot with RAM intact. Without a compatible saved state, the VM may be marked failed and need a manual start/recovery decision. |
| Old prototype VM predates QMP support | Its inventory may be retained and may be discoverable by its process and disk metadata. | QMP-driven stop, pause/resume, and hibernation may be unavailable. Only signal a process after validating it belongs to the recorded instance. |
| Database/file mismatch | Reconciliation can identify some stale records and runtime objects. | SQLite and the filesystem are separate resources; filesystem operations cannot be made atomic with a SQL transaction. Provisioning therefore needs explicit cleanup and reconciliation paths. |

SQLite is appropriate for this single-host learning project. It is not a distributed consensus database or a multi-host scheduler.

## Testing and validation

Run the checks on the target Fedora host after dependencies are available:

```bash
gofmt -w cmd internal
go test ./...
go vet ./...
```

SQLite integration tests require Go to download `github.com/mattn/go-sqlite3`; Fedora needs a working C compiler for the CGO dependency. Test lifecycle behavior on the host with actual VMs, including API restart, forced QEMU exit, host reboot recovery, port reuse, hibernation, and decommission cleanup. Hibernation must be tested on the exact QEMU build and guest image before you rely on it.

## Current limitations

| Area | Current state |
|---|---|
| Compute | Single Linux x86-64 KVM host per CPS process. |
| Placement | `region` and `zone` are labels. No multi-host scheduler selects among hardware nodes. |
| Networking | QEMU user-mode networking with host-side port forwarding; no production bridge/VPC/IPAM implementation. |
| Security | No authentication/authorization or multi-tenant isolation at the API layer. Keep the API on loopback. |
| Availability | No HA controller or distributed consensus. SQLite and the runtime directories are local to the host. |
| Recovery | Reconciliation can repair some state drift, but it cannot guarantee guest survival through host failure. |
| Operations | No comprehensive quotas, admission control guarantees, alerting platform, automatic event retention, or production backup orchestration. |
| Portability | Native target is Linux x86-64 with KVM. Other host platforms require architectural and implementation changes. |

## Future scope

The following are proposed development stages, not claims about functionality that already exists.

| Priority | Workstream | What to build | Why it matters |
|---|---|---|---|
| 1 | API security | Authentication, authorization/RBAC, TLS, request limits, input validation, and audit controls. | Prevents an unauthenticated caller from creating, stopping, or destroying host VMs. |
| 2 | Reliability and idempotency | Idempotency keys, explicit operation records, retries, deadlines, cancellation, and robust cleanup/reconciliation. | Prevents duplicate VMs and makes partial failures recoverable. |
| 3 | Host isolation and resources | cgroups v2, resource admission checks, disk quotas, process supervision, and safe host tool execution. | Limits noisy-neighbor/resource-exhaustion problems and improves operational safety. |
| 4 | VM networking | Linux bridges/TAP devices, IP address management, isolation rules, and optional routed networking. | Moves beyond host-only SSH port forwarding. |
| 5 | Observability | Prometheus metrics, structured logs, request/operation correlation, health/readiness checks, alerts, and event retention. | Makes failures measurable and diagnosable. |
| 6 | Multi-host node agent | A per-host `cps-agent` that controls local QEMU/KVM and reports capacity/state to the control plane using authenticated gRPC/TLS. | Separates global policy from host-local execution. |
| 7 | Scheduler and inventory | A scheduler using available vCPU, RAM, disk, capabilities, zones, and anti-affinity; transactional placement reservations. | Places VMs on appropriate hosts without overcommitting known resources. |
| 8 | Distributed state and HA | Move from local SQLite to a suitable shared relational database, define failover/fencing, and run redundant controllers. | Avoids a single controller/database as the only source of truth. |
| 9 | Advanced VM operations | Backups, snapshot policy, restore testing, live migration where supported, image versioning, and secure image validation. | Improves maintainability and recovery. |
| 10 | Control-plane product features | Quotas, tenants/projects, API versioning, pagination, rate limiting, and an optional web console. | Makes the service safer and easier to operate as a shared platform. |

### Future multi-host architecture

A real multi-region deployment would not have one Go HTTP process directly controlling every QEMU process over the public internet. A likely evolution for CPS is to divide responsibilities between a central control plane and regional/zone node agents:

```text
                     Clients / CLI / Web Console
                               │ HTTPS + Auth
                               ▼
                  ┌───────────────────────────┐
                  │ Global/Regional API       │
                  │ Auth, policy, validation   │
                  └─────────────┬─────────────┘
                                │
               ┌────────────────▼────────────────┐
               │ Durable Inventory + Scheduler   │
               │ Placement, reservations, events │
               └─────────┬──────────────┬────────┘
                         │ mTLS/gRPC     │ mTLS/gRPC
              ┌──────────▼──────┐  ┌─────▼────────────┐
              │ Zone A           │  │ Zone B           │
              │ cps-agent        │  │ cps-agent        │
              │ capacity/reconcile│ │ capacity/reconcile│
              └──────────┬──────┘  └─────┬────────────┘
                         │ local QMP      │ local QMP
                    ┌────▼─────┐      ┌───▼──────┐
                    │ QEMU/KVM │      │ QEMU/KVM │
                    │ Host A1  │      │ Host B1  │
                    └──────────┘      └──────────┘
```

The scheduler would reserve resources transactionally, select an eligible host, and ask that host's agent to perform local operations. Agents would report observed state, while reconciliation would compare observed hypervisor state with desired inventory state. A production design also needs node identity, authenticated transport, timeouts, retries, fencing to prevent duplicate ownership, image distribution, network orchestration, and tested database failover. None of these multi-host components are implemented in the current CPS version.

## Technical Q&A

### 1. What do the different cloud image and boot artifact formats mean?

The files offered by a Linux distribution are not all the same type of artifact. Some are virtual disks, some are packages for VM tooling, and some are boot executables.

| Format | What it is | Key point |
|---|---|---|
| QCOW2 (`.qcow2`) | QEMU virtual disk format. | Supports features such as sparse allocation, backing files, and snapshot metadata. Used by CPS as the base/overlay disk format. |
| Raw (`.raw`, sometimes `.raw.xz`) | Flat byte-for-byte disk image, sometimes compressed for download. | Little format-level overhead; does not itself provide QCOW2 backing chains/snapshot metadata. Actual allocated host space depends on sparsity and storage. |
| VHD (`.vhd`, sometimes `.vhd.xz`) | Virtual hard disk format used in Microsoft virtualization/Azure workflows. | `.xz` means the downloaded artifact is compressed and may need decompression before use. Tooling and platform-specific image constraints apply. |
| Vagrant box (`.box`) | A Vagrant package, typically including a virtual disk plus metadata and provider configuration. | It is a package format, not a virtual disk format by itself. |
| UKI (`.efi`) | Unified Kernel Image: a UEFI PE/COFF executable bundling a boot stub, Linux kernel, initramfs, and kernel command-line data. | Can be signed for Secure Boot verification. It is a boot artifact, not a replacement for a VM's QCOW2/raw root disk. |

#### Why are IBM `s390x` and Power `ppc64le` architectures listed alongside `x86_64` images?

Linux distributions publish images for multiple CPU architectures. `s390x` is IBM Z's 64-bit architecture, commonly used on enterprise mainframes. `ppc64le` is the 64-bit little-endian Power architecture. These images do not run as native x86-64 guests without an appropriate architecture/emulation setup. The current CPS implementation uses `qemu-system-x86_64`, so its present launch path targets x86-64 guests.

### 2. What is the difference between KVM and QEMU? Does CPS use both?

Yes, CPS uses both. KVM is the Linux kernel interface that accelerates eligible guest CPU execution using hardware virtualization support. QEMU is the userspace VM process that provides virtual devices, connects storage and networking, allocates guest memory, and operates the machine. QEMU talks to KVM through `/dev/kvm`. Libvirt is optional management software and is not required by this project.

### 3. Why does CPS use `exec.Command` instead of controlling everything through QMP?

A VM must be created before it can be controlled at runtime. CPS uses `exec.Command` to launch tools such as `qemu-img`, the ISO generator, and `qemu-system-x86_64` with explicit arguments. Once QEMU is running with a configured QMP socket, CPS can send structured QMP requests for supported inspection and lifecycle actions. `exec.Command` does not automatically imply shell execution; passing an executable and arguments directly is a normal Go process-management pattern. The implementation must still validate inputs, check failures, and restrict resource use.

### 4. How does SSH public-key authentication work, and why must the private key stay local?

Generate a key pair with `ssh-keygen`. The `.pub` file contains the public key and is supplied to CPS. Cloud-init places it in the guest account's `authorized_keys`. During connection, the SSH client signs data associated with the session using the private key; the guest's SSH server verifies the signature with the authorized public key. The private key is never sent to the VM or CPS. Do not add private keys or plaintext default passwords to user-data.

### 5. How does cloud-init choose the default VM username?

The username is determined by the cloud image's own cloud-init configuration and any overrides provided by CPS. Common images often use accounts such as `fedora`, `ubuntu`, or `alpine`, but there is no one universal default. Check the selected image's documentation/configuration or supply the account explicitly if the API and image support it. If SSH key authentication fails, check the username, cloud-init logs, image compatibility, and SSH port before assuming the key is wrong.

### 6. Where is my VM running, and how do I connect to it?

In the current implementation, the VM is a QEMU process on the same Linux host running CPS. The per-instance response includes the host/node label, PID, disk location, and SSH host/port/user. QEMU user-mode networking forwards a host-side port to the guest's SSH port. Use the fields returned by `GET /instances/{id}`; do not assume that port `2222` is assigned to every VM. Remote access requires an SSH tunnel or separately configured, secured host/guest networking. Region and zone values in the current single-host version are labels, not physical-placement decisions.

### 7. What happens if I create several VMs? Will they all use `localhost:2222`?

They cannot all bind the same host TCP port at once. CPS should allocate a distinct available host-side SSH port for each concurrently running guest and persist that port in the instance record. Client scripts must read `ssh_port` from the API response rather than hard-code `2222`. Port allocation must check that the port is genuinely available and handle collisions/reuse; a simple incrementing counter alone is not robust across API restarts or multiple CPS processes. A unique SSH known-hosts file per instance also avoids host-key mismatch warnings when ports are reused.

### 8. Will my VMs remain in the inventory after CPS restarts?

SQLite stores the durable instance records and event history, so the inventory remains after the HTTP process restarts as long as `cps.db` is kept. QEMU is launched separately from the API process, and startup/periodic reconciliation compares database records with process and QMP evidence. This does not protect a guest from host reboot, kernel failure, disk failure, or power loss. A normal running VM's RAM state does not persist through a host reboot unless it has been saved in a valid supported hibernation state.

### 9. Are pause and hibernation the same thing?

No. **Pause** freezes guest execution while the QEMU process and guest RAM remain in host memory. **Hibernation** attempts to write runtime state to persistent storage and stop QEMU, so the guest can later be restored. A paused VM will lose its in-memory state if the host reboots or the QEMU process exits. Hibernation depends on QEMU version, device support, snapshot compatibility, and disk space and must be tested on the target setup.

### 10. What do region and availability-zone fields mean in CPS today?

Today, `CPS_REGION` and `CPS_ZONE` are metadata labels. A single local QEMU process does not make a multi-region deployment. In a future architecture, a scheduler would select from registered hosts using capacity, architecture, capabilities, zone policy, anti-affinity constraints, and durable reservations. A node agent on the chosen host would launch and monitor QEMU locally, then report observed state back to the control plane over authenticated RPC.

### 11. How do large public clouds manage so many physical hosts?

A hyperscale cloud generally separates customer-facing APIs and identity/policy from regional placement, durable inventory, host agents, and the host-local virtualization stack. Exact internals differ by provider and are not fully public. The reusable design principles are control-plane separation, durable desired state, scheduling, per-host agents, health monitoring, reconciliation, failure isolation, capacity management, and secure internal communications. CPS's future multi-host architecture sketches those building blocks but does not implement hyperscale scheduling or provider-specific systems.

### 12. Why do some infrastructure jobs focus on VMware while cloud providers hire systems engineers?

Organizations consume and operate infrastructure at different abstraction layers. Many enterprises use vendor platforms such as VMware and hire engineers to manage clusters, capacity, reliability, automation, and vendor integration. Cloud providers must also build and operate their own compute platforms, host agents, hypervisor integrations, networking, storage, fleet orchestration, and reliability systems, so related roles may use titles such as Systems Development Engineer, Software Engineer, infrastructure engineer, kernel engineer, or distributed-systems engineer. Job titles vary widely; the meaningful distinction is the systems being built and operated, not the title alone. Skills from this CPS project—Linux internals, KVM/QEMU, Go, API design, state reconciliation, storage, observability, and distributed systems—are useful foundations for infrastructure engineering, but this project alone is not equivalent to operating production-scale cloud infrastructure.

### 13. Can CPS run on Linux, macOS, and Windows?

| Host platform | Virtualization interface | CPS status |
|---|---|---|
| Linux x86-64 | KVM through `/dev/kvm`, using Intel VT-x or AMD-V. | Native target of this implementation. |
| macOS | Apple's Hypervisor.framework (HVF), which QEMU can use on supported configurations. | Not supported unchanged: CPS currently assumes Linux KVM and `qemu-system-x86_64`. Apple Silicon is ARM64, so an ARM64 guest is usually the natural match; x86-64 guests require emulation/translation and may be much slower. |
| Windows 11 | Windows Hypervisor Platform (WHPX) or Hyper-V-related acceleration in supported QEMU builds. | Not supported unchanged because the implementation relies on Linux KVM interfaces and Linux paths/tools. |
| Windows with WSL 2 | A Linux kernel running inside a WSL 2 VM; nested virtualization and `/dev/kvm` availability depend on host/Windows/WSL configuration and policy. | Experimental only. Verify that `/dev/kvm` is actually available within the environment before attempting to run CPS. |

---

## License

Add the license that matches the repository's intended distribution before publishing or accepting external contributions.
