package main

import (
	"bufio"
	"container/list"
	"crypto/md5"
	"encoding/hex"
	"fmt"
	"go/build"
	"go/token"
	"go/types"
	"io"
	"io/ioutil"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"unicode"
	"unicode/utf8"
)

type Writer struct {
	Output   io.Writer
	BuildTag string
	Stub     bool
	Context  build.Context

	once sync.Once
	bw   *bufio.Writer

	atEOL bool
	depth int
	scope *list.List

	pkg *types.Package
	std map[string]bool
}

func (w *Writer) Write(p Package) error {
	w.pkg = p.Package

	w.init()
	w.line(`// Code generated by gtrace. DO NOT EDIT.`)

	var hasConstraint bool
	for i, line := range p.BuildConstraints {
		hasConstraint = true
		if i == 0 {
			w.line()
		}
		w.line(line)
	}
	if tag := w.BuildTag; tag != "" {
		if !hasConstraint {
			w.line()
		}
		w.code(`// +build `)
		if w.Stub {
			w.code(`!`)
		}
		w.line(w.BuildTag)
	}
	w.line()
	w.line(`package `, p.Name())
	w.line()

	var deps []dep
	for _, trace := range p.Traces {
		deps = w.traceImports(deps, trace)
	}
	w.importDeps(deps)

	w.newScope(func() {
		for _, trace := range p.Traces {
			w.compose(trace)
			if trace.Flag.Has(GenContext) {
				w.context(trace)
			}
			for _, hook := range trace.Hooks {
				if w.Stub {
					w.stubHook(trace, hook)
				} else {
					w.hook(trace, hook)
				}
			}
		}
		for _, trace := range p.Traces {
			for _, hook := range trace.Hooks {
				if !hook.Flag.Has(GenShortcut) {
					continue
				}
				if w.Stub {
					w.stubHookShortcut(trace, hook)
				} else {
					w.hookShortcut(trace, hook)
				}
			}
		}
	})

	return w.bw.Flush()
}

func (w *Writer) init() {
	w.once.Do(func() {
		w.bw = bufio.NewWriter(w.Output)
		w.scope = list.New()
	})
}

func (w *Writer) mustDeclare(name string) {
	s := w.scope.Back().Value.(*scope)
	if !s.set(name) {
		where := s.where(name)
		panic(fmt.Sprintf(
			"gtrace: can't declare identifier: %q: already defined at %q",
			name, where,
		))
	}
}

func (w *Writer) declare(name string) string {
	if isPredeclared(name) {
		name = firstChar(name)
	}
	s := w.scope.Back().Value.(*scope)
	for i := 0; ; i++ {
		v := name
		if i > 0 {
			v += strconv.Itoa(i)
		}
		if token.IsKeyword(v) {
			continue
		}
		if w.isGlobalScope() && w.pkg.Scope().Lookup(v) != nil {
			continue
		}
		if s.set(v) {
			return v
		}
	}
}

func isPredeclared(name string) bool {
	return types.Universe.Lookup(name) != nil
}

func (w *Writer) isGlobalScope() bool {
	return w.scope.Back().Prev() == nil
}

func (w *Writer) capture(vars ...string) {
	s := w.scope.Back().Value.(*scope)
	for _, v := range vars {
		if !s.set(v) {
			panic(fmt.Sprintf("can't capture variable %q", v))
		}
	}
}

type dep struct {
	pkgPath string
	pkgName string
	typName string
}

func (w *Writer) typeImports(dst []dep, t types.Type) []dep {
	if p, ok := t.(*types.Pointer); ok {
		return w.typeImports(dst, p.Elem())
	}
	n, ok := t.(*types.Named)
	if !ok {
		return dst
	}
	var (
		obj = n.Obj()
		pkg = obj.Pkg()
	)
	if pkg != nil && pkg.Path() != w.pkg.Path() {
		return append(dst, dep{
			pkgPath: pkg.Path(),
			pkgName: pkg.Name(),
			typName: obj.Name(),
		})
	}
	return dst
}

func forEachField(s *types.Struct, fn func(*types.Var)) {
	for i := 0; i < s.NumFields(); i++ {
		fn(s.Field(i))
	}
}

