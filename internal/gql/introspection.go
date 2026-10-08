package gql

import (
	"context"
	"sort"
	"strings"

	"github.com/vektah/gqlparser/v2/ast"
)

// GraphQL introspection (__schema, __type), executed against gqlparser's parsed schema, which
// already includes the introspection types themselves, so the validator accepts these queries and
// this file only has to answer them.

type schemaObj struct{ s *ast.Schema }

func (schemaObj) typeName() string { return "__Schema" }

func (o schemaObj) resolve(_ context.Context, x *executor, c collected) (any, error) {
	switch c.first().Name {
	case "types":
		names := make([]string, 0, len(o.s.Types))
		for n := range o.s.Types {
			names = append(names, n)
		}
		sort.Strings(names)
		out := make(list, len(names))
		for i, n := range names {
			out[i] = typeObj{s: o.s, def: o.s.Types[n]}
		}
		return out, nil
	case "queryType":
		return typeObj{s: o.s, def: o.s.Query}, nil
	case "directives":
		names := make([]string, 0, len(o.s.Directives))
		for n := range o.s.Directives {
			names = append(names, n)
		}
		sort.Strings(names)
		out := make(list, len(names))
		for i, n := range names {
			out[i] = directiveObj{s: o.s, d: o.s.Directives[n]}
		}
		return out, nil
	}
	return nil, nil // description, mutationType, subscriptionType: none
}

// typeObj is a named type (def set) or a LIST/NON_NULL wrapper around ref.
type typeObj struct {
	s    *ast.Schema
	def  *ast.Definition
	kind string // "LIST" or "NON_NULL" for wrappers
	of   *ast.Type
}

func typeRef(s *ast.Schema, t *ast.Type) object {
	if t == nil {
		return nil
	}
	if t.NonNull {
		inner := *t
		inner.NonNull = false
		return typeObj{s: s, kind: "NON_NULL", of: &inner}
	}
	if t.Elem != nil {
		return typeObj{s: s, kind: "LIST", of: t.Elem}
	}
	if d := s.Types[t.NamedType]; d != nil {
		return typeObj{s: s, def: d}
	}
	return nil
}

func (typeObj) typeName() string { return "__Type" }

func (o typeObj) resolve(_ context.Context, x *executor, c collected) (any, error) {
	name := c.first().Name
	if o.def == nil { // wrapper
		switch name {
		case "kind":
			return o.kind, nil
		case "ofType":
			return orNil(typeRef(o.s, o.of)), nil
		}
		return nil, nil
	}
	d := o.def
	includeDeprecated, _ := x.args(c)["includeDeprecated"].(bool)
	switch name {
	case "kind":
		return string(d.Kind), nil
	case "name":
		return d.Name, nil
	case "description":
		return nilIfEmpty(d.Description), nil
	case "fields":
		if d.Kind != ast.Object && d.Kind != ast.Interface {
			return nil, nil
		}
		var out list
		for _, f := range d.Fields {
			if strings.HasPrefix(f.Name, "__") || (!includeDeprecated && f.Directives.ForName("deprecated") != nil) {
				continue
			}
			out = append(out, fieldObj{s: o.s, f: f})
		}
		return nonNilList(out), nil
	case "interfaces":
		if d.Kind != ast.Object && d.Kind != ast.Interface {
			return nil, nil
		}
		return list{}, nil
	case "possibleTypes":
		return nil, nil
	case "enumValues":
		if d.Kind != ast.Enum {
			return nil, nil
		}
		var out list
		for _, v := range d.EnumValues {
			if !includeDeprecated && v.Directives.ForName("deprecated") != nil {
				continue
			}
			out = append(out, enumValueObj{v})
		}
		return nonNilList(out), nil
	case "inputFields":
		if d.Kind != ast.InputObject {
			return nil, nil
		}
		var out list
		for _, f := range d.Fields {
			out = append(out, inputValueObj{s: o.s, name: f.Name, desc: f.Description, t: f.Type, def: f.DefaultValue, dirs: f.Directives})
		}
		return nonNilList(out), nil
	case "isOneOf":
		return false, nil
	}
	return nil, nil // ofType, specifiedByURL
}

type fieldObj struct {
	s *ast.Schema
	f *ast.FieldDefinition
}

func (fieldObj) typeName() string { return "__Field" }

func (o fieldObj) resolve(_ context.Context, _ *executor, c collected) (any, error) {
	switch c.first().Name {
	case "name":
		return o.f.Name, nil
	case "description":
		return nilIfEmpty(o.f.Description), nil
	case "args":
		var out list
		for _, a := range o.f.Arguments {
			out = append(out, inputValueObj{s: o.s, name: a.Name, desc: a.Description, t: a.Type, def: a.DefaultValue, dirs: a.Directives})
		}
		return nonNilList(out), nil
	case "type":
		return typeRef(o.s, o.f.Type), nil
	}
	return deprecation(o.f.Directives, c.first().Name), nil
}

type inputValueObj struct {
	s    *ast.Schema
	name string
	desc string
	t    *ast.Type
	def  *ast.Value
	dirs ast.DirectiveList
}

func (inputValueObj) typeName() string { return "__InputValue" }

func (o inputValueObj) resolve(_ context.Context, _ *executor, c collected) (any, error) {
	switch c.first().Name {
	case "name":
		return o.name, nil
	case "description":
		return nilIfEmpty(o.desc), nil
	case "type":
		return typeRef(o.s, o.t), nil
	case "defaultValue":
		if o.def == nil {
			return nil, nil
		}
		return o.def.String(), nil
	}
	return deprecation(o.dirs, c.first().Name), nil
}

type enumValueObj struct{ v *ast.EnumValueDefinition }

func (enumValueObj) typeName() string { return "__EnumValue" }

func (o enumValueObj) resolve(_ context.Context, _ *executor, c collected) (any, error) {
	switch c.first().Name {
	case "name":
		return o.v.Name, nil
	case "description":
		return nilIfEmpty(o.v.Description), nil
	}
	return deprecation(o.v.Directives, c.first().Name), nil
}

type directiveObj struct {
	s *ast.Schema
	d *ast.DirectiveDefinition
}

func (directiveObj) typeName() string { return "__Directive" }

func (o directiveObj) resolve(_ context.Context, _ *executor, c collected) (any, error) {
	switch c.first().Name {
	case "name":
		return o.d.Name, nil
	case "description":
		return nilIfEmpty(o.d.Description), nil
	case "locations":
		out := make([]any, len(o.d.Locations))
		for i, l := range o.d.Locations {
			out[i] = string(l)
		}
		return out, nil
	case "args":
		var out list
		for _, a := range o.d.Arguments {
			out = append(out, inputValueObj{s: o.s, name: a.Name, desc: a.Description, t: a.Type, def: a.DefaultValue, dirs: a.Directives})
		}
		return nonNilList(out), nil
	case "isRepeatable":
		return o.d.IsRepeatable, nil
	}
	return nil, nil
}

func deprecation(ds ast.DirectiveList, field string) any {
	d := ds.ForName("deprecated")
	switch field {
	case "isDeprecated":
		return d != nil
	case "deprecationReason":
		if d == nil {
			return nil
		}
		if a := d.Arguments.ForName("reason"); a != nil && a.Value != nil {
			return a.Value.Raw
		}
		return "No longer supported"
	}
	return nil
}

func nilIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// orNil keeps a nil object from becoming a non-nil interface holding a nil value.
func orNil(o object) any {
	if o == nil {
		return nil
	}
	return o
}

func nonNilList(l list) list {
	if l == nil {
		return list{}
	}
	return l
}
