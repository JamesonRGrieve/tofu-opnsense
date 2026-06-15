// SPDX-License-Identifier: AGPL-3.0-or-later

package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
	"strings"

	"github.com/JamesonRGrieve/tofu-opnsense/internal/opnsense"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/booldefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

var (
	_ resource.Resource                = (*objectResource)(nil)
	_ resource.ResourceWithConfigure   = (*objectResource)(nil)
	_ resource.ResourceWithImportState = (*objectResource)(nil)
)

// NewObjectResource constructs the generic opnsense_object resource.
func NewObjectResource() resource.Resource { return &objectResource{} }

type objectResource struct {
	client *opnsense.Client
}

// objectModel is the state/plan shape for opnsense_object.
type objectModel struct {
	ID          types.String `tfsdk:"id"`
	Module      types.String `tfsdk:"module"`
	Controller  types.String `tfsdk:"controller"`
	UUID        types.String `tfsdk:"uuid"`
	Singleton   types.Bool   `tfsdk:"singleton"`
	Reconfigure types.String `tfsdk:"reconfigure"`
	ItemSuffix  types.String `tfsdk:"item_suffix"`
	SetCommand  types.String `tfsdk:"set_command"`
	Envelope    types.String `tfsdk:"envelope"`
	Body        types.String `tfsdk:"body"`
}

// bareSuffix is the sentinel item_suffix value selecting the empty/bare verb
// family (add/get/set/del with no noun). A handful of controllers — os-acme
// (add/get/update/del) and core IPsec VTI (add/get/set/del) — name their verbs
// with no noun at all, which the default-to-"Item" rule cannot express.
const bareSuffix = "none"

// itemSuffix is the collection-item command noun (default "Item"), so the verbs
// become add<Suffix>/get<Suffix>/set<Suffix>/del<Suffix>. The base model uses
// "Item"; OPNsense plugins like os-haproxy (addServer/getServer/…) and
// os-acme-client (addCertificate/…) use a bespoke noun instead. The sentinel
// "none" selects bare verbs (add/get/set/del).
func itemSuffix(m objectModel) string {
	s := m.ItemSuffix.ValueString()
	switch s {
	case "":
		return "Item"
	case bareSuffix:
		return ""
	default:
		return s
	}
}

// setVerb is the update command. It is `set<suffix>` by default, but a few
// controllers diverge — os-acme uses `update` (no suffix) where the rest use
// `set`. `set_command`, when set, overrides the whole verb verbatim.
func setVerb(m objectModel) string {
	if v := m.SetCommand.ValueString(); v != "" {
		return v
	}
	return "set" + itemSuffix(m)
}

// envelopeKey is the JSON wrap/unwrap key. For the base model it equals the
// controller (`{"alias":{…}}`); plugin items envelope under the item noun, not
// the controller (os-haproxy `addServer` under `haproxy/settings` expects
// `{"server":{…}}`), so `envelope` overrides it when set.
func envelopeKey(m objectModel) string {
	if e := m.Envelope.ValueString(); e != "" {
		return e
	}
	return m.Controller.ValueString()
}

func (r *objectResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_object"
}

