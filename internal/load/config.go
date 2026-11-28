package load

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/vamos-build/vamos/internal/fileset"
	"github.com/vamos-build/vamos/internal/graph"

	"github.com/containerd/platforms"
	imgspecv1 "github.com/opencontainers/image-spec/specs-go/v1"
	"go.starlark.net/starlark"
	"go.starlark.net/syntax"
)

// param is a parameter that a target declares with params:
// a value that each use of the target may pass it, with dep(), and that its functions read as ctx.<name>.
type param struct {
	name   string
	kind   paramKind
	def    starlark.Value // canonical, as canonical returns it; nil if every use must give a value
	values []string       // the values allowed, or for a list each element's; nil allows any
}

// paramKind is the type of a parameter's values, which its default, or param()'s type, sets.
type paramKind int

const (
	stringParam paramKind = iota
	boolParam
	intParam
	listParam // of strings, in the order given
)

// paramKinds names the kinds of parameters, as param()'s type gives them, in the order of their values.
var paramKinds = []string{"string", "bool", "int", "list"}

// reservedParams are the names that ctx has built in, which no parameter can have.
var reservedParams = []string{"platform", "os", "arch", "variant", "exec_platform"}

// canonical checks v as a value of p, the argument what, returning it in the one form that each value has,
// so that equal values make the same configuration: a list is frozen.
func (p *param) canonical(what string, v starlark.Value) (starlark.Value, error) {
	switch p.kind {
	case boolParam:
		if _, ok := v.(starlark.Bool); !ok {
			return nil, fmt.Errorf("%s is %s, want bool", what, v.Type())
		}
		return v, nil
	case intParam:
		if _, ok := v.(starlark.Int); !ok {
			return nil, fmt.Errorf("%s is %s, want int", what, v.Type())
		}
		return v, nil
	case stringParam:
		str, ok := starlark.AsString(v)
		if !ok {
			return nil, fmt.Errorf("%s is %s, want string", what, v.Type())
		}
		if p.values != nil && !slices.Contains(p.values, str) {
			return nil, fmt.Errorf("%s %q is not one of its values, %s", what, str, fileset.QuoteNames(p.values))
		}
		return v, nil
	}
	iterable, ok := v.(starlark.Indexable)
	if _, isString := v.(starlark.String); !ok || isString {
		return nil, fmt.Errorf("%s is %s, want a list of strings", what, v.Type())
	}
	items := make([]starlark.Value, iterable.Len())
	for i := range iterable.Len() {
		str, ok := starlark.AsString(iterable.Index(i))
		if !ok {
			return nil, fmt.Errorf("%s[%d] is %s, want string", what, i, iterable.Index(i).Type())
		}
		if p.values != nil && !slices.Contains(p.values, str) {
			return nil, fmt.Errorf("%s has %q, which is not one of its values, %s",
				what, str, fileset.QuoteNames(p.values))
		}
		items[i] = starlark.String(str)
	}
	list := starlark.NewList(items)
	list.Freeze()
	return list, nil
}

// parse returns the value of p that text, given with --set, names: true or false for a bool,
// and for a list its strings separated by commas, or none if text is empty.
func (p *param) parse(text string) (starlark.Value, error) {
	switch p.kind {
	case boolParam:
		switch text {
		case "true":
			return starlark.True, nil
		case "false":
			return starlark.False, nil
		}
		return nil, fmt.Errorf("%s is %q, want true or false", p.name, text)
	case intParam:
		n, err := strconv.ParseInt(text, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("%s is %q, want an integer", p.name, text)
		}
		return starlark.MakeInt64(n), nil
	case stringParam:
		return p.canonical(p.name, starlark.String(text))
	}
	var items []starlark.Value
	if text != "" {
		for str := range strings.SplitSeq(text, ",") {
			items = append(items, starlark.String(str))
		}
	}
	return p.canonical(p.name, starlark.NewList(items))
}

// newParam returns the parameter that a default, def, or else a type, typ, declares, allowing only values, if given.
// Its name is set by the params dict that holds it.
func newParam(def starlark.Value, typ string, values *starlark.List) (*param, error) {
	p := &param{}
	if def != nil {
		switch def.(type) {
		case starlark.Bool:
			p.kind = boolParam
		case starlark.Int:
			p.kind = intParam
		case starlark.String:
			p.kind = stringParam
		case *starlark.List, starlark.Tuple:
			p.kind = listParam
		default:
			return nil, fmt.Errorf("default is %s, want a string, a bool, an int or a list of strings", def.Type())
		}
	}
	switch k := slices.Index(paramKinds, typ); {
	case typ == "" && def == nil:
		return nil, errors.New("give a default, or else the type of a value that every use must give")
	case typ == "":
	case k < 0:
		return nil, fmt.Errorf("type %q is not one of %s", typ, fileset.QuoteNames(paramKinds))
	case def != nil && paramKind(k) != p.kind:
		return nil, fmt.Errorf("default is %s, but type is %q", def.Type(), typ)
	default:
		p.kind = paramKind(k)
	}
	if values != nil {
		if p.kind != stringParam && p.kind != listParam {
			return nil, errors.New("values is only for a string or list parameter")
		}
		for i := range values.Len() {
			str, ok := starlark.AsString(values.Index(i))
			if !ok {
				return nil, fmt.Errorf("values[%d] is %s, want string", i, values.Index(i).Type())
			}
			p.values = append(p.values, str)
		}
		if len(p.values) == 0 {
			return nil, errors.New("values must not be empty")
		}
	}
	if def != nil {
		var err error
		if p.def, err = p.canonical("default", def); err != nil {
			return nil, err
		}
	}
	return p, nil
}

// paramValue is the value of a call to param(): a parameter, without its name, which a params dict gives it.
type paramValue struct{ param *param }

func (v *paramValue) Type() string          { return "param" }
func (v *paramValue) Freeze()               {} // immutable
func (v *paramValue) Truth() starlark.Bool  { return true }
func (v *paramValue) Hash() (uint32, error) { return 0, errors.New("unhashable type: param") }

func (v *paramValue) String() string {
	var args []string
	if v.param.def != nil {
		args = append(args, "default = "+v.param.def.String())
	} else {
		args = append(args, "type = "+strconv.Quote(paramKinds[v.param.kind]))
	}
	if v.param.values != nil {
		args = append(args, "values = "+reprStrings(v.param.values))
	}
	return "param(" + strings.Join(args, ", ") + ")"
}

// newParamBuiltin implements param(default, values, type),
// which declares a parameter in more detail than its default alone does.
func newParamBuiltin(
	_ *starlark.Thread, fn *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple,
) (starlark.Value, error) {
	var def starlark.Value
	var values *starlark.List
	var typ string
	if err := starlark.UnpackArgs(fn.Name(), args, kwargs,
		"default?", &def,
		"values?", &values,
		"type?", &typ,
	); err != nil {
		return nil, err
	}
	p, err := newParam(def, typ, values)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", fn.Name(), err)
	}
	return &paramValue{p}, nil
}

