package provider

import (
	"context"
	"errors"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/hashicorp/terraform-plugin-go/tftypes"
)

// updateResponse is an Update about to end, with an empty state.
func updateResponse(ctx context.Context, t *testing.T) *resource.UpdateResponse {
	t.Helper()
	var schema resource.SchemaResponse
	newVaultResource().Schema(ctx, resource.SchemaRequest{}, &schema)
	return &resource.UpdateResponse{State: tfsdk.State{
		Schema: schema.Schema,
		Raw:    tftypes.NewValue(schema.Schema.Type().TerraformType(ctx), nil),
	}}
}

func readsBack(_ context.Context, m *vaultModel) (bool, diag.Diagnostics) {
	m.WorkspaceID = types.StringValue("workspace")
	m.DisplayName = types.StringValue("what the server holds")
	m.Metadata = types.MapValueMust(types.StringType, nil)
	return true, nil
}

// A step that failed leaves every step before it done, so the read-back runs
// anyway: the state records what the server holds, and the failure is still
// raised.
func TestAFailedUpdateRecordsWhatTheServerHolds(t *testing.T) {
	ctx := context.Background()
	resp := updateResponse(ctx, t)
	apiError(&resp.Diagnostics, "update vault v", errors.New("gateway timeout"))

	finishUpdate(ctx, resp, subject{"Vault", "v"}, readsBack, vaultModel{ID: types.StringValue("v")}, vaultModel{ID: types.StringValue("v")})

	if !resp.Diagnostics.HasError() {
		t.Fatal("the failure was dropped")
	}
	var held vaultModel
	if diags := resp.State.Get(ctx, &held); diags.HasError() {
		t.Fatalf("the state holds nothing: %v", diags)
	}
	if got := held.DisplayName.ValueString(); got != "what the server holds" {
		t.Fatalf("the state says the display name is %q", got)
	}
}

// A read-back that fails says nothing about the server, so the state keeps
// what it held.
func TestAnUpdateWhoseReadFailsKeepsTheState(t *testing.T) {
	ctx := context.Background()
	resp := updateResponse(ctx, t)

	finishUpdate(ctx, resp, subject{"Vault", "v"}, func(_ context.Context, _ *vaultModel) (bool, diag.Diagnostics) {
		var diags diag.Diagnostics
		apiError(&diags, "read vault v", errors.New("gateway timeout"))
		return true, diags
	}, vaultModel{ID: types.StringValue("v")}, vaultModel{ID: types.StringValue("v")})

	if !resp.Diagnostics.HasError() {
		t.Fatal("the failure was dropped")
	}
	if !resp.State.Raw.IsNull() {
		t.Fatalf("the state was written from an unread resource: %v", resp.State.Raw)
	}
}
