# Deploying with Ansible

Deploys `event-relay` (as a systemd service) and `itrs-notify` (as a plain
binary for the Geneos Effect to invoke) to the Gateway fleet. Pure
`ansible.builtin` — no collections to install, matching the rest of this
design's "nothing extra to install" bias.

## Why this shape

- **A static binary per architecture, copied not compiled.** The role
  never runs `go build` on a target host — none of the ~80 Gateway hosts
  need a Go toolchain, matching the whole reason this design is Go in the
  first place (see the top-level README's "Why Go" section). Build once in
  CI, drop the artifacts where `itrs_relay_binaries_dir` points (default
  `../dist/`, i.e. next to this `ansible/` directory), and this role
  fetches the right one per host via `ansible_architecture`.
- **One shared system account for both binaries, not two.** `itrs-notify`
  doesn't get to pick its own runtime identity — Geneos forks it as part
  of the Gateway's own process tree, so it always runs as whatever account
  Geneos's Effects already run as. `event-relay` is ours to place, so
  `itrs_relay_user` is set to that *same* existing account rather than a
  new dedicated one — this only holds up because that account is already
  a minimal, purpose-built service account with no broader host access;
  if yours is a general-purpose/broadly-privileged account instead, keep
  the daemon on its own dedicated account and share write access via a
  setgid group directory instead (ask if you need that variant). One user
  means a plain owner-only `0750` spool directory — no shared group, no
  setgid, no cross-user permission plumbing.
- **`serial: "10%"` in `site.yml`.** ~80 hosts across regions is exactly
  the situation `resilience-failure`'s blast-radius thinking applies to
  deployment itself, not just runtime: a bad binary or config value should
  hit a canary batch, not the whole fleet, in one play.

## Confirm before rollout

- **`itrs_relay_user` / `itrs_relay_group`**
  (`roles/itrs_event_relay/defaults/main.yml`) — must match the *actual*
  system account Geneos's Effects run as on your Gateway hosts. The
  default (`geneos`) is a placeholder; get this wrong and either
  `event-relay` won't start as that account, or `itrs-notify`'s
  disk-fallback writes fail with a permission error precisely when it's
  needed most (the daemon is down). Leave `itrs_relay_manage_user: false`
  (the default) since this account already exists — the role only needs
  to use it, not create it.
- **`itrs_goarch_map`** — extend it if any region runs a non-`x86_64` /
  non-`aarch64` host; an unmapped architecture fails that host's play
  loudly (see `tasks/main.yml`) rather than silently deploying nothing or
  the wrong binary.
- **`itrs_ems_token`** — don't put the real value in `defaults/main.yml`
  or a plaintext `group_vars` file. Use `ansible-vault` (e.g. a
  `group_vars/itrs_gateway_hosts/vault.yml` encrypted file, or `--ask-vault-pass`
  / a vault password file) and reference it from a normal `group_vars` file
  as `itrs_ems_token: "{{ vault_itrs_ems_token }}"`.

## Usage

```bash
# 1. Build the binaries once (see ../README.md "Build"), one pair per
#    architecture your fleet actually runs, named exactly:
#      dist/event-relay-linux-amd64   dist/itrs-notify-linux-amd64
#      dist/event-relay-linux-arm64   dist/itrs-notify-linux-arm64   (if needed)

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

Idempotent: re-running only restarts `event-relay` when the binary,
`/etc/itrs-event-relay/env`, or the unit file actually changed (via the
role's `notify` handlers) — safe to run repeatedly, including as a
scheduled config-drift check.

`itrs-notify` needs no restart/reload after a redeploy — Geneos forks a
fresh process per trigger, so the very next trigger after the `copy` task
already runs the new binary.