// parseParams returns the parameters that v, the params argument of a call to fnName, declares:
// a dict from each one's name to its default, or to param().
func parseParams(fnName string, v starlark.Value) ([]*param, error) {
	if _, ok := v.(starlark.Callable); ok {
		return nil, fmt.Errorf("%s: params is function, want a dict; params can't depend on ctx, "+
			"since what a target takes must be known before it is configured", fnName)
	}
	dict, ok := v.(*starlark.Dict)
	if !ok {
		return nil, fmt.Errorf("%s: params is %s, want a dict from each parameter's name to its default",
			fnName, v.Type())
	}
	var params []*param
	for _, item := range dict.Items() {
		name, ok := starlark.AsString(item[0])
		switch {
		case !ok:
			return nil, fmt.Errorf("%s: params has a key of type %s, want a parameter's name", fnName, item[0].Type())
		case slices.Contains(reservedParams, name):
			return nil, fmt.Errorf("%s: params: %q is built in, as ctx.%s, and can't be declared", fnName, name, name)
		case !isIdentifier(name) || name == "None" || name == "True" || name == "False":
			return nil, fmt.Errorf("%s: params: name %q must be an identifier, since functions read it as ctx.<name>",
				fnName, name)
		}
		var p *param
		if pv, ok := item[1].(*paramValue); ok {
			copied := *pv.param
			p = &copied
		} else {
			var err error
			if p, err = newParam(item[1], "", nil); err != nil {
				return nil, fmt.Errorf("%s: params: %s: %w", fnName, name, err)
			}
		}
		p.name = name
		params = append(params, p)
	}
	return params, nil
}

// paramIndex returns the index of the parameter name in params, or -1 if there is none.
func paramIndex(params []*param, name string) int {
	return slices.IndexFunc(params, func(p *param) bool { return p.name == name })
}

// paramNames returns the names of params, in order.
func paramNames(params []*param) []string {
	names := make([]string, len(params))
	for i, p := range params {
		names[i] = p.name
	}
	return names
}

// noParam reports that target, which declares params, has no parameter name, suggesting the one meant.
func noParam(target, name string, params []*param) error {
	return fmt.Errorf("%q has no parameter %q%s", target, name, paramHint(name, params))
}

// paramHint suggests the parameter among params meant by name, which isn't one, or else lists them.
func paramHint(name string, params []*param) string {
	names := paramNames(params)
	if closest := closestName(name, names); closest != "" {
		return fmt.Sprintf("; did you mean %q?", closest)
	} else if len(names) > 0 {
		return "; its parameters are " + fileset.QuoteNames(names)
	}
	return "; it declares none"
}

// config is a configuration of one target: its platform, and a value for each of its parameters.
// Equal configurations are the same *config, as intern makes them, so a target in a configuration can be a map key.
type config struct {
	platform *platformValue
	values   []starlark.Value // by parameter, in the order declared
}

// confLabel is a target in a configuration.
type confLabel struct {
	label string
	cfg   *config // nil for scratch, which has none
}

// intern returns the configuration for platform that gives params values, which are canonical.
func (w *workspace) intern(platform *platformValue, params []*param, values []starlark.Value) *config {
	var b strings.Builder
	b.WriteString(platform.String())
	for i, v := range values {
		fmt.Fprintf(&b, "\n%s=%s", params[i].name, v)
	}
	key := b.String()
	if c, ok := w.configs[key]; ok {
		return c
	}
	c := &config{platform: platform, values: values}
	w.configs[key] = c
	return c
}

// commandConfig returns the configuration of the target l that the command line gives: the platform of --platform,
// and the arguments of sets, its --set flags, each name=value, with defaults for the rest.
// If shared, the flags are for several targets,
// so l ignores those naming parameters it lacks; checkSets reports any that no target has.
func (w *workspace) commandConfig(l string, sets []string, shared bool) (*config, error) {
	d := w.declaration(l)
	values := make([]starlark.Value, len(d.params))
	for _, set := range sets {
		name, text, ok := strings.Cut(set, "=")
		if !ok {
			return nil, fmt.Errorf("--set %q: want name=value, as in --set mode=debug", set)
		}
		if name == "platform" {
			return nil, fmt.Errorf("--set %s: give the platform with --platform", set)
		}
		i := paramIndex(d.params, name)
		switch {
		case i < 0 && shared:
			continue
		case i < 0:
			return nil, fmt.Errorf("--set %s: %w", set, noParam(w.display(l), name, d.params))
		case values[i] != nil:
			return nil, fmt.Errorf("--set %s: %s is given more than once", set, name)
		}
		v, err := d.params[i].parse(text)
		if err != nil {
			return nil, fmt.Errorf("--set %s: %w", set, err)
		}
		values[i] = v
	}
	for i, p := range d.params {
		if values[i] == nil && p.def == nil {
			//nolint:staticcheck // ST1005: the message ends with --set's form, its value elided as "..."
			return nil, fmt.Errorf("%q needs a value for its parameter %q, which has no default; "+
				"give it with --set %s=...", w.display(l), p.name, p.name)
		}
		if values[i] == nil {
			values[i] = p.def
		}
	}
	return w.intern(w.platform, d.params, values), nil
}

// checkSets checks that each of sets, the --set flags given for the targets labels, names a parameter of one of them.
func (w *workspace) checkSets(labels []string, sets []string) error {
	for _, set := range sets {
		name, _, _ := strings.Cut(set, "=")
		declares := func(l string) bool { return paramIndex(w.declaration(l).params, name) >= 0 }
		if !slices.ContainsFunc(labels, declares) {
			return fmt.Errorf("--set %s: none of the named targets has a parameter %q", set, name)
		}
	}
	return nil
}

// describeConfig lists what tells the target cl apart in messages, as name=value:
// its platform, if it isn't the command's, and its arguments that aren't defaults.
// It returns "" if there are none.
func (w *workspace) describeConfig(cl confLabel) string {
	if cl.cfg == nil {
		return ""
	}
	var diffs []string
	if cl.cfg.platform.String() != w.platform.String() {
		diffs = append(diffs, "platform="+cl.cfg.platform.String())
	}
	for i, p := range w.declaration(cl.label).params {
		v := cl.cfg.values[i]
		if p.def == nil || v.String() != p.def.String() {
			diffs = append(diffs, p.name+"="+v.String())
		}
	}
	return strings.Join(diffs, ", ")
}

// describeConfigured names the target cl for an error, with what describeConfig lists.
func (w *workspace) describeConfigured(cl confLabel) string {
	if diffs := w.describeConfig(cl); diffs != "" {
		return cl.label + " with " + diffs
	}
	return cl.label
}

