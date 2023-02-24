// Copyright 2018 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// This file implements type parameter inference.

package types2

import (
	"cmd/compile/internal/syntax"
	"fmt"
	"strings"
)

// renameTParams renames the type parameters in a function signature described by its
// type and ordinary parameters (tparams and params) such that each type parameter is
// given a new identity. renameTParams returns the new type and ordinary parameters.
func (check *Checker) renameTParams(pos syntax.Pos, tparams []*TypeParam, params *Tuple) ([]*TypeParam, *Tuple) {
	// For the purpose of type inference we must differentiate type parameters
	// occurring in explicit type or value function arguments from the type
	// parameters we are solving for via unification because they may be the
	// same in self-recursive calls:
	//
	//   func f[P constraint](x P) {
	//           f(x)
	//   }
	//
	// In this example, without type parameter renaming, the P used in the
	// instantation f[P] has the same pointer identity as the P we are trying
	// to solve for through type inference. This causes problems for type
	// unification. Because any such self-recursive call is equivalent to
	// a mutually recursive call, type parameter renaming can be used to
	// create separate, disentangled type parameters. The above example
	// can be rewritten into the following equivalent code:
	//
	//   func f[P constraint](x P) {
	//           f2(x)
	//   }
	//
	//   func f2[P2 constraint](x P2) {
	//           f(x)
	//   }
	//
	// Type parameter renaming turns the first example into the second
	// example by renaming the type parameter P into P2.
	tparams2 := make([]*TypeParam, len(tparams))
	for i, tparam := range tparams {
		tname := NewTypeName(tparam.Obj().Pos(), tparam.Obj().Pkg(), tparam.Obj().Name(), nil)
		tparams2[i] = NewTypeParam(tname, nil)
		tparams2[i].index = tparam.index // == i
	}

	renameMap := makeRenameMap(tparams, tparams2)
	for i, tparam := range tparams {
		tparams2[i].bound = check.subst(pos, tparam.bound, renameMap, nil, check.context())
	}

	return tparams2, check.subst(pos, params, renameMap, nil, check.context()).(*Tuple)
}

// typeParamsString produces a string containing all the type parameter names
// in list suitable for human consumption.
func typeParamsString(list []*TypeParam) string {
	// common cases
	n := len(list)
	switch n {
	case 0:
		return ""
	case 1:
		return list[0].obj.name
	case 2:
		return list[0].obj.name + " and " + list[1].obj.name
	}

	// general case (n > 2)
	var buf strings.Builder
	for i, tname := range list[:n-1] {
		if i > 0 {
			buf.WriteString(", ")
		}
		buf.WriteString(tname.obj.name)
	}
	buf.WriteString(", and ")
	buf.WriteString(list[n-1].obj.name)
	return buf.String()
}

// isParameterized reports whether typ contains any of the type parameters of tparams.
func isParameterized(tparams []*TypeParam, typ Type) bool {
	w := tpWalker{
		seen:    make(map[Type]bool),
		tparams: tparams,
	}
	return w.isParameterized(typ)
}

type tpWalker struct {
	seen    map[Type]bool
	tparams []*TypeParam
}

func (w *tpWalker) isParameterized(typ Type) (res bool) {
	// detect cycles
	if x, ok := w.seen[typ]; ok {
		return x
	}
	w.seen[typ] = false
	defer func() {
		w.seen[typ] = res
	}()

	switch t := typ.(type) {
	case nil, *Basic: // TODO(gri) should nil be handled here?
		break

	case *Array:
		return w.isParameterized(t.elem)

	case *Slice:
		return w.isParameterized(t.elem)

	case *Struct:
		for _, fld := range t.fields {
			if w.isParameterized(fld.typ) {
				return true
			}
		}

	case *Pointer:
		return w.isParameterized(t.base)

	case *Tuple:
		n := t.Len()
		for i := 0; i < n; i++ {
			if w.isParameterized(t.At(i).typ) {
				return true
			}
		}

	case *Signature:
		// t.tparams may not be nil if we are looking at a signature
		// of a generic function type (or an interface method) that is
		// part of the type we're testing. We don't care about these type
		// parameters.
		// Similarly, the receiver of a method may declare (rather then
		// use) type parameters, we don't care about those either.
		// Thus, we only need to look at the input and result parameters.
		return w.isParameterized(t.params) || w.isParameterized(t.results)

	case *Interface:
		tset := t.typeSet()
		for _, m := range tset.methods {
			if w.isParameterized(m.typ) {
				return true
			}
		}
		return tset.is(func(t *term) bool {
			return t != nil && w.isParameterized(t.typ)
		})

	case *Map:
		return w.isParameterized(t.key) || w.isParameterized(t.elem)

	case *Chan:
		return w.isParameterized(t.elem)

	case *Named:
		return w.isParameterizedTypeList(t.TypeArgs().list())

	case *TypeParam:
		// t must be one of w.tparams
		return tparamIndex(w.tparams, t) >= 0

	default:
		unreachable()
	}

	return false
}