func (r *objectResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "A generic OPNsense API resource addressed by `module`/`controller`. " +
			"Covers the standard model CRUD pattern (`addItem`/`getItem`/`setItem`/`delItem` plus a " +
			"`reconfigure`/apply call) for collection items such as `firewall`/`alias`, `firewall`/`filter` (rule), " +
			"`unbound`/`host_override`; and the settings-singleton `get`/`set` pattern (`singleton = true`) for " +
			"controllers like `unbound`/`general` or `firewall`/`settings`. " +
			"`body` declares only the keys this resource manages; device-returned keys outside `body` are ignored " +
			"for drift, so a subset declaration imports to 0-diff and never clobbers unmanaged fields.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "Resource id — `<module>/<controller>` for a singleton, `<module>/<controller>/<uuid>` for a collection item.",
				PlanModifiers:       []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"module": schema.StringAttribute{
				Required:            true,
				MarkdownDescription: "API module, e.g. `firewall`, `unbound`, `interfaces`. First path segment under `/api`.",
				PlanModifiers:       []planmodifier.String{stringplanmodifier.RequiresReplace()},
			},
			"controller": schema.StringAttribute{
				Required: true,
				MarkdownDescription: "API controller, e.g. `alias`, `filter`, `host_override`, `general`. " +
					"Also the JSON envelope key used to wrap `body` on write and to unwrap the device object on read.",
				PlanModifiers: []planmodifier.String{stringplanmodifier.RequiresReplace()},
			},
			"uuid": schema.StringAttribute{
				Computed: true,
				MarkdownDescription: "Server-assigned UUID of a collection item (empty for a `singleton`). " +
					"Captured from the `addItem` response on create.",
				PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"singleton": schema.BoolAttribute{
				Optional: true,
				Computed: true,
				Default:  booldefault.StaticBool(false),
				MarkdownDescription: "When true this is a settings singleton: create/update via `<module>/<controller>/set` and " +
					"read via `<module>/<controller>/get` (no UUID); destroy is a no-op. When false (default) it is a collection " +
					"item driven by `addItem`/`getItem/<uuid>`/`setItem/<uuid>`/`delItem/<uuid>`.",
				PlanModifiers: []planmodifier.Bool{requiresReplaceBool{}},
			},
			"reconfigure": schema.StringAttribute{
				Optional: true,
				MarkdownDescription: "Command path (relative to `/api`) to POST after every write to apply the change, " +
					"e.g. `firewall/alias/reconfigure` or `unbound/service/reconfigure`. Omit to skip the apply step.",
			},
			"item_suffix": schema.StringAttribute{
				Optional: true,
				MarkdownDescription: "Collection-item command noun (default `Item`). The CRUD verbs become " +
					"`add<suffix>`/`get<suffix>`/`set<suffix>`/`del<suffix>`. OPNsense plugins use bespoke nouns " +
					"instead of the base-model `Item`: os-haproxy → `Server`/`Backend`/`Frontend`/`Acl`/`Action`. " +
					"The sentinel `none` selects bare verbs (`add`/`get`/`set`/`del`, no noun) for controllers like " +
					"os-acme and core IPsec VTI. Ignored for `singleton`.",
			},
			"set_command": schema.StringAttribute{
				Optional: true,
				MarkdownDescription: "Override for the update verb (default `set<suffix>`). os-acme uses `update` " +
					"where the rest of OPNsense uses `set`; set `set_command = \"update\"` for those controllers.",
			},
			"envelope": schema.StringAttribute{
				Optional: true,
				MarkdownDescription: "JSON wrap/unwrap key for `body` (default: `controller`). The base model envelopes " +
					"under the controller (`{\"alias\":{…}}`); plugin items envelope under the item noun, not the " +
					"controller — os-haproxy `addServer` (controller `settings`) expects `{\"server\":{…}}`, so set " +
					"`envelope = \"server\"`.",
			},
			"body": schema.StringAttribute{
				Required: true,
				MarkdownDescription: "JSON object of the declared (managed) fields. Sent (wrapped in the `controller` envelope) " +
					"on create/update; state holds the full device object and drift is detected only on these keys.",
				PlanModifiers: []planmodifier.String{subsetSuppress{}},
			},
		},
	}
}

