package provider

import (
	"context"

	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/datasource/schema"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/Kikuvi-Inc/subako-terraform-provider/internal/client"
)

var (
	_ datasource.DataSourceWithConfigure = (*modelProvidersDataSource)(nil)
	_ datasource.DataSourceWithConfigure = (*workspaceDataSource)(nil)
)

func newModelProvidersDataSource() datasource.DataSource {
	return &modelProvidersDataSource{}
}

// modelProvidersDataSource lists the model providers a workspace can use:
// the platform's, with their model catalogs, and the workspace's own.
type modelProvidersDataSource struct {
	client *client.Client
}

type modelProvidersModel struct {
	Providers []modelProviderEntry `tfsdk:"providers"`
}

type modelProviderEntry struct {
	ID          types.String         `tfsdk:"id"`
	Type        types.String         `tfsdk:"type"`
	Format      types.String         `tfsdk:"format"`
	DisplayName types.String         `tfsdk:"display_name"`
	Description types.String         `tfsdk:"description"`
	Models      []platformModelEntry `tfsdk:"models"`
}

type platformModelEntry struct {
	Model       types.String `tfsdk:"model"`
	DisplayName types.String `tfsdk:"display_name"`
	Description types.String `tfsdk:"description"`
}

func (d *modelProvidersDataSource) Metadata(_ context.Context, req datasource.MetadataRequest, resp *datasource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_model_providers"
}

func (d *modelProvidersDataSource) Schema(_ context.Context, _ datasource.SchemaRequest, resp *datasource.SchemaResponse) {
	resp.Schema = schema.Schema{
		Description: "Every model provider the workspace can use.",
		Attributes: map[string]schema.Attribute{
			"providers": schema.ListNestedAttribute{
				Computed: true,
				NestedObject: schema.NestedAttributeObject{
					Attributes: map[string]schema.Attribute{
						"id":           schema.StringAttribute{Computed: true},
						"type":         schema.StringAttribute{Computed: true, Description: "`platform` or `byok`."},
						"format":       schema.StringAttribute{Computed: true},
						"display_name": schema.StringAttribute{Computed: true},
						"description":  schema.StringAttribute{Computed: true},
						"models": schema.ListNestedAttribute{
							Computed:    true,
							Description: "A platform provider's model catalog. Empty for a workspace's own.",
							NestedObject: schema.NestedAttributeObject{
								Attributes: map[string]schema.Attribute{
									"model":        schema.StringAttribute{Computed: true},
									"display_name": schema.StringAttribute{Computed: true},
									"description":  schema.StringAttribute{Computed: true},
								},
							},
						},
					},
				},
			},
		},
	}
}

func (d *modelProvidersDataSource) Configure(_ context.Context, req datasource.ConfigureRequest, resp *datasource.ConfigureResponse) {
	d.client = clientFrom(req.ProviderData, &resp.Diagnostics)
}

func (d *modelProvidersDataSource) Read(ctx context.Context, _ datasource.ReadRequest, resp *datasource.ReadResponse) {
	providers, err := d.client.ListModelProviders(ctx)
	if err != nil {
		apiError(&resp.Diagnostics, "list model providers", err)
		return
	}
	state := modelProvidersModel{Providers: []modelProviderEntry{}}
	for _, p := range providers {
		entry := modelProviderEntry{
			ID:          types.StringValue(p.ID),
			Type:        types.StringValue(p.Type),
			Format:      types.StringValue(p.Format),
			DisplayName: types.StringValue(p.DisplayName),
			Description: types.StringValue(p.Description),
			Models:      []platformModelEntry{},
		}
		for _, m := range p.Models {
			entry.Models = append(entry.Models, platformModelEntry{
				Model:       types.StringValue(m.Model),
				DisplayName: types.StringValue(m.DisplayName),
				Description: types.StringValue(m.Description),
			})
		}
		state.Providers = append(state.Providers, entry)
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

func newWorkspaceDataSource() datasource.DataSource {
	return &workspaceDataSource{}
}

// workspaceDataSource reads one workspace, by default the provider's.
type workspaceDataSource struct {
	client *client.Client
}

type workspaceModel struct {
	ID             types.String `tfsdk:"id"`
	Name           types.String `tfsdk:"name"`
	OrganizationID types.String `tfsdk:"organization_id"`
}

func (d *workspaceDataSource) Metadata(_ context.Context, req datasource.MetadataRequest, resp *datasource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_workspace"
}

func (d *workspaceDataSource) Schema(_ context.Context, _ datasource.SchemaRequest, resp *datasource.SchemaResponse) {
	resp.Schema = schema.Schema{
		Description: "A workspace the credential can see.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				Optional:    true,
				Computed:    true,
				Description: "Defaults to the provider's `workspace_id`.",
			},
			"name":            schema.StringAttribute{Computed: true},
			"organization_id": schema.StringAttribute{Computed: true},
		},
	}
}

func (d *workspaceDataSource) Configure(_ context.Context, req datasource.ConfigureRequest, resp *datasource.ConfigureResponse) {
	d.client = clientFrom(req.ProviderData, &resp.Diagnostics)
}

func (d *workspaceDataSource) Read(ctx context.Context, req datasource.ReadRequest, resp *datasource.ReadResponse) {
	var config workspaceModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &config)...)
	if resp.Diagnostics.HasError() {
		return
	}
	id, ok := plannedString(config.ID).Get()
	if !ok {
		id = d.client.WorkspaceID()
	}
	if id == "" {
		resp.Diagnostics.AddError("No workspace named",
			"Set id, or configure the provider's workspace_id (or $SUBAKO_WORKSPACE).")
		return
	}
	workspace, err := d.client.GetWorkspace(ctx, id)
	if err != nil {
		apiError(&resp.Diagnostics, "read workspace "+id, err)
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &workspaceModel{
		ID:             types.StringValue(workspace.ID),
		Name:           types.StringValue(workspace.Name),
		OrganizationID: types.StringValue(workspace.OrganizationID),
	})...)
}
