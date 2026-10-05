package compiler

import (
	"github.com/google/cel-go/cel"
	"github.com/google/cel-go/ext"
	"github.com/kyverno/sdk/cel/libs/image"
	"k8s.io/apimachinery/pkg/util/version"
	"k8s.io/apiserver/pkg/cel/environment"
	"k8s.io/apiserver/pkg/cel/library"
)

func DefaultEnvOptions() []cel.EnvOption {
	return []cel.EnvOption{
		cel.HomogeneousAggregateLiterals(),
		cel.EagerlyValidateDeclarations(true),
		cel.DefaultUTCTimeZone(true),
		cel.CrossTypeNumericComparisons(true),
		// register common libs
		cel.OptionalTypes(),
		ext.Bindings(),
		ext.Encoders(),
		ext.Lists(),
		ext.Math(),
		ext.Protos(),
		ext.Sets(),
		ext.Strings(),
		// register kubernetes libs
		library.CIDR(),
		library.Format(),
		library.IP(),
		library.Lists(),
		library.Regex(),
		library.URLs(),
		library.Quantity(),
		library.SemverLib(),
	}
}

// --- chokepoints ---
//
// NewEnv and BaseEnvSet are the two places CostLib gets attached to a *cel.Env or
// *environment.EnvSet. CostLib must be the LAST function-registering option: its
// env-capture (costlib.go's CompileOptions) only sees a complete env if nothing adds more
// functions afterward, and its binding registry (guards.go) needs every function on the
// env to wrap it. A caller that builds through NewEnv/BaseEnvSet and then calls a second
// env.Extend / EnvSet.Extend with more function-registering options breaks this silently --
// no error, just a function left unwrapped or a replacement tracker that never wins.

// NewEnv is the chokepoint for bare (non-apiserver) *cel.Env construction. opts is the
// caller's whole option list; CostLib is appended last.
func NewEnv(opts ...cel.EnvOption) (*cel.Env, error) {
	all := make([]cel.EnvOption, 0, len(opts)+1)
	all = append(all, opts...)
	all = append(all, cel.Lib(CostLib())) // must stay last; see the chokepoint comment above
	return cel.NewEnv(all...)
}

// BaseEnvSet is the chokepoint for apiserver-based *environment.EnvSet construction. opts
// is the caller's whole VersionedOptions list, passed to a single Extend call alongside a
// final VersionedOptions carrying CostLib at IntroducedVersion 1.0, so it applies under
// environment.NewExpressions too (webhook matchConditions) as well as
// environment.StoredExpressions.
func BaseEnvSet(v *version.Version, opts ...environment.VersionedOptions) (*environment.EnvSet, error) {
	base := environment.MustBaseEnvSet(v)
	all := make([]environment.VersionedOptions, 0, len(opts)+1)
	all = append(all, opts...)
	all = append(all, environment.VersionedOptions{
		IntroducedVersion: version.MajorMinor(1, 0),
		EnvOptions:        []cel.EnvOption{cel.Lib(CostLib())},
	})
	return base.Extend(all...)
}

func NewBaseEnv() (*cel.Env, error) {
	return NewEnv(DefaultEnvOptions()...)
}

func NewMatchImageEnv() (*cel.Env, error) {
	opts := append(append([]cel.EnvOption{}, DefaultEnvOptions()...),
		cel.Variable(ImageRefKey, cel.StringType),
		image.Lib(image.Latest()),
	)
	return NewEnv(opts...)
}