// ctxValue is what a function giving an attribute of a target is given, as a Starlark value:
// the target's configuration.
// ctx.<name> is the value of its parameter name,
// and ctx.platform, its parts ctx.os, ctx.arch and ctx.variant, and ctx.exec_platform are built in.
type ctxValue struct {
	params []*param
	cfg    *config
	glob   bool           // of a glob, configured for no platform in particular
	exec   *platformValue // the executor's platform, as ctx.exec_platform
}

func (v *ctxValue) Type() string          { return "ctx" }
func (v *ctxValue) Freeze()               {} // its values are frozen
func (v *ctxValue) Truth() starlark.Bool  { return true }
func (v *ctxValue) Hash() (uint32, error) { return 0, errors.New("unhashable type: ctx") }

func (v *ctxValue) String() string {
	var fields []string
	if !v.glob {
		fields = append(fields, fmt.Sprintf("platform = %s", v.cfg.platform))
	}
	for i, value := range v.cfg.values {
		fields = append(fields, fmt.Sprintf("%s = %s", v.params[i].name, value))
	}
	return "ctx(" + strings.Join(fields, ", ") + ")"
}

// Attr returns the value of the parameter name, or of a built-in field.
// Starlark suggests one for a misspelled name.
func (v *ctxValue) Attr(name string) (starlark.Value, error) {
	// Platforms are strings, such as "linux/arm64", so that comparing one with a string works:
	// Starlark's == is False for values of different types.
	// Their parts are as Go's GOOS and GOARCH name them.
	switch name {
	case "platform", "os", "arch", "variant":
		if v.glob {
			return nil, fmt.Errorf("ctx has no %s in a glob, whose files are the same for every platform", name)
		}
	}
	switch name {
	case "platform":
		return starlark.String(v.cfg.platform.String()), nil
	case "os":
		return starlark.String(v.cfg.platform.spec.OS), nil
	case "arch":
		return starlark.String(v.cfg.platform.spec.Architecture), nil
	case "variant":
		return starlark.String(v.cfg.platform.spec.Variant), nil
	case "exec_platform":
		return starlark.String(v.exec.String()), nil
	}
	i := paramIndex(v.params, name)
	if i < 0 {
		declared := "; the target declares no parameters, in params"
		if len(v.params) > 0 {
			declared = "; the target's parameters are " + fileset.QuoteNames(paramNames(v.params))
		}
		return nil, starlark.NoSuchAttrError(fmt.Sprintf("ctx has no parameter %q%s", name, declared))
	}
	return v.cfg.values[i], nil
}

func (v *ctxValue) AttrNames() []string {
	return append(slices.Clone(reservedParams), paramNames(v.params)...)
}

// platformValue is a platform, such as linux/arm64.
// Starlark sees it as a string, such as ctx.platform.
type platformValue struct {
	spec imgspecv1.Platform // normalized
}

// parsePlatform parses a platform written as os/arch or os/arch/variant, normalizing it as containerd does:
// linux/arm64/v8 is linux/arm64, and linux/arm is linux/arm/v7.
func parsePlatform(text string) (*platformValue, error) {
	spec, err := platforms.Parse(text)
	if err != nil {
		return nil, fmt.Errorf("platform %q: want os/arch or os/arch/variant, such as \"linux/arm64\": %w", text, err)
	}
	if strings.Count(text, "/") == 0 {
		return nil, fmt.Errorf("platform %q: want os/arch or os/arch/variant, such as \"linux/arm64\"", text)
	}
	return &platformValue{platforms.Normalize(spec)}, nil
}

func (v *platformValue) String() string { return platforms.Format(v.spec) }

// execPlatformValue is EXEC_PLATFORM,
// which dep() takes as its platform to name a target for the platform that build steps run on.
type execPlatformValue struct{}

func (execPlatformValue) String() string        { return "EXEC_PLATFORM" }
func (execPlatformValue) Type() string          { return "exec_platform" }
func (execPlatformValue) Freeze()               {} // immutable
func (execPlatformValue) Truth() starlark.Bool  { return true }
func (execPlatformValue) Hash() (uint32, error) { return starlark.String("EXEC_PLATFORM").Hash() }

// targetRef names a target that another uses, as a BUILD file gives it: by a label,
// and, if dep() gave it, with arguments for its parameters, and its platform.
type targetRef struct {
	ref      string         // the label as given
	pkg      string         // the package ref is relative to
	label    string         // the target's label, once resolve has found it
	platform *platformValue // for the target, if not the using target's: given by dep(), or an index's entry
	exec     bool           // for the target on the exec platform, as dep() gives with EXEC_PLATFORM
	args     []depArg       // in the order given; once checked, canonical, unless arg()
	// of the call to dep() giving the label, if a BUILD file made it, or a function it calls, where a mistake in what
	// dep() says, such as its arguments, is reported; one made as a .star file is loaded is reported where it is used
	pos    syntax.Position
	frozen bool // once freeze has frozen args
	viaDep bool // dep() gave it, rather than a label alone
}

// depArg is an argument that dep() gives a parameter of the target it names: a value, or an arg().
type depArg struct {
	name  string
	value starlark.Value
}

// checkArgs checks the arguments that ref gives the target d declares, making their values canonical:
// each must name one of its parameters, with a value it can have, and each parameter without a default needs one.
// The values of arg()s are only known once the using target is configured, which checks them then.
func (d *declaration) checkArgs(ref *targetRef) error {
	ref.args = slices.Clone(ref.args)
	for i, a := range ref.args {
		j := paramIndex(d.params, a.name)
		if j < 0 {
			return noParam(ref.ref, a.name, d.params)
		}
		if _, ok := a.value.(*argValue); ok {
			continue
		}
		v, err := d.params[j].canonical(a.name, a.value)
		if err != nil {
			return fmt.Errorf("%q: %w", ref.ref, err)
		}
		ref.args[i].value = v
	}
	for _, p := range d.params {
		given := slices.ContainsFunc(ref.args, func(a depArg) bool { return a.name == p.name })
		if p.def == nil && !given {
			hint := ""
			if len(ref.args) == 0 {
				hint = fmt.Sprintf("; give it with dep(%s, %s = ...)", starlark.String(ref.ref), p.name)
			}
			return fmt.Errorf("%q needs a value for its parameter %q, which has no default%s", ref.ref, p.name, hint)
		}
	}
	return nil
}

// freeze freezes the values of ref's arguments, as freezeValueIn does, so that a list given to dep() is fixed too,
// once. dep() freezes them as it is called, having refused a function within them, so they are data.
func (ref *targetRef) freeze() { ref.freezeIn(map[any]bool{}) }