func (r *objectResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
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

// cmdPath builds an /api command path: /<module>/<controller>/<command>[/<uuid>].
func cmdPath(module, controller, command, uuid string) string {
	p := "/" + strings.Trim(module, "/") + "/" + strings.Trim(controller, "/") + "/" + command
	if uuid != "" {
		p += "/" + uuid
	}
	return p
}

// wrap envelopes the declared body under the controller key, as OPNsense
// addItem/setItem/set expect: {"<controller>": {...body}}.
func wrap(controller string, body []byte) ([]byte, error) {
	var inner json.RawMessage = body
	return json.Marshal(map[string]json.RawMessage{controller: inner})
}

// unwrap extracts the {"<controller>": {...}} envelope returned by
// getItem/get. Returns (object, true) on success, ("", false) when the
// envelope is absent or the node is empty/missing (item gone).
func unwrap(controller string, raw []byte) (string, bool) {
	var env map[string]json.RawMessage
	if json.Unmarshal(raw, &env) != nil {
		return "", false
	}
	node, ok := env[controller]
	if !ok {
		return "", false
	}
	// getBase returns [] (empty object) for a missing UUID; treat an empty
	// object as "not found" so a deleted item is removed from state.
	var obj map[string]json.RawMessage
	if json.Unmarshal(node, &obj) != nil || len(obj) == 0 {
		return "", false
	}
	compact, err := compactJSON(node)
	if err != nil {
		return "", false
	}
	return compact, true
}

// apiResult is the common OPNsense write-command response envelope.
type apiResult struct {
	Result      string                     `json:"result"`
	UUID        string                     `json:"uuid"`
	Validations map[string]json.RawMessage `json:"validations"`
}

// checkResult parses a write-command response and returns an error when the
// API reports a failure or validation errors. uuid is the captured item id.
func checkResult(op string, raw []byte) (string, error) {
	var res apiResult
	if err := json.Unmarshal(raw, &res); err != nil {
		return "", fmt.Errorf("opnsense %s: invalid JSON response: %s", op, string(raw))
	}
	switch res.Result {
	case "saved", "deleted", "ok":
		return res.UUID, nil
	case "":
		// Some action commands (reconfigure) return {"status":"ok"} with no
		// "result" key — accept an absent result as success.
		return res.UUID, nil
	default:
		if len(res.Validations) > 0 {
			vs, _ := json.Marshal(res.Validations)
			return "", fmt.Errorf("opnsense %s failed: %s (validations: %s)", op, res.Result, string(vs))
		}
		return "", fmt.Errorf("opnsense %s failed: result=%q (%s)", op, res.Result, string(raw))
	}
}

// reconfigure POSTs the configured apply command, if any.
func (r *objectResource) reconfigure(m objectModel) error {
	if m.Reconfigure.IsNull() || m.Reconfigure.ValueString() == "" {
		return nil
	}
	p := "/" + strings.Trim(m.Reconfigure.ValueString(), "/")
	if _, err := r.client.Post(p, nil); err != nil {
		return fmt.Errorf("reconfigure %s: %w", p, err)
	}
	return nil
}

func (r *objectResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var m objectModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &m)...)
	if resp.Diagnostics.HasError() {
		return
	}
	body := []byte(m.Body.ValueString())
	if !json.Valid(body) {
		resp.Diagnostics.AddError("Invalid body", "`body` must be valid JSON")
		return
	}
	module, controller := m.Module.ValueString(), m.Controller.ValueString()
	payload, err := wrap(envelopeKey(m), body)
	if err != nil {
		resp.Diagnostics.AddError("OPNsense create: failed to build payload", err.Error())
		return
	}

	if m.Singleton.ValueBool() {
		raw, err := r.client.Post(cmdPath(module, controller, "set", ""), payload)
		if err != nil {
			resp.Diagnostics.AddError("OPNsense create failed", err.Error())
			return
		}
		if _, err := checkResult("set", raw); err != nil {
			resp.Diagnostics.AddError("OPNsense create failed", err.Error())
			return
		}
		m.UUID = types.StringValue("")
		m.ID = types.StringValue(module + "/" + controller)
	} else {
		addCmd := "add" + itemSuffix(m)
		raw, err := r.client.Post(cmdPath(module, controller, addCmd, ""), payload)
		if err != nil {
			resp.Diagnostics.AddError("OPNsense create failed", err.Error())
			return
		}
		uuid, err := checkResult(addCmd, raw)
		if err != nil {
			resp.Diagnostics.AddError("OPNsense create failed", err.Error())
			return
		}
		if uuid == "" {
			resp.Diagnostics.AddError("OPNsense create failed", addCmd+" returned no uuid: "+string(raw))
			return
		}
		m.UUID = types.StringValue(uuid)
		m.ID = types.StringValue(module + "/" + controller + "/" + uuid)
	}

	if err := r.reconfigure(m); err != nil {
		resp.Diagnostics.AddError("OPNsense reconfigure failed", err.Error())
		return
	}
	// Store the declared body verbatim so create plan/state are consistent;
	// the next refresh (Read) replaces it with the full device object.
	resp.Diagnostics.Append(resp.State.Set(ctx, &m)...)
}