func (w *tpWalker) isParameterizedTypeList(list []Type) bool {
	for _, t := range list {
		if w.isParameterized(t) {
			return true
		}
	}
	return false
}

// If the type parameter has a single specific type S, coreTerm returns (S, true).
// Otherwise, if tpar has a core type T, it returns a term corresponding to that
// core type and false. In that case, if any term of tpar has a tilde, the core
// term has a tilde. In all other cases coreTerm returns (nil, false).
func coreTerm(tpar *TypeParam) (*term, bool) {
	n := 0
	var single *term // valid if n == 1
	var tilde bool
	tpar.is(func(t *term) bool {
		if t == nil {
			assert(n == 0)
			return false // no terms
		}
		n++
		single = t
		if t.tilde {
			tilde = true
		}
		return true
	})
	if n == 1 {
		if debug {
			assert(debug && under(single.typ) == coreType(tpar))
		}
		return single, true
	}
	if typ := coreType(tpar); typ != nil {
		// A core type is always an underlying type.
		// If any term of tpar has a tilde, we don't
		// have a precise core type and we must return
		// a tilde as well.
		return &term{tilde, typ}, false
	}
	return nil, false
}

type cycleFinder struct {
	tparams []*TypeParam
	types   []Type
	seen    map[Type]bool
}

func (w *cycleFinder) typ(typ Type) {
	if w.seen[typ] {
		// We have seen typ before. If it is one of the type parameters
		// in tparams, iterative substitution will lead to infinite expansion.
		// Nil out the corresponding type which effectively kills the cycle.
		if tpar, _ := typ.(*TypeParam); tpar != nil {
			if i := tparamIndex(w.tparams, tpar); i >= 0 {
				// cycle through tpar
				w.types[i] = nil
			}
		}
		// If we don't have one of our type parameters, the cycle is due
		// to an ordinary recursive type and we can just stop walking it.
		return
	}
	w.seen[typ] = true
	defer delete(w.seen, typ)

	switch t := typ.(type) {
	case *Basic:
		// nothing to do

	case *Array:
		w.typ(t.elem)

	case *Slice:
		w.typ(t.elem)

	case *Struct:
		w.varList(t.fields)

	case *Pointer:
		w.typ(t.base)

	// case *Tuple:
	//      This case should not occur because tuples only appear
	//      in signatures where they are handled explicitly.

	case *Signature:
		if t.params != nil {
			w.varList(t.params.vars)
		}
		if t.results != nil {
			w.varList(t.results.vars)
		}

	case *Union:
		for _, t := range t.terms {
			w.typ(t.typ)
		}

	case *Interface:
		for _, m := range t.methods {
			w.typ(m.typ)
		}
		for _, t := range t.embeddeds {
			w.typ(t)
		}

	case *Map:
		w.typ(t.key)
		w.typ(t.elem)

	case *Chan:
		w.typ(t.elem)

	case *Named:
		for _, tpar := range t.TypeArgs().list() {
			w.typ(tpar)
		}

	case *TypeParam:
		if i := tparamIndex(w.tparams, t); i >= 0 && w.types[i] != nil {
			w.typ(w.types[i])
		}

	default:
		panic(fmt.Sprintf("unexpected %T", typ))
	}
}

func (w *cycleFinder) varList(list []*Var) {
	for _, v := range list {
		w.typ(v.typ)
	}
}

// If tpar is a type parameter in list, tparamIndex returns the type parameter index.
// Otherwise, the result is < 0. tpar must not be nil.
func tparamIndex(list []*TypeParam, tpar *TypeParam) int {
	// Once a type parameter is bound its index is >= 0. However, there are some
	// code paths (namely tracing and type hashing) by which it is possible to
	// arrive here with a type parameter that has not been bound, hence the check
	// for 0 <= i below.
	// TODO(rfindley): investigate a better approach for guarding against using
	// unbound type parameters.
	if i := tpar.index; 0 <= i && i < len(list) && list[i] == tpar {
		return i
	}
	return -1
}