// freezeIn is freeze, with the values in clean, frozen already and holding no function, left as they are.
func (ref *targetRef) freezeIn(clean map[any]bool) {
	if ref.frozen {
		return
	}
	ref.frozen = true
	for _, a := range ref.args {
		freezeValueIn(a.value, clean)
	}
}

// values returns the values of ref's arguments, in order.
func (ref *targetRef) values() []starlark.Value {
	values := make([]starlark.Value, len(ref.args))
	for i, a := range ref.args {
		values[i] = a.value
	}
	return values
}

// argValues returns the arg()s among ref's arguments, in order.
func (ref *targetRef) argValues() []*argValue {
	var found []*argValue
	for _, a := range ref.args {
		if av, ok := a.value.(*argValue); ok {
			found = append(found, av)
		}
	}
	return found
}

// depValue is the value of a call to dep(): a label, with arguments for the target it names,
// which base, add() and an index accept wherever they accept a label.
type depValue struct {
	target  targetRef
	printed *map[*depValue]bool // while a dep() holding it is printed: the dep()s that printing has shown in full
}

// depsWithin returns the dep()s that values hold, through lists, tuples, dicts, dep()s and add()s, each once,
// skipping the values in seen.
func depsWithin(values []starlark.Value, seen map[any]bool) []*depValue {
	var found []*depValue
	for _, v := range values {
		key := identity(v)
		switch v.(type) {
		case *starlark.List, *starlark.Dict, starlark.Tuple, *depValue, *addStep:
			if seen[key] {
				continue
			}
			seen[key] = true
		}
		switch v := v.(type) {
		case *depValue:
			found = append(found, v)
			found = append(found, depsWithin(v.target.values(), seen)...)
		case *addStep:
			found = append(found, depsWithin([]starlark.Value{v.given}, seen)...)
		case *starlark.List, starlark.Tuple:
			found = append(found, depsWithin(slices.Collect(starlark.Elements(v.(starlark.Iterable))), seen)...)
		case *starlark.Dict:
			for _, item := range v.Items() {
				found = append(found, depsWithin([]starlark.Value{item[0], item[1]}, seen)...)
			}
		}
	}
	return found
}

func (v *depValue) Type() string          { return "dep" }
func (v *depValue) Freeze()               { v.target.freeze() }
func (v *depValue) Truth() starlark.Bool  { return true }
func (v *depValue) Hash() (uint32, error) { return 0, errors.New("unhashable type: dep") }

func (v *depValue) String() string {
	// A dep() is shown in full the first time one being printed holds it, and by its label alone after, if it has
	// arguments, so that one held many times over is printed once.
	if v.printed != nil {
		if (*v.printed)[v] {
			if len(v.target.args) > 0 {
				return fmt.Sprintf("dep(%s, ...)", starlark.String(v.target.ref))
			}
			// Without arguments, it holds no dep() to print again, so it is printed in full, as short.
		}
		(*v.printed)[v] = true
	} else {
		printed := map[*depValue]bool{v: true}
		held := append(depsWithin(v.target.values(), map[any]bool{}), v)
		for _, d := range held {
			d.printed = &printed
		}
		defer func() {
			for _, d := range held {
				d.printed = nil
			}
		}()
	}
	var b strings.Builder
	fmt.Fprintf(&b, "dep(%s", starlark.String(v.target.ref))
	switch {
	case v.target.exec:
		b.WriteString(", platform = EXEC_PLATFORM")
	case v.target.platform != nil:
		fmt.Fprintf(&b, ", platform = %s", starlark.String(v.target.platform.String()))
	}
	for _, a := range v.target.args {
		fmt.Fprintf(&b, ", %s = %s", a.name, a.value)
	}
	b.WriteString(")")
	return b.String()
}

// newDep implements dep(label, platform, **args), which names the target label with arguments for its parameters,
// and, if given, for a platform other than the using target's.
// Labels are relative to the package of the BUILD file running,
// or of the target whose attribute a function of ctx is giving.
// The target using it resolves the label, and checks the arguments, as it is made.
func (w *workspace) newDep(
	thread *starlark.Thread, fn *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple,
) (starlark.Value, error) {
	var ref string
	if err := starlark.UnpackPositionalArgs(fn.Name(), args, nil, 1, &ref); err != nil {
		return nil, err
	}
	c := contextOf(thread)
	if ref == scratch {
		return nil, fmt.Errorf(`%s: "scratch" is the empty image, which takes no arguments`, fn.Name())
	}
	if c.file == nil && !strings.HasPrefix(ref, "//") {
		return nil, fmt.Errorf(`%s: label %q: as a .star file is loaded, labels must start with "//"`, fn.Name(), ref)
	}
	target := targetRef{ref: ref, viaDep: true}
	if c.file != nil {
		target.pkg, target.pos = c.file.pkg, callerPos(thread)
	}
	if _, _, err := parseLabel(ref, target.pkg); err != nil {
		return nil, fmt.Errorf("%s: %w", fn.Name(), err)
	}
	for _, kv := range kwargs {
		name := string(kv[0].(starlark.String)) // Starlark requires keywords to be strings
		if name != "platform" {
			target.args = append(target.args, depArg{name: name, value: kv[1]})
			continue
		}
		switch v := kv[1].(type) {
		case execPlatformValue:
			target.exec = true
		case starlark.String:
			p, err := parsePlatform(string(v))
			if err != nil {
				return nil, fmt.Errorf("%s: %w", fn.Name(), err)
			}
			target.platform = p
		default:
			return nil, fmt.Errorf(`%s: platform is %s, want a platform, such as "linux/arm64", or EXEC_PLATFORM`,
				fn.Name(), v.Type())
		}
	}
	// A parameter's value is never a function, so one within an argument is refused now, which leaves the arguments
	// data, which are fixed as dep() is called, as a rule's are, so that the target is configured with them.
	for _, a := range target.args {
		if where, ok := functionWithin(a.value, c.clean, map[any]bool{}); ok {
			slices.Reverse(where)
			return nil, fmt.Errorf("%s: %s%s is a function, which no parameter's value can be", fn.Name(), a.name,
				strings.Join(where, ""))
		}
	}
	c.addRef(refLabel{target.pkg, target.ref})
	// What one dep() froze, another, given the same values, as a loop or a function of ctx may give them, skips.
	if c.clean == nil {
		c.clean = map[any]bool{}
	}
	target.freezeIn(c.clean)
	return &depValue{target: target}, nil
}

// argValue is the value of a call to arg():
// the value of a parameter of the target using dep(), passed on as an argument of dep().
type argValue struct {
	name string
	pos  syntax.Position // of the call, where a mistake in it is reported
}

func (v *argValue) Type() string          { return "arg" }
func (v *argValue) Freeze()               {} // immutable
func (v *argValue) Truth() starlark.Bool  { return true }
func (v *argValue) Hash() (uint32, error) { return 0, errors.New("unhashable type: arg") }
func (v *argValue) String() string        { return fmt.Sprintf("arg(%s)", starlark.String(v.name)) }