// collapseOptionFields rewrites OPNsense OptionField objects in a read-back body
// down to the plain value a config body declares. OPNsense returns a select field
// as a map of every option to {value,selected}, e.g.
//
//	{"inet":{"value":"IPv4","selected":1},"inet6":{"value":"IPv6","selected":0}}
//
// while the declared body sends just "inet". Without collapsing, the stored state
// never structurally matches the declared subset, so subsetMatches fails and the
// item is re-written on every apply (harmless for most, fatal where the field is
// required on write). This normalizes the read object so a subset declaration
// reaches a true 0-diff. Non-OptionField objects/arrays are recursed into.
func collapseOptionFields(jsonStr string) string {
	var v any
	if json.Unmarshal([]byte(jsonStr), &v) != nil {
		return jsonStr
	}
	out, err := json.Marshal(collapseValue(v))
	if err != nil {
		return jsonStr
	}
	return string(out)
}

func collapseValue(v any) any {
	switch x := v.(type) {
	case map[string]any:
		if sel, ok := optionFieldSelection(x); ok {
			return sel
		}
		for k, val := range x {
			x[k] = collapseValue(val)
		}
		return x
	case []any:
		for i, val := range x {
			x[i] = collapseValue(val)
		}
		return x
	default:
		return v
	}
}

// optionFieldSelection reports whether m is an OptionField (non-empty, every
// value an object carrying a "selected" flag) and, if so, returns the comma-
// joined selected option keys (sorted for determinism; single-select — the
// dominant case — yields exactly the one key).
func optionFieldSelection(m map[string]any) (string, bool) {
	if len(m) == 0 {
		return "", false
	}
	for _, val := range m {
		obj, ok := val.(map[string]any)
		if !ok {
			return "", false
		}
		if _, has := obj["selected"]; !has {
			return "", false
		}
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	selected := make([]string, 0, len(keys))
	for _, k := range keys {
		if isSelected(m[k].(map[string]any)["selected"]) {
			selected = append(selected, k)
		}
	}
	return strings.Join(selected, ","), true
}

func isSelected(v any) bool {
	switch s := v.(type) {
	case float64:
		return s != 0
	case json.Number:
		return s.String() != "0" && s.String() != ""
	case string:
		return s == "1" || s == "true"
	case bool:
		return s
	}
	return false
}

func (r *objectResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var m objectModel
	resp.Diagnostics.Append(req.State.Get(ctx, &m)...)
	if resp.Diagnostics.HasError() {
		return
	}
	module, controller := m.Module.ValueString(), m.Controller.ValueString()
	var p string
	if m.Singleton.ValueBool() {
		p = cmdPath(module, controller, "get", "")
	} else {
		p = cmdPath(module, controller, "get"+itemSuffix(m), m.UUID.ValueString())
	}
	raw, err := r.client.Get(p)
	if err != nil {
		if opnsense.NotFound(err) {
			resp.State.RemoveResource(ctx)
			return
		}
		resp.Diagnostics.AddError("OPNsense read failed", err.Error())
		return
	}
	obj, ok := unwrap(envelopeKey(m), raw)
	if !ok {
		// Empty/absent envelope — the item no longer exists.
		resp.State.RemoveResource(ctx)
		return
	}
	m.Body = types.StringValue(collapseOptionFields(obj))
	resp.Diagnostics.Append(resp.State.Set(ctx, &m)...)
}

func (r *objectResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var m objectModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &m)...)
	if resp.Diagnostics.HasError() {
		return
	}
	body := []byte(m.Body.ValueString())
	if !json.Valid(body) {
		resp.Diagnostics.AddError("Invalid body", "`body` must be valid JSON")
		return
	}
	module, controller := m.Module.ValueString(), m.Controller.ValueString()
	payload, err := wrap(envelopeKey(m), body)
	if err != nil {
		resp.Diagnostics.AddError("OPNsense update: failed to build payload", err.Error())
		return
	}
	var p, setCmd string
	if m.Singleton.ValueBool() {
		setCmd = "set"
		p = cmdPath(module, controller, "set", "")
	} else {
		setCmd = setVerb(m)
		p = cmdPath(module, controller, setCmd, m.UUID.ValueString())
	}
	raw, err := r.client.Post(p, payload)
	if err != nil {
		resp.Diagnostics.AddError("OPNsense update failed", err.Error())
		return
	}
	if _, err := checkResult(setCmd, raw); err != nil {
		resp.Diagnostics.AddError("OPNsense update failed", err.Error())
		return
	}
	if err := r.reconfigure(m); err != nil {
		resp.Diagnostics.AddError("OPNsense reconfigure failed", err.Error())
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &m)...)
}