func unwrapStruct(t types.Type) (n *types.Named, s *types.Struct) {
	var ok bool
	n, ok = t.(*types.Named)
	if ok {
		s, _ = n.Underlying().(*types.Struct)
	}
	return
}

func (w *Writer) funcImports(dst []dep, flag GenFlag, fn Func) []dep {
	for _, p := range fn.Params {
		dst = w.typeImports(dst, p.Type)
		if !flag.Has(GenShortcut) {
			continue
		}
		if _, s := unwrapStruct(p.Type); s != nil {
			forEachField(s, func(v *types.Var) {
				if v.Exported() {
					dst = w.typeImports(dst, v.Type())
				}
			})
		}
	}
	for _, fn := range fn.Result {
		dst = w.funcImports(dst, flag, fn)
	}
	return dst
}

func (w *Writer) traceImports(dst []dep, t Trace) []dep {
	if t.Flag.Has(GenContext) {
		dst = append(dst, dep{
			pkgPath: "context",
			pkgName: "context",
			typName: "Context",
		})
	}
	for _, h := range t.Hooks {
		dst = w.funcImports(dst, h.Flag, h.Func)
	}
	return dst
}

func (w *Writer) importDeps(deps []dep) {
	seen := map[string]bool{}
	for i := 0; i < len(deps); {
		d := deps[i]
		if seen[d.pkgPath] {
			n := len(deps)
			deps[i], deps[n-1] = deps[n-1], deps[i]
			deps = deps[:n-1]
			continue
		}
		seen[d.pkgPath] = true
		i++
	}
	if len(deps) == 0 {
		return
	}
	sort.Slice(deps, func(i, j int) bool {
		var (
			d0   = deps[i]
			d1   = deps[j]
			std0 = w.isStdLib(d0.pkgPath)
			std1 = w.isStdLib(d1.pkgPath)
		)
		if std0 != std1 {
			return std0
		}
		return d0.pkgPath < d1.pkgPath
	})
	w.line(`import (`)
	var (
		lastStd bool
	)
	for _, d := range deps {
		if w.isStdLib(d.pkgPath) {
			lastStd = true
		} else if lastStd {
			lastStd = false
			w.line()
		}
		w.line("\t", `"`, d.pkgPath, `"`)
	}
	w.line(`)`)
	w.line()
}

func (w *Writer) isStdLib(pkg string) bool {
	w.ensureStdLibMapping()
	s := strings.Split(pkg, "/")[0]
	return w.std[s]
}

func (w *Writer) ensureStdLibMapping() {
	if w.std != nil {
		return
	}
	w.std = make(map[string]bool)

	src := filepath.Join(w.Context.GOROOT, "src")
	files, err := ioutil.ReadDir(src)
	if err != nil {
		panic(fmt.Sprintf("can't list GOROOT's src: %v", err))
	}
	for _, file := range files {
		if !file.IsDir() {
			continue
		}
		name := filepath.Base(file.Name())
		switch name {
		case
			"cmd",
			"internal":
			// Ignored.

		default:
			w.std[name] = true
		}
	}
}

func (w *Writer) call(args []string) {
	w.code(`(`)
	for i, name := range args {
		if i > 0 {
			w.code(`, `)
		}
		w.code(name)
	}
	w.line(`)`)
}

func (w *Writer) compose(trace Trace) {
	w.newScope(func() {
		t := w.declare("t")
		x := w.declare("x")
		ret := w.declare("ret")
		w.line(`// Compose returns a new `, trace.Name, ` which has functional fields composed`)
		w.line(`// both from `, t, ` and `, x, `.`)
		w.code(`func (`, t, ` `, trace.Name, `) Compose(`, x, ` `, trace.Name, `) `)
		w.line(`(`, ret, ` `, trace.Name, `) {`)
		w.block(func() {
			for _, hook := range trace.Hooks {
				w.composeHook(hook, t, x, ret+"."+hook.Name)
			}
			w.line(`return `, ret)
		})
		w.line(`}`)
	})
}