// newArg implements arg(name).
// The target declaring the dep() that holds it checks name,
// since it isn't declared yet: a call's arguments run before the call.
func newArg(
	thread *starlark.Thread, fn *starlark.Builtin, args starlark.Tuple, kwargs []starlark.Tuple,
) (starlark.Value, error) {
	var name string
	if err := starlark.UnpackPositionalArgs(fn.Name(), args, kwargs, 1, &name); err != nil {
		return nil, err
	}
	return &argValue{name: name, pos: thread.CallFrame(1).Pos}, nil
}

// argsIn returns the arg()s that v, an attribute of a target, passes on:
// in its dep()s, and those of its add() steps, in order.
func argsIn(v starlark.Value) []*argValue {
	return argsWithin(v, map[any]bool{})
}

// argsWithin is argsIn, skipping the lists, tuples and dicts in seen, so that one holding itself ends the walk, and
// one held many times over is walked once.
func argsWithin(v starlark.Value, seen map[any]bool) []*argValue {
	key := identity(v)
	switch v.(type) {
	case *starlark.List, *starlark.Dict, starlark.Tuple:
		if seen[key] {
			return nil
		}
		seen[key] = true
	}
	switch v := v.(type) {
	case *depValue:
		return v.target.argValues()
	case *addStep:
		return v.target.argValues()
	case *starlark.List, starlark.Tuple:
		var found []*argValue
		for e := range starlark.Elements(v.(starlark.Iterable)) {
			found = append(found, argsWithin(e, seen)...)
		}
		return found
	case *starlark.Dict:
		var found []*argValue
		for _, item := range v.Items() {
			found = append(found, argsWithin(item[1], seen)...)
		}
		return found
	}
	return nil
}

// checkArgNames reports an arg() among values, attributes of the target,
// which declares params, that names a parameter the target doesn't declare.
// It is reported where arg() is called.
func checkArgNames(target string, params []*param, values ...starlark.Value) error {
	for _, v := range values {
		for _, av := range argsIn(v) {
			if paramIndex(params, av.name) < 0 {
				return &posError{pos: av.pos, err: fmt.Errorf("arg: %q has no parameter %q to pass on%s",
					target, av.name, paramHint(av.name, params))}
			}
		}
	}
	return nil
}

// posError is an error at pos, in a BUILD or .star file, rather than at the call that returns it.
type posError struct {
	pos syntax.Position
	err error
}

func (e *posError) Error() string { return fmt.Sprintf("%s: %s", e.pos, e.err) }
func (e *posError) Unwrap() error { return e.err }

// edge returns the target that ref names from the target from, in its configuration: for the platform that ref gives,
// or else from's, with the arguments that ref gives, its arg()s given from's values, and defaults for the rest.
func (w *workspace) edge(ref targetRef, from confLabel) (confLabel, error) {
	if ref.label == scratch {
		return confLabel{label: scratch}, nil
	}
	platform := from.cfg.platform
	switch {
	case ref.exec:
		platform = w.execPlatform
	case ref.platform != nil:
		platform = ref.platform
	}
	d := w.declaration(ref.label)
	if d.kind == "glob" {
		// A glob's files are the same for every platform, so every platform's targets share one walk of them.
		platform = w.platform
	}
	values := make([]starlark.Value, len(d.params))
	for i, p := range d.params {
		values[i] = p.def
	}
	for _, a := range ref.args {
		i := paramIndex(d.params, a.name)
		av, ok := a.value.(*argValue)
		if !ok {
			values[i] = a.value
			continue
		}
		j := paramIndex(w.declaration(from.label).params, av.name)
		if j < 0 { // checkArgNames reports this first
			return confLabel{}, fmt.Errorf("%s: arg: %q has no parameter %q to pass on", av.pos, from.label, av.name)
		}
		v, err := d.params[i].canonical(a.name, from.cfg.values[j])
		if err != nil {
			return confLabel{}, fmt.Errorf("%s: arg: %s: %w (configuring %s)",
				av.pos, starlark.String(ref.ref), err, w.describeConfigured(from))
		}
		values[i] = v
	}
	return confLabel{ref.label, w.intern(platform, d.params, values)}, nil
}

// targetRefOf returns the target that v, the argument param of a call to fnName, names: a label, or dep().
func targetRefOf(c *evalContext, fnName, param string, v starlark.Value) (targetRef, error) {
	switch v := v.(type) {
	case starlark.String:
		return targetRef{ref: string(v), pkg: c.file.pkg}, nil
	case *depValue:
		return v.target, nil
	}
	return targetRef{}, fmt.Errorf("%s: %s is %s, want a label or dep()", fnName, param, v.Type())
}

// configurer works out the targets a build needs, configuring each once.
type configurer struct {
	w       *workspace
	pulls   map[confLabel]*pullSpec
	targets map[confLabel]*targetSpec
	globs   map[confLabel]*fileSet
	indexes map[confLabel][]confLabel // each index's manifests, in order
	path    []string                  // labels of the targets being configured, outermost first, to report a cycle
	// pinning says that targets are configured only to find the pulls they use, for vamos update,
	// so that a target that mounts caches in an unnamed workspace, which can't be built, still names its pulls.
	pinning bool
	undo    []func() // while pinning: what forgets each target configured, in order, for mark
}

// configure returns the graph that building roots needs: the targets,
// and those they use, each in the configuration it is used in.
// It returns the files of each glob in it too, by node, for readGlobs to read.
func (w *workspace) configure(roots []confLabel) (*graph.Graph, map[graph.Ref]*fileSet, error) {
	cr := &configurer{
		w:       w,
		pulls:   map[confLabel]*pullSpec{},
		targets: map[confLabel]*targetSpec{},
		globs:   map[confLabel]*fileSet{},
		indexes: map[confLabel][]confLabel{},
	}
	for _, cl := range roots {
		if err := cr.image(cl); err != nil {
			return nil, nil, err
		}
	}
	l := &lowering{
		w:     w,
		cr:    cr,
		g:     &graph.Graph{Version: graph.Version, ExecPlatform: platforms.Format(w.execPlatform.spec)},
		refs:  map[confLabel]graph.Ref{},
		globs: map[*fileSet]graph.Ref{},
		sets:  map[graph.Ref]*fileSet{},
	}
	for _, cl := range roots {
		ref, err := l.image(cl)
		if err != nil {
			return nil, nil, err
		}
		if !slices.Contains(l.g.Roots, ref) {
			l.g.Roots = append(l.g.Roots, ref)
		}
	}
	return l.g, l.sets, nil
}