func (r *objectResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var m objectModel
	resp.Diagnostics.Append(req.State.Get(ctx, &m)...)
	if resp.Diagnostics.HasError() {
		return
	}
	module, controller := m.Module.ValueString(), m.Controller.ValueString()
	if m.Singleton.ValueBool() {
		// Settings singletons cannot be deleted; just drop from state.
		return
	}
	delCmd := "del" + itemSuffix(m)
	raw, err := r.client.Post(cmdPath(module, controller, delCmd, m.UUID.ValueString()), nil)
	if err != nil {
		if opnsense.NotFound(err) {
			return // already gone
		}
		resp.Diagnostics.AddError("OPNsense delete failed", err.Error())
		return
	}
	if _, err := checkResult(delCmd, raw); err != nil {
		resp.Diagnostics.AddError("OPNsense delete failed", err.Error())
		return
	}
	if err := r.reconfigure(m); err != nil {
		resp.Diagnostics.AddError("OPNsense reconfigure failed", err.Error())
	}
}

func (r *objectResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	// Import id is a slash-delimited address; trailing `|`-fields carry the
	// operational hints (positional) so imported state matches config (→ 0-diff):
	//   <module>/<controller>[/<uuid>][|<reconfigure>[|<item_suffix>[|<envelope>[|<set_command>]]]]
	// A two-segment address (no uuid) is a singleton; three segments is a
	// collection item. Plugin items (os-haproxy/os-acme) pass item_suffix +
	// envelope, e.g. `haproxy/settings/<uuid>|haproxy/service/reconfigure|Server|server`.
	// Body is populated on the following Read.
	pipeParts := strings.Split(req.ID, "|")
	idPart := pipeParts[0]
	pipeField := func(i int) string {
		if i < len(pipeParts) {
			return strings.TrimSpace(pipeParts[i])
		}
		return ""
	}
	reconf, itemSfx, envel, setCmd := pipeField(1), pipeField(2), pipeField(3), pipeField(4)
	segs := strings.Split(strings.Trim(idPart, "/"), "/")
	if len(segs) < 2 || len(segs) > 3 {
		resp.Diagnostics.AddError("Invalid import id",
			"expected `<module>/<controller>` (singleton) or `<module>/<controller>/<uuid>` (item), "+
				"optionally `|<reconfigure>`; got: "+req.ID)
		return
	}
	module, controller := segs[0], segs[1]
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("module"), module)...)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("controller"), controller)...)
	if len(segs) == 3 {
		uuid := segs[2]
		resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("uuid"), uuid)...)
		resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("singleton"), false)...)
		resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("id"), module+"/"+controller+"/"+uuid)...)
	} else {
		resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("uuid"), "")...)
		resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("singleton"), true)...)
		resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("id"), module+"/"+controller)...)
	}
	setStrOrNull := func(attr, val string) {
		if val != "" {
			resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root(attr), val)...)
		} else {
			resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root(attr), types.StringNull())...)
		}
	}
	setStrOrNull("reconfigure", reconf)
	setStrOrNull("item_suffix", itemSfx)
	setStrOrNull("set_command", setCmd)
	setStrOrNull("envelope", envel)
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("body"), "{}")...)
}