func (w *Writer) composeHook(hook Hook, t1, t2, dst string) {
	w.line(`switch {`)
	w.line(`case `, t1, `.`, hook.Name, ` == nil:`)
	w.line("\t", dst, ` = `, t2, `.`, hook.Name)
	w.line(`case `, t2, `.`, hook.Name, ` == nil:`)
	w.line("\t", dst, ` = `, t1, `.`, hook.Name)
	w.line(`default:`)
	w.block(func() {
		h1 := w.declare("h1")
		h2 := w.declare("h2")
		w.line(h1, ` := `, t1, `.`, hook.Name)
		w.line(h2, ` := `, t2, `.`, hook.Name)
		w.code(dst, ` = `)
		w.composeHookCall(hook.Func, h1, h2)
	})
	w.line(`}`)
}

func (w *Writer) composeHookCall(fn Func, h1, h2 string) {
	w.newScope(func() {
		w.capture(h1, h2)
		w.block(func() {
			w.capture(h1, h2)
			w.code(`func`)
			args := w.funcParams(fn.Params)
			w.funcResults(fn)
			w.line(`{`)
			var (
				r1 string
				r2 string
				rs []string
			)
			if fn.HasResult() {
				r1 = w.declare("r1")
				r2 = w.declare("r2")
				rs = []string{r1, r2}
			}
			for i, h := range []string{h1, h2} {
				if fn.HasResult() {
					w.code(rs[i], ` := `)
				}
				w.code(h)
				w.call(args)
			}
			if fn.HasResult() {
				w.line(`switch {`)
				w.line(`case `, r1, ` == nil:`)
				w.line("\t", `return `, r2)
				w.line(`case `, r2, ` == nil:`)
				w.line("\t", `return `, r1)
				w.line(`default:`)
				w.block(func() {
					w.code(`return `)
					w.composeHookCall(fn.Result[0], r1, r2)
				})
				w.line(`}`)
			}
		})
		w.line(`}`)
	})
}

var contextType = (func() types.Type {
	pkg := types.NewPackage("context", "context")
	typ := types.NewInterfaceType(nil, nil)
	name := types.NewTypeName(0, pkg, "Context", typ)
	return types.NewNamed(name, typ, nil)
})()

func (w *Writer) stubFunc(id string, f Func) (name string) {
	name = funcName("gtrace", "noop", id)
	name = unexported(name)
	name = w.declare(name)

	var res string
	for _, f := range f.Result {
		res = w.stubFunc(id, f)
	}
	w.newScope(func() {
		w.code(`func `, name)
		w.funcParamsUnused(f.Params)
		w.funcResults(f)
		w.line(`{`)
		if f.HasResult() {
			w.block(func() {
				w.line(`return `, res)
			})
		}
		w.line(`}`)
	})

	return name
}

func (w *Writer) stubHook(trace Trace, hook Hook) {
	var stubName string
	if hook.Func.HasResult() {
		stubName = w.stubFunc(uniqueTraceHookID(trace, hook), hook.Func.Result[0])
	}
	haveNames := haveNames(hook.Func.Params)
	w.newScope(func() {
		w.code(`func (`, trace.Name, `) `, unexported(hook.Name))
		w.code(`(`)
		if trace.Flag.Has(GenContext) {
			if haveNames {
				ctx := w.declare("ctx")
				w.code(ctx, ` `)
			}
			w.code(`context.Context`)
		}
		for i, p := range hook.Func.Params {
			if i > 0 || trace.Flag.Has(GenContext) {
				w.code(`, `)
			}
			if haveNames {
				name := w.declare(nameParam(p))
				w.code(name, ` `)
			}
			w.code(w.typeString(p.Type))
		}
		w.code(`) `)
		w.funcResultsFlags(hook.Func, docs)
		w.line(`{`)
		if hook.Func.HasResult() {
			w.block(func() {
				w.line(`return `, stubName)
			})
		}
		w.line(`}`)
	})
}

func (w *Writer) stubShortcutFunc(id string, f Func) (name string) {
	name = funcName("gtrace", "noop", id)
	name = unexported(name)
	name = w.declare(name)

	var res string
	for _, f := range f.Result {
		res = w.stubShortcutFunc(id, f)
	}
	w.newScope(func() {
		w.code(`func `, name)
		w.code(`(`)
		params := flattenParams(nil, f.Params)
		for i, p := range params {
			if i > 0 {
				w.code(`, `)
			}
			w.code(w.typeString(p.Type))
		}
		w.code(`) `)
		if f.HasResult() {
			w.shortcutFuncSign(f.Result[0])
		}
		w.line(`{`)
		if f.HasResult() {
			w.block(func() {
				w.line(`return `, res)
			})
		}
		w.line(`}`)
	})

	return name
}