// lowering turns the targets a configurer made into the nodes of a graph,
// each after the nodes it uses, in the order the configurer made them.
type lowering struct {
	w     *workspace
	cr    *configurer
	g     *graph.Graph
	refs  map[confLabel]graph.Ref // of pulls, layers and tasks
	globs map[*fileSet]graph.Ref
	sets  map[graph.Ref]*fileSet // the files of each glob's node
}

// add appends n to the graph, returning its ref.
func (l *lowering) add(n graph.Node) graph.Ref {
	l.g.Nodes = append(l.g.Nodes, n)
	return graph.Ref(len(l.g.Nodes) - 1)
}

// image returns the node of the pull, layer, task or index cl, adding it, and the nodes it uses, if it isn't yet.
func (l *lowering) image(cl confLabel) (graph.Ref, error) {
	if cl.label == scratch {
		return graph.Scratch, nil
	}
	if ref, ok := l.refs[cl]; ok {
		return ref, nil
	}
	n := graph.Node{Label: l.w.display(cl.label), Pos: l.w.declaration(cl.label).pos.String()}
	if p, ok := l.cr.pulls[cl]; ok {
		n.Desc, n.Target = l.w.describe("pull", cl), l.w.targetIdentity(cl)
		image := p.image.String()
		n.Pull = &graph.Pull{Image: image, Platform: cl.cfg.platform.String(), Digest: l.w.lock.Pulls[image].Digest}
	} else if manifests, ok := l.cr.indexes[cl]; ok {
		n.Desc = l.w.describe("index", cl)
		n.Index = &graph.Index{}
		for _, m := range manifests {
			ref, err := l.image(m)
			if err != nil {
				return 0, err
			}
			n.Index.Manifests = append(n.Index.Manifests, ref)
		}
	} else {
		s := l.cr.targets[cl]
		base, err := l.image(s.base)
		if err != nil {
			return 0, err
		}
		steps := make([]graph.Step, len(s.steps))
		for i, in := range s.steps {
			steps[i] = in.lower()
			if set := in.source(); set != nil {
				from, err := l.set(set)
				if err != nil {
					return 0, err
				}
				steps[i].From = &from
			}
		}
		n.Target = l.w.targetIdentity(cl)
		if s.task {
			n.Desc = l.w.describe("task", cl)
			n.Task = &graph.Task{Base: base, Steps: steps, ToolCaches: s.mounts}
			if s.output != nil {
				n.Task.Output = &graph.Output{Source: s.source, Selection: s.output.sel}
			}
		} else {
			n.Desc = l.w.describe("layer", cl)
			platform := cl.cfg.platform.String() // its base's too, as configuring it checks
			n.Layer = &graph.Layer{
				Base: base, Platform: platform, Steps: steps, Command: s.command, Entrypoint: s.entrypoint,
				ToolCaches: s.mounts,
			}
		}
	}
	ref := l.add(n)
	l.refs[cl] = ref
	return ref, nil
}

// set returns the node whose files set is: a glob's, adding it if it isn't yet, or a task's.
func (l *lowering) set(set *fileSet) (graph.Ref, error) {
	if set.task != nil {
		return l.image(set.task.id)
	}
	if ref, ok := l.globs[set]; ok {
		return ref, nil
	}
	label := l.w.display(set.label)
	ref := l.add(graph.Node{
		Label:  label,
		Desc:   fmt.Sprintf("glob %q", label),
		Pos:    l.w.declaration(set.label).pos.String(),
		Source: &graph.Source{},
	})
	l.globs[set] = ref
	l.sets[ref] = set
	return ref, nil
}

// describe names the target cl, declared by kind, for messages, e.g. `layer "app"` or `task "//ui:build"`.
// A target for another platform than the command's,
// or given arguments other than its defaults, is described with them,
// e.g. `task "build" (platform=linux/arm64, mode="debug")`.
func (w *workspace) describe(kind string, cl confLabel) string {
	name := fmt.Sprintf("%s %q", kind, w.display(cl.label))
	if diffs := w.describeConfig(cl); diffs != "" {
		name += " (" + diffs + ")"
	}
	return name
}

// targetIdentity returns the identity of the target cl in its configuration, as graph.Node.Target gives it:
// its canonical label, its platform and every parameter's value, sorted by name,
// so that neither the command's platform, nor which values are defaults, nor the order of declaration changes it.
func (w *workspace) targetIdentity(cl confLabel) string {
	var b strings.Builder
	b.WriteString(cl.label)
	b.WriteString(" platform=" + cl.cfg.platform.String())
	params := w.declaration(cl.label).params
	order := make([]int, len(params))
	for i := range order {
		order[i] = i
	}
	slices.SortFunc(order, func(i, j int) int { return strings.Compare(params[i].name, params[j].name) })
	for _, i := range order {
		fmt.Fprintf(&b, " %s=%s", params[i].name, cl.cfg.values[i])
	}
	return b.String()
}

// indexManifests returns the manifests of the index cl, each a pull or layer in its configuration, in order.
func (w *workspace) indexManifests(cl confLabel) ([]confLabel, error) {
	tmpl, err := w.template(w.declaration(cl.label), cl)
	if err != nil {
		return nil, err
	}
	var manifests []confLabel
	for _, ref := range tmpl.(*indexSpec).manifests {
		m, err := w.edge(ref, cl)
		if err != nil {
			return nil, err
		}
		manifests = append(manifests, m)
	}
	return manifests, nil
}

// platformOf returns the platform that the target cl is configured for.
func platformOf(cl confLabel) *platformValue {
	return cl.cfg.platform
}

// chooseManifest returns the manifest of the index l for platform, for a destination that takes one image.
func (w *workspace) chooseManifest(l string, manifests []confLabel, platform imgspecv1.Platform, why string) (
	confLabel, error) {
	matcher := platforms.NewMatcher(platform)
	var found []string
	for _, m := range manifests {
		if matcher.Match(platformOf(m).spec) {
			return m, nil
		}
		found = append(found, platformOf(m).String())
	}
	return confLabel{}, fmt.Errorf("index %q has no manifest for %s, %s; it has %s",
		w.display(l), platforms.Format(platform), why, fileset.QuoteNames(found))
}

// enter records that the target l is being configured, reporting a cycle if it already is, in any configuration:
// a target can't use itself, even in another configuration.
// It sees only the targets being configured, so a label repeated through a target configured already goes unseen,
// which the configured graph, having no cycle, allows.
// The caller must call leave.
func (cr *configurer) enter(l string) error {
	if i := slices.Index(cr.path, l); i >= 0 {
		var cycle []string
		for _, p := range append(slices.Clone(cr.path[i:]), l) {
			cycle = append(cycle, cr.w.display(p))
		}
		return &cycleError{cycle: cycle}
	}
	cr.path = append(cr.path, l)
	return nil
}

// cycleError reports targets that use each other in a cycle, found as they are configured.
type cycleError struct {
	cycle []string // the targets' labels, as displayed, the first again at the end
	// The declaration of the target whose reference closes the cycle, and the reference, such as base "b",
	// once that target has said; and its configuration, if a function of ctx made the reference.
	at, configuring string
}