// ---------------------------------------------------------------------------
// requiresReplaceBool — RequiresReplace for the singleton flag (it selects the
// command family; flipping it changes the resource identity).
// ---------------------------------------------------------------------------

type requiresReplaceBool struct{}

func (requiresReplaceBool) Description(context.Context) string {
	return "Changing `singleton` forces resource replacement."
}
func (requiresReplaceBool) MarkdownDescription(context.Context) string {
	return (requiresReplaceBool{}).Description(nil)
}
func (requiresReplaceBool) PlanModifyBool(_ context.Context, req planmodifier.BoolRequest, resp *planmodifier.BoolResponse) {
	if req.StateValue.IsNull() || req.PlanValue.IsNull() || req.PlanValue.IsUnknown() {
		return
	}
	if req.StateValue.ValueBool() != req.PlanValue.ValueBool() {
		resp.RequiresReplace = true
	}
}

// ---------------------------------------------------------------------------
// subset plan modifier — suppress diff when every declared key already matches
// the full device object held in prior state. This is what lets a subset
// `body` import/refresh to 0-diff without clobbering unmanaged device fields.
// ---------------------------------------------------------------------------

type subsetSuppress struct{}

func (subsetSuppress) Description(context.Context) string {
	return "Suppress diff when all declared JSON keys already match the device object in state."
}
func (subsetSuppress) MarkdownDescription(context.Context) string {
	return (subsetSuppress{}).Description(nil)
}

func (subsetSuppress) PlanModifyString(_ context.Context, req planmodifier.StringRequest, resp *planmodifier.StringResponse) {
	if req.StateValue.IsNull() || req.StateValue.IsUnknown() {
		return // create — nothing to reconcile against
	}
	if req.ConfigValue.IsNull() || req.ConfigValue.IsUnknown() {
		return
	}
	// All declared (config) keys already match the device object in prior state:
	// keep the full prior object and show no diff. Otherwise leave the planned
	// (config) value in place so the drift surfaces as an update.
	if subsetMatches(req.StateValue.ValueString(), req.ConfigValue.ValueString()) {
		resp.PlanValue = req.StateValue
	}
}

// subsetMatches reports whether every top-level key in the config JSON object
// is present in the prior JSON object with a structurally-equal value (config
// is a value-subset of prior). Invalid JSON on either side returns false so the
// caller falls back to a normal diff.
func subsetMatches(prior, cfg string) bool {
	var p, c map[string]json.RawMessage
	if json.Unmarshal([]byte(prior), &p) != nil {
		return false
	}
	if json.Unmarshal([]byte(cfg), &c) != nil {
		return false
	}
	for k, cv := range c {
		pv, ok := p[k]
		if !ok || !jsonEqual(cv, pv) {
			return false
		}
	}
	return true
}

// jsonEqual compares two raw JSON values structurally (order-insensitive).
func jsonEqual(a, b json.RawMessage) bool {
	var av, bv any
	if json.Unmarshal(a, &av) != nil || json.Unmarshal(b, &bv) != nil {
		return false
	}
	return reflect.DeepEqual(av, bv)
}

// compactJSON re-serializes raw JSON in compact, key-sorted-by-encoder form.
func compactJSON(raw []byte) (string, error) {
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return "", err
	}
	out, err := json.Marshal(v)
	if err != nil {
		return "", err
	}
	return string(out), nil
}
