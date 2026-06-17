// SPDX-License-Identifier: AGPL-3.0-or-later

package provider

import (
	"context"
	"fmt"
	"strings"

	"github.com/JamesonRGrieve/tofu-opnsense/internal/opnsense"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

var (
	_ resource.Resource              = (*reconcileResource)(nil)
	_ resource.ResourceWithConfigure = (*reconcileResource)(nil)
)

// NewReconcileResource constructs the opnsense_reconcile resource: an
// unconditional apply that POSTs a set of reconfigure/apply command paths on
// every run. It manages no remote object — it exists to heal config-vs-live
// drift Terraform cannot detect. The provider tracks config.xml (what it reads
// back via the API), not the live pf/service state, so a plan with 0 object
// changes never re-applies and a config-only edit (manual change without Apply,
// an offline/partial adoption, a reboot loading stale state) silently lingers.
// Pairing this with a `triggers` map holding `timestamp()` forces an apply every
// run, reconciling live state to config. Use ONLY for seamless reloads
// (filter/alias/DNS/DHCP/HAProxy); never for link/tunnel re-inits.
func NewReconcileResource() resource.Resource { return &reconcileResource{} }

type reconcileResource struct {
	client *opnsense.Client
}

type reconcileModel struct {
	ID        types.String `tfsdk:"id"`
	Endpoints types.List   `tfsdk:"endpoints"`
	Triggers  types.Map    `tfsdk:"triggers"`
}

func (r *reconcileResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_reconcile"
}

func (r *reconcileResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "Unconditional reconcile/apply. POSTs each `/api`-relative command path in " +
			"`endpoints` (e.g. `firewall/filter/apply`, `firewall/alias/reconfigure`, `unbound/service/reconfigure`) " +
			"on every create/update — it manages no remote object. Pair with a `triggers` map containing " +
			"`timestamp()` so it re-applies on every run, healing config-vs-live drift Terraform cannot detect " +
			"(the provider tracks config, not the live ruleset, so a 0-change plan otherwise never re-applies). " +
			"Use ONLY for seamless reloads (filter/alias/DNS/DHCP/HAProxy); NEVER for link/tunnel re-inits " +
			"(interface/VLAN/WireGuard/IPsec) — those can drop the management path mid-apply.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "Static resource id (`reconcile`).",
				PlanModifiers:       []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"endpoints": schema.ListAttribute{
				Required:            true,
				ElementType:         types.StringType,
				MarkdownDescription: "Ordered list of `/api`-relative command paths to POST (parameterless apply/reconfigure calls).",
			},
			"triggers": schema.MapAttribute{
				Optional:            true,
				ElementType:         types.StringType,
				MarkdownDescription: "Arbitrary key/value map; any change re-runs the apply. Set a key to `timestamp()` to fire every run.",
			},
		},
	}
}

func (r *reconcileResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	if req.ProviderData == nil {
		return
	}
	client, ok := req.ProviderData.(*opnsense.Client)
	if !ok {
		resp.Diagnostics.AddError("Unexpected provider data",
			fmt.Sprintf("expected *opnsense.Client, got %T", req.ProviderData))
		return
	}
	r.client = client
}

// runReconcile POSTs each endpoint in order via post and returns one warning
// string per failed endpoint plus allFailed=true when every endpoint failed.
// A per-endpoint failure is tolerated (best-effort: an optional service may be
// absent on a given box); total failure means the device is unreachable or
// every path is wrong, which the caller escalates to an error. Pure (post is
// injected) so the aggregation is unit-testable without a live device.
func runReconcile(endpoints []string, post func(path string) ([]byte, error)) (warnings []string, allFailed bool) {
	failed := 0
	for _, ep := range endpoints {
		p := "/" + strings.Trim(ep, "/")
		raw, err := post(p)
		if err == nil {
			_, err = checkResult("reconcile "+ep, raw)
		}
		if err != nil {
			failed++
			warnings = append(warnings, fmt.Sprintf("%s: %s", p, err.Error()))
		}
	}
	return warnings, len(endpoints) > 0 && failed == len(endpoints)
}

func (r *reconcileResource) apply(ctx context.Context, m reconcileModel, diags *diag.Diagnostics) {
	var eps []string
	diags.Append(m.Endpoints.ElementsAs(ctx, &eps, false)...)
	if diags.HasError() {
		return
	}
	warnings, allFailed := runReconcile(eps, func(p string) ([]byte, error) { return r.client.Post(p, nil) })
	for _, w := range warnings {
		diags.AddWarning("OPNsense reconcile endpoint failed", w)
	}
	if allFailed {
		diags.AddError("OPNsense reconcile failed",
			"every reconcile endpoint failed — the device is likely unreachable or all command paths are invalid")
	}
}

func (r *reconcileResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var m reconcileModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &m)...)
	if resp.Diagnostics.HasError() {
		return
	}
	r.apply(ctx, m, &resp.Diagnostics)
	if resp.Diagnostics.HasError() {
		return
	}
	m.ID = types.StringValue("reconcile")
	resp.Diagnostics.Append(resp.State.Set(ctx, &m)...)
}

func (r *reconcileResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	// No remote object to read; keep prior state verbatim.
	var m reconcileModel
	resp.Diagnostics.Append(req.State.Get(ctx, &m)...)
	if resp.Diagnostics.HasError() {
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &m)...)
}

func (r *reconcileResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var m reconcileModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &m)...)
	if resp.Diagnostics.HasError() {
		return
	}
	r.apply(ctx, m, &resp.Diagnostics)
	if resp.Diagnostics.HasError() {
		return
	}
	m.ID = types.StringValue("reconcile")
	resp.Diagnostics.Append(resp.State.Set(ctx, &m)...)
}

func (r *reconcileResource) Delete(_ context.Context, _ resource.DeleteRequest, _ *resource.DeleteResponse) {
	// Manages no remote object — nothing to delete.
}
