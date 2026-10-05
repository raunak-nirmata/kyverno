package compiler

import (
	"context"
	"fmt"
	"runtime"
	"strings"
	"testing"

	"github.com/google/cel-go/cel"
	"github.com/kyverno/kyverno/pkg/cel/requestbudget"
	"k8s.io/apimachinery/pkg/util/version"
	"k8s.io/apiserver/pkg/cel/environment"
)

// familyEnv is the shape every policy family's compiler builds: the apiserver base env set
// at the family's compatibility version, extended through BaseEnvSet so CostLib is
// registered last, with the resource and derived-variable roots declared dyn exactly as the
// real compilers declare them.
func familyEnv(t *testing.T) *cel.Env {
	t.Helper()
	opts := append(DefaultEnvOptions(),
		cel.Variable("object", cel.DynType),
		cel.Variable("variables", cel.DynType),
	)
	set, err := BaseEnvSet(version.MajorMinor(2, 0), environment.VersionedOptions{
		IntroducedVersion: version.MajorMinor(1, 0),
		EnvOptions:        opts,
	})
	if err != nil {
		t.Fatalf("env set: %v", err)
	}
	env, err := set.Env(environment.StoredExpressions)
	if err != nil {
		t.Fatalf("env: %v", err)
	}
	return env
}

func liveHeapMB() float64 {
	runtime.GC()
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	return float64(m.HeapAlloc) / (1 << 20)
}

// TestWideConcatOverOneDynFieldIsRefused pins the first shape this cut exists to bound: one
// 1 MiB dyn field concatenated across 128 operands. Unfixed, every add_string resolved no
// overload, so cel-go's own switch billed a flat 1 each: measured on this branch at base,
// the expression built a 128 MiB string, retained 131.1 MiB of live heap, and was charged
// 383 of the 1,000,000-unit limit with no error. Priced by function name instead, the same
// expression is refused.
func TestWideConcatOverOneDynFieldIsRefused(t *testing.T) {
	env := familyEnv(t)
	operands := make([]string, 128)
	for i := range operands {
		operands[i] = "object.blob"
	}
	ast, err := Compile(env, strings.Join(operands, " + "))
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	prg, err := NewProgram(env, ast)
	if err != nil {
		t.Fatalf("program: %v", err)
	}
	vars := map[string]any{"object": map[string]any{"blob": strings.Repeat("x", 1<<20)}}

	ctx, release := requestbudget.Ensure(context.Background())
	defer release()
	before := liveHeapMB()
	out, details, evalErr := prg.ContextEval(ctx, vars)
	after := liveHeapMB()

	charged := "untracked"
	if details != nil {
		if c := details.ActualCost(); c != nil {
			charged = fmt.Sprintf("%d", *c)
		}
	}
	resultBytes := 0
	if out != nil {
		if s, ok := out.Value().(string); ok {
			resultBytes = len(s)
		}
	}
	t.Logf("128 x 1 MiB dyn concat: err=%v charged=%s resultBytes=%d liveHeap %.1f -> %.1f MiB", evalErr, charged, resultBytes, before, after)

	if evalErr == nil {
		t.Fatalf("expected the expression to be refused; it completed, charged %s, built %d bytes, live heap %.1f MiB", charged, resultBytes, after)
	}
	if !IsResourceError(evalErr) {
		t.Fatalf("expected a resource refusal, got: %v", evalErr)
	}
	if after-before > 64 {
		t.Fatalf("refused but still retained %.1f MiB of live heap, want well under the unfixed 131.1 MiB", after-before)
	}
}

