# tofu-opnsense — Agent Operating Guide

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
