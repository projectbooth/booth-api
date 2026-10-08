package rest

import (
	"fmt"

	"github.com/projectbooth/booth-api/internal/table"
)

// OpenAPI renders the endpoint's OpenAPI 3.1 document, generated from the same model the handler
// serves, so the document can't drift from the behaviour. The server URL is ".", which OpenAPI
// resolves against the document's own location: the API's base, wherever it is mounted (ADR 0101).
func OpenAPI(m table.Model, l table.Limits) map[string]any {
	rowProps := map[string]any{}
	var required []string
	for _, f := range m.Fields {
		s := schemaFor(f)
		if f.Desc != "" {
			s["description"] = f.Desc
		}
		rowProps[f.Name] = s
		required = append(required, f.Name) // every field is present; nullable ones may be null
	}
	errorSchema := map[string]any{
		"type":     "object",
		"required": []string{"error"},
		"properties": map[string]any{"error": map[string]any{
			"type": "object", "required": []string{"message"},
			"properties": map[string]any{"message": map[string]any{"type": "string"}},
		}},
	}
	errorResponse := func(desc string) map[string]any {
		return map[string]any{"description": desc, "content": map[string]any{"application/json": map[string]any{"schema": ref("Error")}}}
	}

	fieldNames := make([]string, len(m.Fields))
	var orderable []string
	for i, f := range m.Fields {
		fieldNames[i] = f.Name
		if f.Orderable() {
			orderable = append(orderable, f.Name, "-"+f.Name)
		}
	}
	listParams := []any{
		map[string]any{"name": "limit", "in": "query", "description": fmt.Sprintf("Rows per page. Over %d is an error, not a clamp.", l.MaxFirst),
			"schema": map[string]any{"type": "integer", "minimum": 0, "maximum": l.MaxFirst, "default": l.DefaultFirst}},
		map[string]any{"name": "after", "in": "query", "description": "page.nextCursor from the previous page. Only valid with the same filters and order.",
			"schema": map[string]any{"type": "string"}},
		fieldsParam(fieldNames),
	}
	if len(orderable) > 0 {
		listParams = append(listParams, map[string]any{
			"name": "order", "in": "query", "style": "form", "explode": false,
			"description": "Comma-separated sort keys; a leading '-' sorts descending. The primary key is always added as a tiebreak.",
			"schema":      map[string]any{"type": "array", "items": map[string]any{"type": "string", "enum": orderable}},
		})
	}
	for _, f := range m.Fields {
		if !f.Filterable() {
			continue
		}
		for _, op := range table.OpsFor(f.Scalar) {
			p := map[string]any{"name": fmt.Sprintf("filter[%s][%s]", f.Name, op.Name), "in": "query", "schema": filterValueSchema(f, op.Name)}
			switch op.Name {
			case "in":
				p["style"], p["explode"] = "form", true
				p["description"] = fmt.Sprintf("Repeat the parameter for each value (at most %d).", l.MaxInList)
			case "startsWith":
				p["description"] = "Prefix match; % and _ are literal."
			case "eq":
				p["description"] = fmt.Sprintf("Also accepted as filter[%s].", f.Name)
			}
			listParams = append(listParams, p)
		}
	}

	paths := map[string]any{
		"/rows": map[string]any{"get": map[string]any{
			"operationId": "listRows",
			"summary":     "List rows",
			"description": "Filters are AND-ed. Pages with page.nextCursor until it is null.",
			"parameters":  listParams,
			"responses": map[string]any{
				"200": map[string]any{"description": "A page of rows", "content": map[string]any{"application/json": map[string]any{"schema": map[string]any{
					"type": "object", "required": []string{"data", "page"},
					"properties": map[string]any{
						"data": map[string]any{"type": "array", "items": ref("Row")},
						"page": map[string]any{"type": "object", "required": []string{"hasNext", "nextCursor"}, "properties": map[string]any{
							"hasNext":    map[string]any{"type": "boolean"},
							"nextCursor": map[string]any{"type": []string{"string", "null"}},
						}},
					},
				}}}},
				"400": errorResponse("The request is invalid or over a limit"),
				"401": errorResponse("Missing or invalid API key"),
				"403": errorResponse("The key isn't scoped to this dataset"),
				"503": errorResponse("The query timed out"),
			},
		}},
	}
	if len(m.PK) == 1 {
		paths["/rows/{key}"] = map[string]any{"get": map[string]any{
			"operationId": "getRow",
			"summary":     "Get one row by primary key",
			"parameters": []any{
				map[string]any{"name": "key", "in": "path", "required": true, "description": "The row's " + m.PK[0].Name, "schema": filterValueSchema(m.PK[0], "eq")},
				fieldsParam(fieldNames),
			},
			"responses": map[string]any{
				"200": map[string]any{"description": "The row", "content": map[string]any{"application/json": map[string]any{"schema": map[string]any{
					"type": "object", "required": []string{"data"}, "properties": map[string]any{"data": ref("Row")},
				}}}},
				"400": errorResponse("The key isn't a valid value"),
				"401": errorResponse("Missing or invalid API key"),
				"403": errorResponse("The key isn't scoped to this dataset"),
				"404": errorResponse("No row with that key"),
			},
		}}
	}
	paths["/openapi.json"] = map[string]any{"get": map[string]any{
		"operationId": "openapi", "summary": "This document",
		"responses": map[string]any{
			"200": map[string]any{"description": "OpenAPI 3.1 document"},
			"401": errorResponse("Missing or invalid API key"),
		},
	}}

	return map[string]any{
		"openapi": "3.1.0",
		"info": map[string]any{
			"title":       m.Def.DatasetName,
			"version":     m.Def.GeneratedAt.UTC().Format("2006-01-02T15:04:05Z"),
			"description": fmt.Sprintf("Read-only API over the catalog dataset %q (table %s.%s). The version is when its columns were last generated.", m.Def.DatasetName, m.Def.Table.Schema, m.Def.Table.Name),
		},
		"servers":  []any{map[string]any{"url": "."}},
		"paths":    paths,
		"security": []any{map[string]any{"apiKey": []string{}}},
		"components": map[string]any{
			"securitySchemes": map[string]any{"apiKey": map[string]any{
				"type": "http", "scheme": "bearer", "bearerFormat": "booth_ak_<id>_<secret>",
				"description": "An API key issued in Booth's API module, scoped to this dataset.",
			}},
			"schemas": map[string]any{
				"Row":   map[string]any{"type": "object", "properties": rowProps, "required": required, "additionalProperties": false},
				"Error": errorSchema,
			},
		},
	}
}