// TestChainedDoublingAcrossProgramsIsRefused pins the second shape: thirteen successive
// doublings of a 64 KiB seed, each one a separate program whose result feeds the next
// through variables[]. A per-program limit structurally cannot see the chain -- every stage
// is its own program and only ever doubles its own input -- so unfixed, the chain ran to
// completion on this branch at base: a 512 MB string, 1,026.0 MiB of live heap, 65 units
// charged in total, no error. This is what the shared per-request budget exists for.
func TestChainedDoublingAcrossProgramsIsRefused(t *testing.T) {
	env := familyEnv(t)
	const stages = 13

	programs := make([]cel.Program, 0, stages)
	for i := 0; i < stages; i++ {
		expr := `object.blob + object.blob`
		if i > 0 {
			expr = fmt.Sprintf(`variables.v%d + variables.v%d`, i-1, i-1)
		}
		ast, err := Compile(env, expr)
		if err != nil {
			t.Fatalf("compile stage %d: %v", i, err)
		}
		prg, err := NewProgram(env, ast)
		if err != nil {
			t.Fatalf("program stage %d: %v", i, err)
		}
		programs = append(programs, prg)
	}

	derived := map[string]any{}
	vars := map[string]any{
		"object":    map[string]any{"blob": strings.Repeat("x", 64<<10)},
		"variables": derived,
	}

	ctx, release := requestbudget.Ensure(context.Background())
	defer release()
	before := liveHeapMB()
	var charged uint64
	var refusedAt = -1
	var lastErr error
	var lastBytes int
	for i, prg := range programs {
		out, details, err := prg.ContextEval(ctx, vars)
		if details != nil {
			if c := details.ActualCost(); c != nil {
				charged += *c
			}
		}
		if err != nil {
			refusedAt, lastErr = i, err
			break
		}
		s := out.Value().(string)
		lastBytes = len(s)
		derived[fmt.Sprintf("v%d", i)] = s
	}
	after := liveHeapMB()
	t.Logf("13 chained dyn doublings: refusedAt=%d err=%v chargedTotal=%d largestResult=%d bytes liveHeap %.1f -> %.1f MiB",
		refusedAt, lastErr, charged, lastBytes, before, after)

	if refusedAt < 0 {
		t.Fatalf("expected the shared request budget to refuse a stage; all %d completed, charged %d, final result %d bytes, live heap %.1f MiB",
			stages, charged, lastBytes, after)
	}
	if !IsResourceError(lastErr) {
		t.Fatalf("expected a resource refusal, got: %v", lastErr)
	}
	if after-before > 128 {
		t.Fatalf("refused but still retained %.1f MiB of live heap, want well under the unfixed 1,026.0 MiB", after-before)
	}
}

// TestSameOperatorIsPricedTheSameTypedAndDyn is the control for why a cost limit alone does
// nothing here: cel-go prices add_string by resolved overload ID, so the identical
// concatenation over a 1 MiB value was charged 209,720 against a typed operand and 5
// against a dyn one on this branch at base -- a 41,944x underestimate. Pricing by function
// name instead must bring the two within a small factor of each other.
func TestSameOperatorIsPricedTheSameTypedAndDyn(t *testing.T) {
	const size = 1 << 20
	blob := strings.Repeat("x", size)

	typedEnv, err := NewEnv(append(DefaultEnvOptions(), cel.Variable("s", cel.StringType))...)
	if err != nil {
		t.Fatalf("typed env: %v", err)
	}
	dynEnv, err := NewEnv(append(DefaultEnvOptions(), cel.Variable("object", cel.DynType))...)
	if err != nil {
		t.Fatalf("dyn env: %v", err)
	}

	charge := func(env *cel.Env, expr string, vars map[string]any) uint64 {
		ast, err := Compile(env, expr)
		if err != nil {
			t.Fatalf("compile %q: %v", expr, err)
		}
		prg, err := NewProgram(env, ast)
		if err != nil {
			t.Fatalf("program %q: %v", expr, err)
		}
		_, details, err := prg.ContextEval(context.Background(), vars)
		if err != nil {
			t.Fatalf("eval %q: %v", expr, err)
		}
		if details == nil || details.ActualCost() == nil {
			t.Fatalf("%q: no cost tracked -- the limit cannot bind on an untracked program", expr)
		}
		return *details.ActualCost()
	}

	typed := charge(typedEnv, `s + s`, map[string]any{"s": blob})
	dyn := charge(dynEnv, `object.blob + object.blob`, map[string]any{"object": map[string]any{"blob": blob}})
	t.Logf("s + s over %d bytes: typed=%d dyn=%d", size, typed, dyn)

	if dyn*4 < typed {
		t.Fatalf("dyn concat charged %d against %d typed -- the dyn path is still underpriced by %.0fx", dyn, typed, float64(typed)/float64(dyn))
	}
}