func (e *cycleError) Error() string {
	msg := "targets use each other in a cycle: " + strings.Join(e.cycle, " -> ")
	if e.at != "" {
		msg = e.at + ": " + msg
	}
	if e.configuring != "" {
		msg += " (configuring " + e.configuring + ")"
	}
	return msg
}

// placeCycle gives err, if it is a cycle that no reference has closed yet, the reference that closes it:
// the target that d declares, configured as cl, names the first of the cycle as what, such as base "b".
func (cr *configurer) placeCycle(err error, d *declaration, cl confLabel, what string) error {
	var cycle *cycleError
	if errors.As(err, &cycle) && cycle.at == "" {
		cycle.at = fmt.Sprintf("%s: %s: %s", d.pos, d.kind, what)
		if d.usesCtx {
			cycle.configuring = cr.w.describeConfigured(cl)
		}
	}
	return err
}

// leave records that the target being configured last is done.
func (cr *configurer) leave() { cr.path = cr.path[:len(cr.path)-1] }

// mark returns what restores cr, which is pinning, to what it has configured now, forgetting what it configures after,
// for a target whose configuration fails part way, and what keeps that instead, once the target is configured.
func (cr *configurer) mark() (forget, keep func()) {
	n := len(cr.undo)
	forget = func() {
		for _, undo := range slices.Backward(cr.undo[n:]) {
			undo()
		}
		cr.undo, cr.path = cr.undo[:n], nil
	}
	keep = func() { cr.undo = cr.undo[:n] }
	return forget, keep
}

// configured records that cl is configured, as undo forgets, if cr is pinning, for mark.
func (cr *configurer) configured(undo func()) {
	if cr.pinning {
		cr.undo = append(cr.undo, undo)
	}
}

// image configures the pull, layer, task or index cl, and the targets it uses.
// Scratch needs none.
func (cr *configurer) image(cl confLabel) error {
	if cl.label == scratch {
		return nil
	}
	if _, ok := cr.pulls[cl]; ok {
		return nil
	}
	if _, ok := cr.targets[cl]; ok {
		return nil
	}
	if _, ok := cr.indexes[cl]; ok {
		return nil
	}
	d := cr.w.declaration(cl.label)
	tmpl, err := cr.w.template(d, cl)
	if err != nil {
		return err
	}
	if p, ok := tmpl.(*pullSpec); ok {
		instance := &pullSpec{label: p.label, id: cl, image: p.image}
		cr.pulls[cl] = instance
		cr.configured(func() { delete(cr.pulls, cl) })
		return nil
	}
	if idx, ok := tmpl.(*indexSpec); ok {
		// Only the command line, or a publication it names, names an index, so it can't be in a cycle.
		var manifests []confLabel
		for _, ref := range idx.manifests {
			m, err := cr.w.edge(ref, cl)
			if err != nil {
				return err
			}
			if err := cr.image(m); err != nil {
				return err
			}
			manifests = append(manifests, m)
		}
		cr.indexes[cl] = manifests
		cr.configured(func() { delete(cr.indexes, cl) })
		return nil
	}

	t := tmpl.(*targetSpec)
	if err := cr.enter(cl.label); err != nil {
		return err
	}
	defer cr.leave()
	s := &targetSpec{
		label:      t.label,
		id:         cl,
		task:       t.task,
		command:    t.command,
		entrypoint: t.entrypoint,
		source:     t.source,
		mounts:     t.mounts,
	}
	if !cr.pinning {
		if err := cr.w.checkNamedFor(d, cl, s); err != nil {
			return err
		}
	}
	base, err := cr.w.edge(t.baseRef, cl)
	if err != nil {
		return err
	}
	if err := cr.image(base); err != nil {
		return cr.placeCycle(err, d, cl, fmt.Sprintf("base %q", t.baseRef.ref))
	}
	// A layer's base must be for its platform, since the base's files are; dep() can give it another. A task's base
	// may be for any, since only its output, if any, is kept.
	if !t.task && base.label != scratch && base.cfg.platform.String() != cl.cfg.platform.String() {
		return fmt.Errorf("%s: layer: its base %s is for %s, not %s; a layer's base must be for its platform "+
			"(configuring %s)", d.pos, cr.w.display(base.label), base.cfg.platform, cl.cfg.platform,
			cr.w.describeConfigured(cl))
	}
	s.base = base
	for _, in := range t.steps {
		if add, ok := in.(*addStep); ok {
			used, err := cr.w.edge(add.target, cl)
			if err != nil {
				return err
			}
			instance := *add
			if instance.set, err = cr.set(used); err != nil {
				return cr.placeCycle(err, d, cl, fmt.Sprintf("add: source %q", add.target.ref))
			}
			in = &instance
		}
		s.steps = append(s.steps, in)
	}
	if t.output != nil {
		s.output = &fileSet{label: t.label, task: s, sel: t.output.sel}
	}
	cr.targets[cl] = s
	cr.configured(func() { delete(cr.targets, cl) })
	return nil
}

// set configures the glob or task cl, returning its files.
func (cr *configurer) set(cl confLabel) (*fileSet, error) {
	d := cr.w.declaration(cl.label)
	if d.kind == "task" {
		if err := cr.image(cl); err != nil {
			return nil, err
		}
		return cr.targets[cl].output, nil
	}
	if set, ok := cr.globs[cl]; ok {
		return set, nil
	}
	tmpl, err := cr.w.template(d, cl)
	if err != nil {
		return nil, err
	}
	set := tmpl.(*fileSet)
	if d.usesCtx {
		set.configuring = cr.w.describeConfigured(cl)
	}
	cr.globs[cl] = set
	cr.configured(func() { delete(cr.globs, cl) })
	return set, nil
}

