package provider

import (
	"context"

	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/subako-ai/subako-terraform-provider/internal/bundle"
	"github.com/subako-ai/subako-terraform-provider/internal/client"
)

var (
	_ resource.ResourceWithConfigure   = (*skillResource)(nil)
	_ resource.ResourceWithImportState = (*skillResource)(nil)
	_ resource.ResourceWithModifyPlan  = (*skillResource)(nil)
)

func newSkillResource() resource.Resource {
	return &skillResource{}
}

// skillResource is a skill whose latest version holds a local directory's
// content. A change to any file pushes a new version; versions are immutable,
// so the skill is the unit Terraform owns.
type skillResource struct {
	client *client.Client
}

type skillModel struct {
	ID            types.String `tfsdk:"id"`
	SourceDir     types.String `tfsdk:"source_dir"`
	DisplayName   types.String `tfsdk:"display_name"`
	Name          types.String `tfsdk:"name"`
	ContentHash   types.String `tfsdk:"content_hash"`
	LatestVersion types.Int64  `tfsdk:"latest_version"`
}

func (r *skillResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_skill"
}

func (r *skillResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		Description: "A skill uploaded from a local directory holding `SKILL.md`. Changing any file " +
			"in the directory pushes a new skill version.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				Computed:      true,
				PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"source_dir": schema.StringAttribute{
				Required:    true,
				Description: "The bundle directory: `SKILL.md` at its root, and every file under it uploaded.",
			},
			"display_name": schema.StringAttribute{
				Optional:    true,
				Computed:    true,
				Description: "A label shown in place of the frontmatter name. Left unset, Terraform does not manage it.",
				// CODESYNC(display-name-len)
				Validators:    []validator.String{charsAtMost(200)},
				PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"name": schema.StringAttribute{
				Computed:    true,
				Description: "The `name` the latest version's frontmatter declares.",
			},
			"content_hash": schema.StringAttribute{
				Computed: true,
				Description: "A fingerprint of the latest version's files. Terraform compares it with " +
					"the directory's to decide whether to push.",
			},
			"latest_version": schema.Int64Attribute{
				Computed: true,
			},
		},
	}
}

func (r *skillResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	r.client = clientFrom(req.ProviderData, &resp.Diagnostics)
}

// ModifyPlan fingerprints the directory, so a change to its files is a
// change to the plan. An unchanged fingerprint keeps what the version
// reported; a changed one leaves it to the push. Terraform proposes the prior
// computed values here, so a push has to mark them unknown itself.
func (r *skillResource) ModifyPlan(ctx context.Context, req resource.ModifyPlanRequest, resp *resource.ModifyPlanResponse) {
	if req.Plan.Raw.IsNull() {
		return
	}
	var plan skillModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() || plan.SourceDir.IsUnknown() {
		return
	}
	local, err := bundle.Read(plan.SourceDir.ValueString())
	if err != nil {
		resp.Diagnostics.AddAttributeError(path.Root("source_dir"), "Unreadable skill bundle", err.Error())
		return
	}
	plan.ContentHash = types.StringValue(local.Hash())
	plan.Name = types.StringUnknown()
	plan.LatestVersion = types.Int64Unknown()

	if !req.State.Raw.IsNull() {
		var state skillModel
		resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
		if resp.Diagnostics.HasError() {
			return
		}
		if plan.ContentHash.Equal(state.ContentHash) {
			plan.Name = state.Name
			plan.LatestVersion = state.LatestVersion
		}
	}
	resp.Diagnostics.Append(resp.Plan.Set(ctx, &plan)...)
}

