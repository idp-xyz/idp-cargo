package httpapi

import (
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"regexp"
	"strings"
	"time"
)

// OpenAPI answers the OpenAPI 3.1 document of a route table, as indented JSON. It is deterministic: the same table
// gives the same bytes, so a test can hold the published document to the routes.
func OpenAPI(title, version string, ops []Op) ([]byte, error) {
	g := &generator{schemas: map[string]any{}}
	g.schemas["Problem"] = map[string]any{"type": "object", "description": "RFC 9457 problem details with the IDP business code",
		"properties": map[string]any{"type": str(), "title": str(), "status": map[string]any{"type": "integer"}, "detail": str(),
			"code": str(), "correlation_id": str()}}
	paths := map[string]map[string]any{}
	ids := map[string]int{}
	for _, op := range ops {
		item := paths[op.Path]
		if item == nil {
			item = map[string]any{}
			paths[op.Path] = item
		}
		id := op.Context + "." + op.Calls
		if op.Calls == "" {
			id = op.Context + "." + strings.ToLower(op.Method) + strings.ReplaceAll(op.Path, "/", "_")
		}
		ids[id]++
		if ids[id] > 1 {
			id = fmt.Sprintf("%s.%d", id, ids[id])
		}
		item[strings.ToLower(op.Method)] = g.operation(op, id)
	}
	doc := map[string]any{
		"openapi": "3.1.0",
		"info": map[string]any{"title": title, "version": version, "description": "Cargo's HTTP API. Every /v1 call carries a bearer token; " +
			"the tenant and the actor come only from it. A command carries Idempotency-Key (its command id) and, when it changes a resource at a " +
			"revision, If-Match \"n\"; it states its business time in occurred_at. Errors are RFC 9457 problems with a business code."},
		"security":   []any{map[string]any{"bearer": []any{}}},
		"paths":      paths,
		"components": map[string]any{"schemas": g.schemas, "securitySchemes": map[string]any{"bearer": map[string]any{"type": "http", "scheme": "bearer"}}},
	}
	out, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(out, '\n'), nil
}

type generator struct {
	schemas map[string]any
}

func str() map[string]any { return map[string]any{"type": "string"} }

func (g *generator) operation(op Op, id string) map[string]any {
	o := map[string]any{"operationId": id, "tags": []any{op.Context}}
	if op.Summary != "" {
		o["summary"] = op.Summary
	}
	if op.Legacy {
		o["description"] = "A route written before V2.106: its body and response are described in the context's adapter and in docs/api."
		o["responses"] = map[string]any{"default": problem()}
		o["x-calls"] = op.Calls
		return o
	}
	o["x-calls"] = op.Calls
	if op.Action != "" {
		perm := op.Context + ":" + op.Action
		if op.Named {
			perm += " (granted by name only)"
		}
		o["x-permission"] = perm
	} else if op.ContextChecks {
		o["x-permission"] = "checked by the context"
	}
	s := shapeOf(op, op.input)
	var params []any
	for _, m := range pathParam.FindAllStringSubmatch(op.Path, -1) {
		f := op.input.FieldByIndex(s.path[m[1]])
		params = append(params, map[string]any{"name": m[1], "in": "path", "required": true, "schema": g.schema(f.Type)})
	}
	if op.command {
		params = append(params, map[string]any{"name": "Idempotency-Key", "in": "header", "required": true, "schema": str(),
			"description": "the command id: the same key with the same body returns the first receipt"})
		if s.revision != nil {
			params = append(params, map[string]any{"name": "If-Match", "in": "header", "required": true, "schema": str(),
				"description": `the revision the command expects, "n"`})
		}
		o["requestBody"] = map[string]any{"required": true, "content": map[string]any{"application/json": map[string]any{
			"schema": g.object(op.input, s.server)}}}
	} else {
		for _, f := range fieldsOf(op.input) {
			if s.server[f.key] {
				continue
			}
			params = append(params, map[string]any{"name": f.key, "in": "query", "schema": g.schema(op.input.FieldByIndex(f.index).Type)})
		}
	}
	if len(params) > 0 {
		o["parameters"] = params
	}
	result := g.schema(op.result)
	if op.result.Kind() == reflect.Slice {
		result = map[string]any{"type": "object", "properties": map[string]any{"items": result}, "additionalProperties": false}
	}
	ok := map[string]any{"description": http.StatusText(op.Status), "content": map[string]any{"application/json": map[string]any{"schema": result}}}
	revision := "Revision"
	if op.command {
		revision = "NewRevision"
	}
	if op.result.Kind() == reflect.Struct {
		if f, found := op.result.FieldByName(revision); found && f.Type.Kind() == reflect.Int64 {
			ok["headers"] = map[string]any{"ETag": map[string]any{"schema": str(), "description": `the revision, "n"`}}
		}
	}
	o["responses"] = map[string]any{fmt.Sprint(op.Status): ok, "default": problem()}
	return o
}

