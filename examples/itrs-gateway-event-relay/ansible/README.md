# Deploying with Ansible

Deploys `itrs-notify` (a plain binary Geneos invokes directly as an
Effect) to the Gateway fleet — its EMS URL/token, a dead-letter directory
for failed deliveries, nothing else. Pure `ansible.builtin` — no
collections to install.

There is no daemon to deploy here. An earlier version of this role also
shipped `event-relay` as a systemd service; that's gone now that the real
per-host volume (peak ~835/min, ~14/s) turned out to be far below what
justified a relay — see `../README.md`'s "Why one binary" and
`../../../docs/design/itrs-gateway-event-script.md`'s revision history.
If you're looking at an older checkout with a `roles/itrs_event_relay/`
directory, that's the stale version; this role replaces it.

## Why this shape

- **A static binary per architecture, copied not compiled.** The role
  never runs `go build` on a target host. Build once in CI, drop the
  artifact where `itrs_notify_binaries_dir` points (default `../dist/`),
  and this role fetches the right one per host via `ansible_architecture`.
- **A secrets file, not a systemd `EnvironmentFile=`.** With no
  long-running process, there's no unit file to carry one. The role
  writes `/etc/itrs-notify/env` (mode `0600`) instead, and
  `cmd/itrs-notify/main.go` reads it at startup — filling in only what
  Geneos's own Effect environment didn't already set, so an
  Effect-level override still wins if you ever need one.
- **`serial: "10%"` in `site.yml`.** Still worth keeping even for a
  single binary with no service to restart: ~80 hosts across regions
  means a bad build should hit a canary batch, not the whole fleet, in
  one play.

## Confirm before rollout

- **`itrs_notify_user` / `itrs_notify_group`**
  (`roles/itrs_notify/defaults/main.yml`) — must match the real system
  account Geneos's Effects run as, so the dead-letter directory and
  secrets file are readable/writable by the process that actually needs
  them. The default (`geneos`) is a placeholder. This role does not
  create the account — it's expected to already exist.
- **`itrs_goarch_map`** — extend it if any region runs a non-`x86_64` /
  non-`aarch64` host; an unmapped architecture fails that host's play
  loudly (see `tasks/main.yml`) rather than silently deploying nothing or
  the wrong binary.
- **`itrs_ems_token`** — don't put the real value in `defaults/main.yml`
  or a plaintext `group_vars` file. Use `ansible-vault` and reference it
  from a normal `group_vars` file as
  `itrs_ems_token: "{{ vault_itrs_ems_token }}"`.

## Usage

```bash
# 1. Build the binary once (see ../README.md "Build"), one per
#    architecture your fleet actually runs, named exactly:
#      dist/itrs-notify-linux-amd64
#      dist/itrs-notify-linux-arm64   (if needed)

# 2. Fill in the real inventory.
cp inventory.example.ini inventory.ini
# edit inventory.ini with your ~80 hosts

# 3. Canary a single host first.
ansible-playbook site.yml --limit gw-us-east-01.example.internal --ask-vault-pass

# 4. Then one region, then the rest — `serial: "10%"` in site.yml still
#    applies within whatever --limit selects.
ansible-playbook site.yml --limit itrs_gateway_us --ask-vault-pass
ansible-playbook site.yml --ask-vault-pass   # the full fleet
```

Idempotent: re-running only touches files that actually changed. No
restart/reload step exists or is needed anywhere in this role — Geneos
forks a fresh `itrs-notify` process per trigger, so the very next trigger
after a `copy`/`template` task already sees the new binary or config.