// template returns the template of the target that d declares, in the configuration of cl, making it from the
// arguments of the call declaring it, which resolves the targets it names, loading their packages:
// once, if no argument is a function of ctx, or else in each configuration, with what each function returns in its
// place.
// A mistake is reported at the call, or where a value in it was made, such as an add() or arg(), with the
// configuration only for a template that depends on it.
func (w *workspace) template(d *declaration, cl confLabel) (any, error) {
	if d.static != nil {
		return d.static, nil
	}
	c := &evalContext{mode: configuring, file: d.file, target: d}
	thread := &starlark.Thread{Name: d.file.path}
	thread.SetLocal(contextKey, c)
	defer cancelWith(w.ctx, thread)()
	ctx := &ctxValue{params: d.params, cfg: cl.cfg, glob: d.kind == "glob", exec: w.execPlatform}
	var values []starlark.Value // the arguments, as the functions give them, whose arg()s are checked
	call := func(param string, v starlark.Value) (starlark.Value, error) {
		fn, ok := v.(starlark.Callable)
		if !ok {
			values = append(values, v)
			return v, nil
		}
		result, err := starlark.Call(thread, fn, starlark.Tuple{ctx}, nil)
		if err != nil {
			return nil, w.interrupted(err)
		}
		if _, ok := result.(starlark.Callable); ok {
			return nil, fmt.Errorf("%s: %s gave a function, not the value of %s", d.kind, fn.Name(), param)
		}
		values = append(values, result)
		return result, nil
	}

	args := make(starlark.Tuple, len(d.args))
	kwargs := make([]starlark.Tuple, len(d.kwargs))
	var err error
	for i, v := range d.args {
		if args[i], err = call(fmt.Sprintf("argument %d", i+1), v); err != nil {
			break
		}
	}
	for i, kv := range d.kwargs {
		if err != nil {
			break
		}
		var v starlark.Value
		v, err = call(string(kv[0].(starlark.String)), kv[1])
		kwargs[i] = starlark.Tuple{kv[0], v}
	}
	if err == nil {
		err = checkArgNames(w.display(cl.label), d.params, values...)
	}
	var tmpl any
	if err == nil {
		_, tmpl, err = d.make(c, d.fn, args, kwargs)
	}
	if err == nil {
		if !d.usesCtx {
			d.static = tmpl
		}
		return tmpl, nil
	}
	err = placeMistake(d, err)
	if !d.usesCtx || !isMistake(err) {
		return nil, err
	}
	return nil, fmt.Errorf("%w (configuring %s)", err, w.describeConfigured(cl))
}

// placeMistake returns err, from making the template of the target that d declares, as it is reported: at the call
// declaring it, or where the value at fault was made, such as an add() or arg().
// An interruption, or an error loading another package's BUILD file, which already says where it is, is returned as
// it is.
func placeMistake(d *declaration, err error) error {
	if nested, ok := errors.AsType[*loadError](err); ok {
		return nested
	}
	if !isMistake(err) {
		return err
	}
	if evalErr, ok := errors.AsType[*starlark.EvalError](err); ok {
		return errors.New(describeEvalError(evalErr))
	} else if at, ok := errors.AsType[*posError](err); ok {
		return errors.New(at.Error())
	}
	return fmt.Errorf("%s: %w", d.pos, err)
}

// isMistake reports whether err is a mistake in a rule, rather than an interruption or an error loading another
// package's BUILD file.
func isMistake(err error) bool {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	_, nested := errors.AsType[*loadError](err)
	return !nested
}

// kwargsValues returns the values of the keyword arguments of the call declaring d, in order.
func (d *declaration) kwargsValues() []starlark.Value {
	values := make([]starlark.Value, len(d.kwargs))
	for i, kv := range d.kwargs {
		values[i] = kv[1]
	}
	return values
}

// givenRefs resolves the targets that the arguments of d, the target l, name as they are given, not by a function of
// ctx, as making its template would, loading their packages: its base, the add()s of its steps and its tool_caches, an
// index's layer, and a publication's images and checks; and checks the arg()s they pass on. It returns them.
// It finds the packages a target uses, and its mistakes, where its configuration can't be made.
func (w *workspace) givenRefs(d *declaration, l string) ([]use, error) {
	var used []use
	c := &evalContext{mode: configuring, file: d.file, target: d}
	given := func(param string) (starlark.Value, bool) {
		v, ok := d.given[param]
		if _, isFunction := v.(starlark.Callable); isFunction {
			return nil, false
		}
		return v, ok
	}
	var values []starlark.Value // in the order given, as template checks them
	for _, v := range append(slices.Clone(d.args), d.kwargsValues()...) {
		if _, isFunction := v.(starlark.Callable); !isFunction {
			values = append(values, v)
		}
	}
	err := checkArgNames(w.display(l), d.params, values...)
	if base, ok := given("base"); ok && err == nil {
		var ref targetRef
		if ref, err = w.makeBase(c, d.fn, base); err == nil && ref.label != scratch {
			used = append(used, use{ref.label, fmt.Sprintf("base %q", ref.ref)})
		}
	}
	if steps, ok := given("steps"); ok && err == nil {
		if list, ok := steps.(*starlark.List); ok {
			// Only the add()s among them: the rest are checked as the target is configured.
			var adds []starlark.Value
			for e := range list.Elements() {
				if add, ok := e.(*addStep); ok {
					adds = append(adds, add)
				}
			}
			var steps []step
			steps, err = w.stepList(c, d.fn.Name(), starlark.NewList(adds))
			for _, in := range steps {
				add := in.(*addStep)
				used = append(used, use{add.target.label, fmt.Sprintf("add: source %q", add.target.ref)})
			}
		}
	}
	if caches, ok := given("tool_caches"); ok && err == nil {
		if list, ok := caches.(*starlark.List); ok {
			name := l[strings.LastIndexByte(l, ':')+1:] // as making the template names the target
			_, err = w.resolveMounts(c, fmt.Sprintf("%s %q", d.fn.Name(), name), list)
		}
	}
	// What else names a target, as dict values or list elements, by the parameter it is given for.
	resolve := func(param string, v starlark.Value, as refUse) {
		if err != nil {
			return
		}
		var ref targetRef
		if ref, err = targetRefOf(c, d.fn.Name(), param, v); err == nil && d.kind == "index" &&
			(ref.exec || ref.platform != nil) {
			at := "platforms[0]" // as makeIndex says of the first, for layer given with platforms, or by a dict
			if param != "layer" {
				at = param + "'s key"
			}
			err = fmt.Errorf("%s: %s: an index's dep() can't give platform, since %s says what each "+
				"manifest is for", d.fn.Name(), param, at)
		}
		if err == nil {
			if _, err = w.resolve(c, d.fn, param, &ref, as); err == nil {
				used = append(used, use{ref.label, fmt.Sprintf("%s %q", param, ref.ref)})
			}
		}
	}
	if layer, ok := given("layer"); ok {
		if dict, ok := layer.(*starlark.Dict); ok {
			for _, item := range dict.Items() {
				resolve(fmt.Sprintf("layer[%s]", item[0]), item[1], useImage)
			}
		} else {
			resolve("layer", layer, useImage)
		}
	}
	if images, ok := given("images"); ok {
		if dict, ok := images.(*starlark.Dict); ok {
			for _, item := range dict.Items() {
				resolve(fmt.Sprintf("images[%s]", item[0]), item[1], usePublished)
			}
		}
	}
	if checks, ok := given("checks"); ok {
		if list, ok := checks.(*starlark.List); ok {
			for i := range list.Len() {
				resolve(fmt.Sprintf("checks[%d]", i), list.Index(i), useCheck)
			}
		}
	}
	if err != nil {
		return nil, placeMistake(d, err)
	}
	return used, nil
}
