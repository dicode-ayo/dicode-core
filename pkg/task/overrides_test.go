package task

import "testing"

// TestValidatePerEdgeOverrides_AcceptsBareOutputToken locks the contract
// that validatePerEdgeOverrides does NOT reject ${input.output} appearing
// as a per-edge param Default. The token survives config-load and is
// resolved by ResolveInputOutputList at dispatch time.
func TestValidatePerEdgeOverrides_AcceptsBareOutputToken(t *testing.T) {
	o := &Overrides{
		Params: ParamOverrides{
			{Name: "content", Default: "${input.output}"},
			{Name: "path", Default: "/foo/bar"},
		},
	}
	if err := validatePerEdgeOverrides("trigger.chain.overrides", o); err != nil {
		t.Errorf("unexpected rejection of ${input.output}: %v", err)
	}
}