func (w *Writer) stubHookShortcut(trace Trace, hook Hook) {
	name := funcName(trace.Name, hook.Name)
	name = unexported(name)
	w.mustDeclare(name)

	var stubName string
	if hook.Func.HasResult() {
		stubName = w.stubShortcutFunc(
			uniqueTraceHookID(trace, hook),
			hook.Func.Result[0],
		)
	}

	params := flattenParams(nil, hook.Func.Params)
	haveNames := haveNames(params)

	w.newScope(func() {
		w.code(`func `, name)
		w.code(`(`)
		if trace.Flag.Has(GenContext) {
			if haveNames {
				ctx := w.declare("ctx")
				w.code(ctx, ` `)
			}
			w.code(`context.Context, `)
		}

		if haveNames {
			t := w.declare("t")
			w.code(t, ` `)
		}
		w.code(trace.Name)

		for _, p := range params {
			w.code(`, `)
			if haveNames {
				name := w.declare(nameParam(p))
				w.code(name, ` `)
			}
			w.code(w.typeString(p.Type))
		}
		w.code(`) `)
		w.shortcutFuncResultsFlags(hook.Func, docs)
		w.line(`{`)
		if hook.Func.HasResult() {
			w.block(func() {
				w.line(`return `, stubName)
			})
		}
		w.line(`}`)
	})
}

func (w *Writer) hook(trace Trace, hook Hook) {
	w.newScope(func() {
		t := w.declare("t")
		x := w.declare("c") // For context's trace.
		fn := w.declare("fn")

		w.code(`func (`, t, ` `, trace.Name, `) `, unexported(hook.Name))

		w.code(`(`)
		var ctx string
		if trace.Flag.Has(GenContext) {
			ctx = w.declare("ctx")
			w.code(ctx, ` context.Context`)
		}
		var args []string
		for i, p := range hook.Func.Params {
			if i > 0 || ctx != "" {
				w.code(`, `)
			}
			args = append(args, w.funcParam(p))
		}
		w.code(`) `)
		w.funcResultsFlags(hook.Func, docs)
		w.line(`{`)
		w.block(func() {
			if ctx != "" {
				w.line(x, ` := Context`, trace.Name, `(`, ctx, `)`)
				w.code(`var fn `)
				w.funcSign(hook.Func)
				w.line()
				w.composeHook(hook, t, x, fn)
			} else {
				w.line(fn, ` := `, t, `.`, hook.Name)
			}
			w.line(`if `, fn, ` == nil {`)
			w.block(func() {
				w.zeroReturn(hook.Func)
			})
			w.line(`}`)

			w.hookFuncCall(hook.Func, fn, args)
		})
		w.line(`}`)
	})
}

func (w *Writer) hookFuncCall(fn Func, name string, args []string) {
	var res string
	if fn.HasResult() {
		res = w.declare("res")
		w.code(res, ` := `)
	}

	w.code(name)
	w.call(args)

	if fn.HasResult() {
		w.line(`if `, res, ` == nil {`)
		w.block(func() {
			w.zeroReturn(fn)
		})
		w.line(`}`)
		if fn.Result[0].HasResult() {
			w.newScope(func() {
				w.code(`return func`)
				args := w.funcParams(fn.Result[0].Params)
				w.funcResults(fn.Result[0])
				w.line(`{`)
				w.block(func() {
					w.hookFuncCall(fn.Result[0], res, args)
				})
				w.line(`}`)
			})
		} else {
			w.line(`return `, res)
		}
	}
}

