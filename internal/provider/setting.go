package provider

import (
	"context"

	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

// setting is what an attribute the configuration may leave unset holds: a
// value Terraform manages, or nothing at all. The framework's three states
// collapse here and nowhere else -- a value is managed only when it is
// neither null nor unknown -- so no caller can read an unknown as a value or
// an absent one as an empty one.
type setting[T any] struct {
	raw     attr.Value
	value   T
	managed bool
}

// Get answers the value and whether Terraform manages it.
func (s setting[T]) Get() (T, bool) {
	return s.value, s.managed
}

// Equal reports whether both settings read the same attribute value.
func (s setting[T]) Equal(other setting[T]) bool {
	return s.raw.Equal(other.raw)
}

// planned reads a framework value as a setting over itself: the one place
// null and unknown become "Terraform does not manage this".
func planned[V attr.Value](v V) setting[V] {
	if v.IsNull() || v.IsUnknown() {
		return setting[V]{raw: v}
	}
	return setting[V]{raw: v, value: v, managed: true}
}

// fullyKnown reports whether every value under each of values is known. A
// value the plan leaves unknown could be anything once applied, so a caller
// comparing what an apply would send has nothing to compare yet.
func fullyKnown(ctx context.Context, values ...attr.Value) (bool, diag.Diagnostics) {
	var diags diag.Diagnostics
	for _, value := range values {
		raw, err := value.ToTerraformValue(ctx)
		if err != nil {
			diags.AddError("Unreadable attribute value", err.Error())
			return false, diags
		}
		if !raw.IsFullyKnown() {
			return false, diags
		}
	}
	return true, diags
}

// mapped carries a setting through a conversion, leaving an unmanaged one
// unmanaged.
func mapped[A, B any](s setting[A], convert func(A) B) setting[B] {
	if !s.managed {
		return setting[B]{raw: s.raw}
	}
	return setting[B]{raw: s.raw, value: convert(s.value), managed: true}
}

// plannedString reads a string attribute.
func plannedString(v types.String) setting[string] {
	return mapped(planned(v), types.String.ValueString)
}

// plannedInt64 reads an integer attribute.
func plannedInt64(v types.Int64) setting[int64] {
	return mapped(planned(v), types.Int64.ValueInt64)
}

// plannedStrings reads a set-of-strings attribute.
func plannedStrings(ctx context.Context, v types.Set) (setting[[]string], diag.Diagnostics) {
	if _, ok := planned(v).Get(); !ok {
		return setting[[]string]{raw: v}, nil
	}
	var out []string
	diags := v.ElementsAs(ctx, &out, false)
	return setting[[]string]{raw: v, value: out, managed: true}, diags
}

// plannedLabels reads a map-of-strings attribute. An attribute Terraform does
// not manage carries no labels.
func plannedLabels(ctx context.Context, v types.Map) (map[string]string, diag.Diagnostics) {
	labels := map[string]string{}
	if _, ok := planned(v).Get(); !ok {
		return labels, nil
	}
	return labels, v.ElementsAs(ctx, &labels, false)
}

// stringPointer is an optional string on the wire: nil where Terraform
// manages nothing.
func stringPointer(v types.String) *string {
	return pointer(plannedString(v))
}

// int64Pointer is an optional integer on the wire.
func int64Pointer(v types.Int64) *int64 {
	return pointer(plannedInt64(v))
}

func pointer[T any](s setting[T]) *T {
	value, ok := s.Get()
	if !ok {
		return nil
	}
	return &value
}
