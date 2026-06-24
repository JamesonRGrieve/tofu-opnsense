# tofu-opnsense — Agent Operating Guide

> **⛔ NO DIRECT APPLIES TO ANY DEVICE — EVER.**
>
> Direct changes to **any** device — router, firewall, switch, access point, hypervisor, mail gateway, or any other appliance — are **NEVER** permitted, by anyone, for any reason. This bans hand-run `tofu apply`, hand-run `ansible-playbook`, SSH/serial/CLI config writes, REST/API mutations, and web-GUI/console edits.
>
> **Every change MUST flow through the sanctioned pipeline:** declare intent in **prod-netbox** (the single source of truth), then realize it **only** through **prod-semaphore** (the sanctioned runner). A change that did not go **prod-netbox → prod-semaphore** must never reach a device.
>
> **Sole exception:** a specific direct action is permitted *only* when the operator authorizes that exact action in advance by answering an explicit, **alarm-flavored `AskUserQuestion`** — one that names the device, the precise action, and the risk — **in the affirmative**. No standing grants, no inferred permission, no carrying one approval to another action or device. Absent that in-the-moment "yes," the answer is no.
>
> **Never offload the work onto the operator.** When you are blocked, ask for the break-glass authorization that lets *you* do the job — never ask the operator to run a command, SSH in, or make the change on your behalf. The operator grants permission; they do not perform your labour.

Native OpenTofu/Terraform provider for **OPNsense** via its REST API. Sibling of
`../tofu-aruba-aos` and `../openwrt-ubus` (same generic-over-the-API philosophy,
same toolchain). The workspace-root `../CLAUDE.md` applies; this adds specifics.

## What this is / isn't

- **Is:** a provider for OPNsense firewalls, driven entirely through the
  documented REST API (`https://<host>/api`, HTTP Basic auth where the username
  is the API key and the password is the API secret).
- **Isn't:** a pfSense provider, and not a typed per-feature provider. It is
  generic over the RPC surface.

## Design tenets

- **The generic resources here are `opnsense_object` (+ data source)** — they
  address any `module`/`controller`. Resist adding typed resources until there's
  a real ergonomics need.
- **The subset plan modifier is `subsetMatches`**; `body` is the keys we manage,
  declared only. State holds the full device object so a subset imports to
  0-diff and unmanaged fields are never clobbered.
- **Two command families, selected by `singleton`:**
  - collection item (`singleton = false`, default): create =
    `POST <module>/<controller>/addItem {<controller>:body}` (capture `uuid`);
    read = `GET getItem/<uuid>`; update = `POST setItem/<uuid>`; delete =
    `POST delItem/<uuid>`.
  - settings singleton (`singleton = true`): create/update =
    `POST <module>/<controller>/set {<controller>:body}`; read =
    `GET <module>/<controller>/get`; delete is a no-op.
  - After every write, `POST <reconfigure>` (if set) applies the change.
- **Envelopes:** write bodies are wrapped `{<controller>: {...body}}`; read
  responses are unwrapped from `{<controller>: {...}}`. `getItem` for a missing
  UUID returns `[]` (empty) — treated as "gone" → removed from state.
- **Write-result check:** `{"result":"saved"|"deleted"}` is success;
  `{"result":"failed","validations":{...}}` is surfaced as an error.

## Toolchain

- Go 1.26.4 (`/home/jameson/.local/go`), `terraform-plugin-framework` v1.19.0.
  Do not add or bump deps — they mirror `../tofu-aruba-aos`.
- Provider address: `registry.terraform.io/JamesonRGrieve/tofu-opnsense`.
- `make check` (tidy + fmt + vet + test + build) is the gate; `.githooks/pre-commit`
  re-runs it. Enable with `git config core.hooksPath .githooks`. Never `--no-verify`.

## Hard rules

- **No secrets in the repo.** Creds come from the provider config (OpenBao →
  `TF_VAR_*` via Semaphore).
- **NEVER touch `house-opnsense` (the production house backbone firewall).** A
  bad write takes the whole network down. Validate only against an unambiguous
  lab OPNsense, and drive live changes via Semaphore.