func (w *Writer) context(trace Trace) {
	w.line()
	w.line(`type `, unexported(trace.Name), `ContextKey struct{}`)
	w.line()

	w.newScope(func() {
		var (
			ctx = w.declare("ctx")
			t   = w.declare("t")
		)
		w.line(`// With`, trace.Name, ` returns context which has associated `, trace.Name, ` with it.`)
		w.code(`func With`, trace.Name, `(`)
		w.code(ctx, ` context.Context, `)
		w.code(t, ` `, trace.Name, `) `)
		w.line(`context.Context {`)
		w.block(func() {
			w.line(`return context.WithValue(`, ctx, `,`)
			w.line("\t", unexported(trace.Name), `ContextKey{},`)
			w.line("\t", `Context`, trace.Name, `(`, ctx, `).Compose(`, t, `),`)
			w.line(`)`)
		})
		w.line(`}`)
		w.line()
	})
	w.newScope(func() {
		var (
			ctx = w.declare("ctx")
			t   = w.declare("t")
		)
		w.line(`// Context`, trace.Name, ` returns `, trace.Name, ` associated with `, ctx, `.`)
		w.line(`// If there is no `, trace.Name, ` associated with `, ctx, ` then zero value `)
		w.line(`// of `, trace.Name, ` is returned.`)
		w.code(`func Context`, trace.Name, `(`, ctx, ` context.Context) `)
		w.line(trace.Name, ` {`)
		w.block(func() {
			w.code(t, `, _ := ctx.Value(`, unexported(trace.Name), `ContextKey{})`)
			w.line(`.(`, trace.Name, `)`)
			w.line(`return `, t)
		})
		w.line(`}`)
		w.line()
	})
}

func nameParam(p Param) (s string) {
	s = p.Name
	if s == "" {
		s = firstChar(ident(typeBasename(p.Type)))
	}
	return unexported(s)
}

func (w *Writer) declareParams(src []Param) (names []string) {
	names = make([]string, len(src))
	for i, p := range src {
		names[i] = w.declare(nameParam(p))
	}
	return names
}

func flattenParams(dst, src []Param) []Param {
	for _, p := range src {
		_, s := unwrapStruct(p.Type)
		if s != nil {
			dst = flattenStruct(dst, s)
			continue
		}
		dst = append(dst, p)
	}
	return dst
}

func typeBasename(t types.Type) (name string) {
	lo, name := rsplit(t.String(), '.')
	if name == "" {
		name = lo
	}
	return name
}

func flattenStruct(dst []Param, s *types.Struct) []Param {
	forEachField(s, func(f *types.Var) {
		if !f.Exported() {
			return
		}
		var (
			name = f.Name()
			typ  = f.Type()
		)
		if name == typeBasename(typ) {
			// NOTE: field name essentially be empty for embeded structs or
			// fields called exactly as type.
			name = ""
		}
		dst = append(dst, Param{
			Name: name,
			Type: typ,
		})
	})
	return dst
}

func (w *Writer) constructParams(params []Param, names []string) (res []string) {
	for _, p := range params {
		n, s := unwrapStruct(p.Type)
		if s != nil {
			var v string
			v, names = w.constructStruct(n, s, names)
			res = append(res, v)
			continue
		}
		name := names[0]
		names = names[1:]
		res = append(res, name)
	}
	return res
}

func (w *Writer) constructStruct(n *types.Named, s *types.Struct, vars []string) (string, []string) {
	p := w.declare("p")
	// TODO Ptr
	// maybe skip pointers from flattening to not allocate anyhing during trace.
	w.line(`var `, p, ` `, w.typeString(n))
	for i := 0; i < s.NumFields(); i++ {
		v := s.Field(i)
		if !v.Exported() {
			continue
		}
		name := vars[0]
		vars = vars[1:]
		w.line(p, `.`, v.Name(), ` = `, name)
	}
	return p, vars
}

