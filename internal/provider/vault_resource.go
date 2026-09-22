package provider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/mapdefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/Kikuvi-Inc/subako-terraform-provider/internal/client"
)

var (
	_ resource.ResourceWithConfigure   = (*vaultResource)(nil)
	_ resource.ResourceWithImportState = (*vaultResource)(nil)
)

func newVaultResource() resource.Resource {
	return &vaultResource{}
}

// vaultResource is a vault: the credentials a session presents to the MCP
// servers it reaches. Vaults usually belong to end users and are made at
// run time; one Terraform manages is one every session shares.
type vaultResource struct {
	client *client.Client
}

type vaultModel struct {
	ID          types.String `tfsdk:"id"`
	WorkspaceID types.String `tfsdk:"workspace_id"`
	DisplayName types.String `tfsdk:"display_name"`
	Metadata    types.Map    `tfsdk:"metadata"`
}

func (r *vaultResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_vault"
}

func (r *vaultResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		Description: "A vault of credentials a session can be given. Deleting it purges its credentials.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				Computed:      true,
				PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"workspace_id": schema.StringAttribute{
				Computed:      true,
				PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"display_name": schema.StringAttribute{
				Required: true,
				// CODESYNC(display-name-len)
				Validators: []validator.String{charsAtMost(200)},
			},
			"metadata": schema.MapAttribute{
				Optional:    true,
				Computed:    true,
				ElementType: types.StringType,
				Default:     mapdefault.StaticValue(types.MapValueMust(types.StringType, nil)),
				Description: "Labels mapping the vault back to your own records.",
			},
		},
	}
}

func (r *vaultResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	r.client = clientFrom(req.ProviderData, &resp.Diagnostics)
}

// vaultLabels reads a vault's metadata as the map of strings this resource
// manages. The API takes any JSON at all there, so what another client left
// may be no such map, and the error then says what it is instead.
func vaultLabels(metadata json.RawMessage) (map[string]string, error) {
	var stored map[string]json.RawMessage
	if err := json.Unmarshal(metadata, &stored); err != nil {
		return nil, errors.New("it is not a JSON object")
	}
	labels := make(map[string]string, len(stored))
	for key, value := range stored {
		var label string
		if err := json.Unmarshal(value, &label); err != nil {
			return nil, fmt.Errorf("it holds a value under %s that is not a string", key)
		}
		labels[key] = label
	}
	return labels, nil
}

// setVault writes what the server holds into m. Metadata this resource
// cannot hold leaves the map out of the state and is reported rather than
// refused, so such a vault can still be planned, refreshed, and destroyed;
// the next apply writes what the configuration names over it.
func setVault(ctx context.Context, m *vaultModel, v *client.Vault) diag.Diagnostics {
	var diags diag.Diagnostics
	m.WorkspaceID = types.StringValue(v.WorkspaceID)
	m.DisplayName = types.StringValue(v.DisplayName)

	labels, err := vaultLabels(v.Metadata)
	if err != nil {
		diags.AddAttributeWarning(path.Root("metadata"), "Unreadable vault metadata",
			"The metadata of vault "+m.ID.ValueString()+" is left out of the state: "+err.Error()+
				". An apply writes what the configuration names over it.")
		m.Metadata = types.MapNull(types.StringType)
		return diags
	}
	metadata, d := types.MapValueFrom(ctx, types.StringType, labels)
	diags.Append(d...)
	m.Metadata = metadata
	return diags
}

// read refreshes m from the server. found is false when the vault is gone.
func (r *vaultResource) read(ctx context.Context, m *vaultModel) (found bool, diags diag.Diagnostics) {
	id := m.ID.ValueString()
	vault, err := r.client.GetVault(ctx, id)
	if client.IsNotFound(err) {
		return false, diags
	}
	if err != nil {
		apiError(&diags, "read vault "+id, err)
		return true, diags
	}
	diags.Append(setVault(ctx, m, vault)...)
	return true, diags
}

func (r *vaultResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan vaultModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	metadata, diags := plannedLabels(ctx, plan.Metadata)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}
	id, err := r.client.CreateVault(ctx, plan.DisplayName.ValueString(), metadata)
	if err != nil {
		apiError(&resp.Diagnostics, "create vault", err)
		return
	}
	// Recorded before the read-back, so a failure there taints this vault
	// rather than orphaning it.
	created := vaultModel{
		ID:          types.StringValue(id),
		WorkspaceID: types.StringNull(),
		DisplayName: plan.DisplayName,
		Metadata:    plan.Metadata,
	}
	next := plan
	next.ID = created.ID
	finishCreate(ctx, resp, subject{"Vault", id}, r.read, next, created)
}

func (r *vaultResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	refresh(ctx, req, resp, r.read)
}

func (r *vaultResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan, state vaultModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	metadata, diags := plannedLabels(ctx, plan.Metadata)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}
	id := state.ID.ValueString()
	run(&resp.Diagnostics, step{
		action: "update vault " + id,
		call: func() error {
			_, err := r.client.UpdateVault(ctx, id, plan.DisplayName.ValueString(), metadata)
			return err
		},
	})

	next := plan
	next.ID = state.ID
	finishUpdate(ctx, resp, subject{"Vault", id}, r.read, next, state)
}

func (r *vaultResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state vaultModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	id := state.ID.ValueString()
	remove(resp, "delete vault "+id, func() error { return r.client.DeleteVault(ctx, id) })
}

func (r *vaultResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	resource.ImportStatePassthroughID(ctx, path.Root("id"), req, resp)
}
