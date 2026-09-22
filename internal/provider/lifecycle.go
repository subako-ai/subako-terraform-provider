package provider

import (
	"context"

	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/resource"

	"github.com/Kikuvi-Inc/subako-terraform-provider/internal/client"
)

// Every resource here has the same shape: a server call or two that change
// something, then a read-back that decides what the state holds. The four
// endings below are that shape, written once, so no resource can drop the
// read's diagnostics, keep a resource the server no longer has, or leave a
// created one out of the state.

// reader refreshes m from the server. found is false when the resource is
// gone; diagnostics travel whether or not it was found.
type reader[M any] func(ctx context.Context, m *M) (found bool, diags diag.Diagnostics)

// subject names the resource a lifecycle diagnostic is about.
type subject struct {
	kind string
	id   string
}

func (s subject) String() string {
	return s.kind + " " + s.id
}

func (s subject) vanished(during string) (string, string) {
	return s.kind + " vanished", s.String() + " was deleted while it was being " + during + "."
}

// refresh answers a Read: a resource the server no longer holds leaves the
// state, and anything the read reported travels with it.
func refresh[M any](ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse, read reader[M]) {
	var state M
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	found, diags := read(ctx, &state)
	resp.Diagnostics.Append(diags...)
	if !found {
		resp.State.RemoveResource(ctx)
		return
	}
	if resp.Diagnostics.HasError() {
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

// finishCreate ends a Create. The resource already exists by now, so the
// state holds `created` whenever anything fails -- Terraform taints it on the
// next run rather than losing track of it -- and what the read-back found
// otherwise.
func finishCreate[M any](ctx context.Context, resp *resource.CreateResponse, s subject, read reader[M], next, created M) {
	if !resp.Diagnostics.HasError() {
		found, diags := read(ctx, &next)
		resp.Diagnostics.Append(diags...)
		if !found {
			resp.Diagnostics.AddError(s.vanished("created"))
		}
	}
	if resp.Diagnostics.HasError() {
		resp.Diagnostics.Append(resp.State.Set(ctx, &created)...)
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &next)...)
}

// finishUpdate ends an Update by reading the resource back, whether or not a
// step failed, so the next plan is made against what the server holds; a
// failure is still raised. What the read cannot observe -- a base URL the API
// never returns, the version a write-only secret was sent under -- has to
// come from somewhere, and that depends on the steps: when they all succeeded
// it is what `planned` says was sent, and when one failed it is what `prior`
// held, since a change recorded but never sent would leave nothing for the
// next plan to retry. The state keeps what it held when the read-back itself
// fails, and a resource that vanished under the update is an error rather
// than a silent removal.
func finishUpdate[M any](ctx context.Context, resp *resource.UpdateResponse, s subject, read reader[M], planned, prior M) {
	next := planned
	if resp.Diagnostics.HasError() {
		next = prior
	}
	found, diags := read(ctx, &next)
	resp.Diagnostics.Append(diags...)
	if !found {
		resp.Diagnostics.AddError(s.vanished("updated"))
		return
	}
	if diags.HasError() {
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &next)...)
}

// remove ends a Delete: a resource the server no longer holds is one deleted.
func remove(resp *resource.DeleteResponse, action string, del func() error) {
	if err := del(); err != nil && !client.IsNotFound(err) {
		apiError(&resp.Diagnostics, action, err)
	}
}

// step is one server call made after the resource exists, named for the
// diagnostic a failure raises.
type step struct {
	action string
	call   func() error
}

// run makes each call until one fails. A step after a failed one would act on
// a resource whose state is already unknown, so it is not made.
func run(diags *diag.Diagnostics, steps ...step) {
	for _, s := range steps {
		if err := s.call(); err != nil {
			apiError(diags, s.action, err)
			return
		}
	}
}