func problem() map[string]any {
	return map[string]any{"description": "a problem", "content": map[string]any{"application/problem+json": map[string]any{
		"schema": map[string]any{"$ref": "#/components/schemas/Problem"}}}}
}

var timeType = reflect.TypeFor[time.Time]()

var unsafeName = regexp.MustCompile(`[^A-Za-z0-9_.]+`)

// name is a component's name: the context of the package (".../contexts/org/api" is org) and the type's name.
func name(t reflect.Type) string {
	pkg := t.PkgPath()
	if i := strings.Index(pkg, "/contexts/"); i >= 0 {
		pkg = strings.SplitN(pkg[i+len("/contexts/"):], "/", 2)[0]
	} else if j := strings.LastIndex(pkg, "/"); j >= 0 {
		pkg = pkg[j+1:]
	}
	return unsafeName.ReplaceAllString(pkg+"."+t.Name(), "_")
}

// object is the inline schema of a request body: the struct's fields without the keys the server sets.
func (g *generator) object(t reflect.Type, skip map[string]bool) map[string]any {
	props := map[string]any{}
	for _, f := range fieldsOf(t) {
		if skip[f.key] {
			continue
		}
		props[f.key] = g.schema(t.FieldByIndex(f.index).Type)
	}
	return map[string]any{"type": "object", "properties": props, "additionalProperties": false}
}

func (g *generator) schema(t reflect.Type) map[string]any {
	if t == timeType {
		return map[string]any{"type": "string", "format": "date-time"}
	}
	if t.Implements(marshalerType) && t.Kind() != reflect.Pointer {
		return map[string]any{"description": "JSON as " + t.String() + " writes it"}
	}
	switch t.Kind() {
	case reflect.Pointer:
		return map[string]any{"anyOf": []any{g.schema(t.Elem()), map[string]any{"type": "null"}}}
	case reflect.Struct:
		if t.Name() == "" {
			return g.object(t, nil)
		}
		n := name(t)
		if _, done := g.schemas[n]; !done {
			g.schemas[n] = map[string]any{} // a recursive type ends here
			g.schemas[n] = g.object(t, nil)
		}
		return map[string]any{"$ref": "#/components/schemas/" + n}
	case reflect.Slice, reflect.Array:
		if t.Elem().Kind() == reflect.Uint8 {
			return map[string]any{"type": "string", "contentEncoding": "base64"}
		}
		return map[string]any{"type": "array", "items": g.schema(t.Elem())}
	case reflect.Map:
		return map[string]any{"type": "object", "additionalProperties": g.schema(t.Elem())}
	case reflect.String:
		return str()
	case reflect.Bool:
		return map[string]any{"type": "boolean"}
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return map[string]any{"type": "integer", "format": fmt.Sprintf("int%d", t.Bits())}
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return map[string]any{"type": "integer", "minimum": 0}
	case reflect.Float32, reflect.Float64:
		return map[string]any{"type": "number"}
	}
	return map[string]any{}
}