func (w *Writer) hookShortcut(trace Trace, hook Hook) {
	name := funcName(trace.Name, hook.Name)
	name = unexported(name)
	w.mustDeclare(name)

	w.newScope(func() {
		t := w.declare("t")
		w.code(`func `, name)
		w.code(`(`)
		var ctx string
		if trace.Flag.Has(GenContext) {
			ctx = w.declare("ctx")
			w.code(ctx, ` context.Context`)
			w.code(`, `)
		}
		w.code(t, ` `, trace.Name)

		var (
			params = flattenParams(nil, hook.Func.Params)
			names  = w.declareParams(params)
		)
		for i, p := range params {
			w.code(`, `)
			w.code(names[i], ` `, w.typeString(p.Type))
		}
		w.code(`) `)
		w.shortcutFuncResultsFlags(hook.Func, docs)
		w.line(`{`)
		w.block(func() {
			for _, name := range names {
				w.capture(name)
			}
			vars := w.constructParams(hook.Func.Params, names)
			var res string
			if hook.Func.HasResult() {
				res = w.declare("res")
				w.code(res, ` := `)
			}
			w.code(t, `.`, unexported(hook.Name))
			if ctx != "" {
				vars = append([]string{ctx}, vars...)
			}
			w.call(vars)
			if hook.Func.HasResult() {
				w.code(`return `)
				w.hookFuncShortcut(hook.Func.Result[0], res)
			}
		})
		w.line(`}`)
	})
}

func (w *Writer) hookFuncShortcut(fn Func, name string) {
	w.newScope(func() {
		w.code(`func(`)
		var (
			params = flattenParams(nil, fn.Params)
			names  = w.declareParams(params)
		)
		for i, p := range params {
			if i > 0 {
				w.code(`, `)
			}
			w.code(names[i], ` `, w.typeString(p.Type))
		}
		w.code(`) `)
		w.shortcutFuncResults(fn)
		w.line(`{`)
		w.block(func() {
			for _, name := range names {
				w.capture(name)
			}
			params := w.constructParams(fn.Params, names)
			var res string
			if fn.HasResult() {
				res = w.declare("res")
				w.code(res, ` := `)
			}
			w.code(name)
			w.call(params)
			if fn.HasResult() {
				w.code(`return `)
				w.hookFuncShortcut(fn.Result[0], res)
			}
		})
		w.line(`}`)
	})
}

func (w *Writer) zeroReturn(fn Func) {
	if !fn.HasResult() {
		w.line(`return`)
		return
	}
	fn = fn.Result[0]
	w.code(`return `)
	w.funcSign(fn)
	w.line(`{`)
	w.block(func() {
		w.zeroReturn(fn)
	})
	w.line(`}`)
}

func (w *Writer) funcParams(params []Param) (vars []string) {
	w.code(`(`)
	for i, p := range params {
		if i > 0 {
			w.code(`, `)
		}
		vars = append(vars, w.funcParam(p))
	}
	w.code(`) `)
	return
}

func (w *Writer) funcParamsUnused(params []Param) {
	w.code(`(`)
	for i, p := range params {
		if i > 0 {
			w.code(`, `)
		}
		w.code(w.typeString(p.Type))
	}
	w.code(`) `)
}

func (w *Writer) funcParam(p Param) (name string) {
	name = w.declare(nameParam(p))
	w.code(name, ` `)
	w.code(w.typeString(p.Type))
	return name
}

func (w *Writer) funcParamSign(p Param) {
	name := nameParam(p)
	if len(name) == 1 || isPredeclared(name) {
		name = "_"
	}
	w.code(name, ` `)
	w.code(w.typeString(p.Type))
}

type flags uint8

func (f flags) has(x flags) bool {
	return f&x != 0
}

const (
	zeroFlags flags = 1 << iota >> 1
	docs
)

func (w *Writer) funcResultsFlags(fn Func, flags flags) {
	for _, fn := range fn.Result {
		w.funcSignFlags(fn, flags)
	}
}

func (w *Writer) funcResults(fn Func) {
	w.funcResultsFlags(fn, 0)
}

func (w *Writer) funcSignFlags(fn Func, flags flags) {
	haveNames := haveNames(fn.Params)
	w.code(`func(`)
	for i, p := range fn.Params {
		if i > 0 {
			w.code(`, `)
		}
		if flags.has(docs) && haveNames {
			w.funcParamSign(p)
		} else {
			w.code(w.typeString(p.Type))
		}
	}
	w.code(`) `)
	w.funcResultsFlags(fn, flags)
}

func (w *Writer) funcSign(fn Func) {
	w.funcSignFlags(fn, 0)
}

