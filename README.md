<!-- SPDX-License-Identifier: AGPL-3.0-or-later -->
# terraform-provider-opnsense

A native OpenTofu/Terraform provider for **OPNsense** firewalls, driven entirely
through the documented **REST API** (`https://<host>/api`, HTTP Basic auth with
an API key/secret).

## Why generic

OPNsense exposes a broad, uniform RPC-style REST surface:
`/api/<module>/<controller>/<command>[/<uuid>]`. Almost every model controller
follows the same CRUD shape — `addItem`, `getItem/<uuid>`, `setItem/<uuid>`,
`delItem/<uuid>` plus a per-module `service/reconfigure` apply — and settings
controllers follow a `get`/`set` singleton shape. Rather than hand-code a
resource per feature (and chase plugin additions forever), this provider is
**generic over the API** — one resource and one data source address *any*
module/controller. That is full feature coverage by construction.

## Resources

### `opnsense_object` (resource)

CRUD + `ImportState` for any OPNsense model controller.

```hcl
# Collection item (firewall alias): addItem / getItem / setItem / delItem.
resource "opnsense_object" "lab_hosts" {
  module      = "firewall"
  controller  = "alias"
  reconfigure = "firewall/alias/reconfigure"   # POSTed after every write
  body = jsonencode({
    name    = "lab_hosts"
    type    = "host"
    content = "10.0.0.1"
    enabled = "1"
  })
}

# Settings singleton (Unbound general): get / set, no uuid, destroy is a no-op.
resource "opnsense_object" "unbound" {
  module      = "unbound"
  controller  = "general"
  singleton   = true
  reconfigure = "unbound/service/reconfigure"
  body        = jsonencode({ enabled = "1" })
}
```

**Manage-declared-only / 0-diff imports.** `body` declares *only* the keys you
manage. State holds the full device object; a plan modifier suppresses the diff
when every declared key already matches the device, so:

- importing an existing object (`tofu import` / `import {}` block) lands at
  **0-diff** with no apply against the firewall, and
- the provider never clobbers device fields you didn't declare.

| Attribute | | Meaning |
|-----------|---|---------|
| `module` | required, ForceNew | API module — first path segment under `/api` (`firewall`, `unbound`, …) |
| `controller` | required, ForceNew | API controller; also the JSON envelope key wrapping `body` |
| `body` | required | JSON object of the keys you manage |
| `singleton` | optional (default `false`), ForceNew | `true` → settings `get`/`set` (no uuid); `false` → collection `addItem`/`getItem`/`setItem`/`delItem` |
| `reconfigure` | optional | command path POSTed after every write to apply (e.g. `firewall/alias/reconfigure`) |
| `uuid` | computed | server-assigned UUID of a collection item (empty for a singleton) |
| `id` | computed | `<module>/<controller>` (singleton) or `<module>/<controller>/<uuid>` (item) |

**Import id**: `<module>/<controller>/<uuid>` for a collection item,
`<module>/<controller>` for a singleton; append `|<reconfigure>` to carry the
apply command into imported state. Examples:
`firewall/alias/<uuid>|firewall/alias/reconfigure`,
`unbound/general|unbound/service/reconfigure`.

### `opnsense_object` (data source)

```hcl
data "opnsense_object" "aliases" { path = "firewall/alias/searchItem" }  # .response is raw JSON
```

## Provider configuration

```hcl
terraform {
  required_providers {
    opnsense = { source = "registry.terraform.io/jamesonrgrieve/opnsense" }
  }
}

provider "opnsense" {
  host     = "192.168.7.9"      # no scheme
  key      = var.opnsense_key
  secret   = var.opnsense_secret # sensitive
  insecure = true                # OPNsense self-signed cert (default true)
}
```

## Local build / dev install

```sh
make build          # -> terraform-provider-opnsense
make install        # installs to $DEV_BIN_DIR for a dev_overrides .tfrc
make check          # tidy + fmt + vet + test + build (pre-commit / CI gate)
```

For runners without registry access, install into a filesystem mirror:
`<plugins>/registry.terraform.io/JamesonRGrieve/tofu-opnsense/<ver>/<os>_<arch>/terraform-provider-opnsense`
and point a `.terraformrc` `provider_installation { filesystem_mirror {...} }` at it.

## License

AGPL-3.0-or-later.