func ref(name string) map[string]any { return map[string]any{"$ref": "#/components/schemas/" + name} }

func fieldsParam(names []string) map[string]any {
	return map[string]any{
		"name": "fields", "in": "query", "style": "form", "explode": false,
		"description": "Comma-separated fields to return, in this order. Default: all.",
		"schema":      map[string]any{"type": "array", "items": map[string]any{"type": "string", "enum": names}},
	}
}

// scalarSchema is a value's JSON Schema, matching how table.outExpr serializes it.
func scalarSchema(s table.Scalar) map[string]any {
	switch s {
	case table.Int:
		return map[string]any{"type": "integer", "format": "int32"}
	case table.BigInt:
		return map[string]any{"type": "string", "format": "int64", "pattern": `^-?[0-9]+$`, "description": "A 64-bit integer, as a string so no precision is lost."}
	case table.Decimal:
		return map[string]any{"type": "string", "format": "decimal", "pattern": `^-?[0-9]+(\.[0-9]+)?$`, "description": "An exact decimal, as a string."}
	case table.Float:
		return map[string]any{"type": "number", "format": "double"}
	case table.Boolean:
		return map[string]any{"type": "boolean"}
	case table.ID:
		return map[string]any{"type": "string", "format": "uuid"}
	case table.Date:
		return map[string]any{"type": "string", "format": "date"}
	case table.DateTime:
		return map[string]any{"type": "string", "format": "date-time"}
	case table.Time:
		return map[string]any{"type": "string", "format": "time"}
	case table.JSON:
		return map[string]any{"description": "Any JSON value."}
	}
	return map[string]any{"type": "string"}
}

func schemaFor(f table.Field) map[string]any {
	s := scalarSchema(f.Scalar)
	if f.List {
		item := s
		if t, ok := item["type"]; ok {
			item["type"] = []any{t, "null"} // array elements may be NULL in Postgres
		}
		s = map[string]any{"type": "array", "items": item}
	}
	if f.Nullable {
		if t, ok := s["type"]; ok {
			s["type"] = []any{t, "null"}
		} else {
			s = map[string]any{"anyOf": []any{s, map[string]any{"type": "null"}}}
		}
	}
	return s
}

// filterValueSchema is how a filter value is written in a query string: always text, in the
// scalar's documented format.
func filterValueSchema(f table.Field, op string) map[string]any {
	if op == "isNull" {
		return map[string]any{"type": "boolean"}
	}
	s := scalarSchema(f.Scalar)
	if op == "in" {
		return map[string]any{"type": "array", "items": s}
	}
	return s
}