func (r *skillResource) read(ctx context.Context, m *skillModel) (found bool, diags diag.Diagnostics) {
	id := m.ID.ValueString()
	skill, err := r.client.GetSkill(ctx, id)
	if client.IsNotFound(err) {
		return false, diags
	}
	if err != nil {
		apiError(&diags, "read skill "+id, err)
		return true, diags
	}
	version, err := r.client.LatestSkillVersion(ctx, id)
	if err != nil {
		apiError(&diags, "read skill "+id+" versions", err)
		return true, diags
	}
	m.DisplayName = types.StringValue(skill.DisplayName)
	m.Name = types.StringValue(skill.Name)
	m.LatestVersion = types.Int64Value(skill.LatestVersion)
	m.ContentHash = types.StringNull()
	if version != nil {
		entries := make([]bundle.Entry, len(version.Manifest))
		for i, e := range version.Manifest {
			entries[i] = bundle.Entry{Path: e.Path, Sha256: e.Sha256}
		}
		m.ContentHash = types.StringValue(bundle.Hash(entries))
	}
	return true, diags
}

func pack(dir string, diags *diag.Diagnostics) []byte {
	local, err := bundle.Read(dir)
	if err != nil {
		diags.AddAttributeError(path.Root("source_dir"), "Unreadable skill bundle", err.Error())
		return nil
	}
	archive, err := local.Pack()
	if err != nil {
		diags.AddAttributeError(path.Root("source_dir"), "Unpackable skill bundle", err.Error())
		return nil
	}
	return archive
}

// rename gives a skill its display name.
func (r *skillResource) rename(ctx context.Context, id, displayName string) step {
	return step{
		action: "name skill " + id,
		call: func() error {
			_, err := r.client.RenameSkill(ctx, id, displayName)
			return err
		},
	}
}

func (r *skillResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan skillModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	archive := pack(plan.SourceDir.ValueString(), &resp.Diagnostics)
	if resp.Diagnostics.HasError() {
		return
	}
	version, err := r.client.CreateSkill(ctx, archive)
	if err != nil {
		apiError(&resp.Diagnostics, "create skill", err)
		return
	}
	// Recorded before the steps below, so a failure there taints this skill
	// rather than orphaning it.
	created := skillModel{
		ID:            types.StringValue(version.SkillID),
		SourceDir:     plan.SourceDir,
		DisplayName:   types.StringNull(),
		Name:          types.StringNull(),
		ContentHash:   types.StringNull(),
		LatestVersion: types.Int64Null(),
	}

	if displayName, ok := plannedString(plan.DisplayName).Get(); ok {
		run(&resp.Diagnostics, r.rename(ctx, version.SkillID, displayName))
	}

	next := plan
	next.ID = created.ID
	finishCreate(ctx, resp, subject{"Skill", version.SkillID}, r.read, next, created)
}

func (r *skillResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	refresh(ctx, req, resp, r.read)
}

func (r *skillResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan, state skillModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	id := state.ID.ValueString()

	var steps []step
	if !plan.ContentHash.Equal(state.ContentHash) {
		archive := pack(plan.SourceDir.ValueString(), &resp.Diagnostics)
		if resp.Diagnostics.HasError() {
			return
		}
		steps = append(steps, step{
			action: "push skill " + id + " version",
			call: func() error {
				_, err := r.client.PushSkillVersion(ctx, id, archive)
				return err
			},
		})
	}
	wanted := plannedString(plan.DisplayName)
	if displayName, ok := wanted.Get(); ok && !wanted.Equal(plannedString(state.DisplayName)) {
		steps = append(steps, r.rename(ctx, id, displayName))
	}
	run(&resp.Diagnostics, steps...)

	next := plan
	next.ID = state.ID
	finishUpdate(ctx, resp, subject{"Skill", id}, r.read, next, state)
}

func (r *skillResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state skillModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	id := state.ID.ValueString()
	remove(resp, "delete skill "+id+
		" (a skill cannot be deleted while any published agent version or live session grants it)",
		func() error { return r.client.DeleteSkill(ctx, id) })
}

func (r *skillResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	resource.ImportStatePassthroughID(ctx, path.Root("id"), req, resp)
}
