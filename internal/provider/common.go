package provider

import (
	"context"
	"fmt"
	"regexp"
	"unicode/utf8"

	"github.com/hashicorp/terraform-plugin-framework-validators/helpers/validatordiag"
	"github.com/hashicorp/terraform-plugin-framework-validators/stringvalidator"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"

	"github.com/subako-ai/terraform-provider-subako/internal/client"
)

// clientFrom unwraps the client Configure handed over. Before Configure has
// run, the data is nil and there is nothing to do yet.
func clientFrom(data any, diags *diag.Diagnostics) *client.Client {
	if data == nil {
		return nil
	}
	c, ok := data.(*client.Client)
	if !ok {
		diags.AddError("Unexpected provider data", fmt.Sprintf("expected *client.Client, got %T", data))
		return nil
	}
	return c
}

func apiError(diags *diag.Diagnostics, action string, err error) {
	diags.AddError("Subako API error", fmt.Sprintf("Could not %s: %s", action, err))
}

// CODESYNC(grant-name-charset)
var grantNamePattern = regexp.MustCompile(`^[a-z0-9-]+$`)

// grantNameValidators are the rules a grant name keeps on the server.
func grantNameValidators() []validator.String {
	return []validator.String{
		// CODESYNC(grant-name-len)
		charsBetween(1, 32),
		stringvalidator.RegexMatches(grantNamePattern, "must be lowercase alphanumeric plus hyphen"),
		stringvalidator.NoneOf("parent"),
	}
}

// uuidPattern is a UUID as the API writes one: lowercase and hyphenated.
var uuidPattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// uuidValidators are the rules an id the server echoes back into the state
// keeps. The server reads a UUID in any spelling and answers with the
// canonical one, so a configuration naming any other spelling would be handed
// back a value it does not match, and no apply could ever converge.
func uuidValidators() []validator.String {
	return []validator.String{stringvalidator.RegexMatches(uuidPattern,
		"must be a lowercase hyphenated UUID, which is how the API spells the id it answers with")}
}

// charLength bounds a string in characters, as every bound the server keeps
// does (`#[garde(length(chars, ...))]`). Counting bytes would refuse a value
// the API accepts, and make a resource already holding one impossible to
// manage.
type charLength struct {
	atLeast int
	atMost  int
}

var _ validator.String = charLength{}

// charsBetween bounds a string in characters from both ends.
func charsBetween(atLeast, atMost int) validator.String {
	return charLength{atLeast: atLeast, atMost: atMost}
}

// charsAtMost bounds a string in characters from above, leaving the empty
// string allowed.
func charsAtMost(atMost int) validator.String {
	return charLength{atMost: atMost}
}

func (v charLength) Description(_ context.Context) string {
	if v.atLeast > 0 {
		return fmt.Sprintf("string length must be between %d and %d characters", v.atLeast, v.atMost)
	}
	return fmt.Sprintf("string length must be at most %d characters", v.atMost)
}

func (v charLength) MarkdownDescription(ctx context.Context) string {
	return v.Description(ctx)
}

func (v charLength) ValidateString(ctx context.Context, req validator.StringRequest, resp *validator.StringResponse) {
	value, ok := plannedString(req.ConfigValue).Get()
	if !ok {
		return
	}
	if count := utf8.RuneCountInString(value); count < v.atLeast || count > v.atMost {
		resp.Diagnostics.Append(validatordiag.InvalidAttributeValueLengthDiagnostic(
			req.Path, v.Description(ctx), fmt.Sprintf("%d", count)))
	}
}

// Policy actions, as `PolicyActionBody` spells them.
var policyActions = []string{"allow", "require_approval", "deny"}