func (w *Writer) shortcutFuncSignFlags(fn Func, flags flags) {
	var (
		params    = flattenParams(nil, fn.Params)
		haveNames = haveNames(params)
	)
	w.code(`func(`)
	for i, p := range params {
		if i > 0 {
			w.code(`, `)
		}
		if flags.has(docs) && haveNames {
			w.funcParamSign(p)
		} else {
			w.code(w.typeString(p.Type))
		}
	}
	w.code(`) `)
	w.shortcutFuncResultsFlags(fn, flags)
}

func (w *Writer) shortcutFuncSign(fn Func) {
	w.shortcutFuncSignFlags(fn, 0)
}

func (w *Writer) shortcutFuncResultsFlags(fn Func, flags flags) {
	for _, fn := range fn.Result {
		w.shortcutFuncSignFlags(fn, flags)
	}
}

func (w *Writer) shortcutFuncResults(fn Func) {
	w.shortcutFuncResultsFlags(fn, 0)
}

func haveNames(params []Param) bool {
	for _, p := range params {
		name := nameParam(p)
		if len(name) > 1 && !isPredeclared(name) {
			return true
		}
	}
	return false
}

func (w *Writer) typeString(t types.Type) string {
	return types.TypeString(t, func(pkg *types.Package) string {
		if pkg.Path() == w.pkg.Path() {
			return "" // same package; unqualified
		}
		return pkg.Name()
	})
}

func (w *Writer) tab(n int) {
	w.depth += n
}

func (w *Writer) block(fn func()) {
	w.depth++
	w.newScope(fn)
	w.depth--
}

func (w *Writer) newScope(fn func()) {
	w.scope.PushBack(new(scope))
	fn()
	w.scope.Remove(w.scope.Back())
}

func (w *Writer) line(args ...string) {
	w.code(args...)
	w.bw.WriteByte('\n')
	w.atEOL = true
}

func (w *Writer) code(args ...string) {
	if w.atEOL {
		for i := 0; i < w.depth; i++ {
			w.bw.WriteByte('\t')
		}
		w.atEOL = false
	}
	for _, arg := range args {
		w.bw.WriteString(arg)
	}
}

func exported(s string) string {
	r, size := utf8.DecodeRuneInString(s)
	if r == utf8.RuneError {
		panic("invalid string")
	}
	return string(unicode.ToUpper(r)) + s[size:]
}

func unexported(s string) string {
	r, size := utf8.DecodeRuneInString(s)
	if r == utf8.RuneError {
		panic("invalid string")
	}
	return string(unicode.ToLower(r)) + s[size:]
}

func firstChar(s string) string {
	r, _ := utf8.DecodeRuneInString(s)
	if r == utf8.RuneError {
		panic("invalid string")
	}
	return string(r)
}

func ident(s string) string {
	// Identifier must not begin with number.
	for len(s) > 0 {
		r, size := utf8.DecodeRuneInString(s)
		if r == utf8.RuneError {
			panic("invalid string")
		}
		if !unicode.IsNumber(r) {
			break
		}
		s = s[size:]
	}

	// Filter out non letter/number/underscore characters.
	s = strings.Map(func(r rune) rune {
		switch {
		case r == '_' ||
			unicode.IsLetter(r) ||
			unicode.IsNumber(r):

			return r
		default:
			return -1
		}
	}, s)

	if !token.IsIdentifier(s) {
		s = "_" + s
	}

	return s
}

func funcName(names ...string) string {
	var sb strings.Builder
	for i, name := range names {
		if i == 0 {
			sb.WriteString(name)
		} else {
			sb.WriteString(exported(name))
		}
	}
	return sb.String()
}

type decl struct {
	where string
}

type scope struct {
	vars map[string]decl
}

func (s *scope) set(v string) bool {
	if s.vars == nil {
		s.vars = make(map[string]decl)
	}
	if _, has := s.vars[v]; has {
		return false
	}
	_, file, line, _ := runtime.Caller(2)
	s.vars[v] = decl{
		where: fmt.Sprintf("%s:%d", file, line),
	}
	return true
}

func (s *scope) where(v string) string {
	d := s.vars[v]
	return d.where
}

func uniqueTraceHookID(t Trace, h Hook) string {
	hash := md5.New()
	io.WriteString(hash, t.Name)
	io.WriteString(hash, h.Name)
	p := hash.Sum(nil)
	s := hex.EncodeToString(p)
	return s[:8]
}
