package executor

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"reflect"
	"strconv"
	"strings"

	"github.com/go-openapi/jsonpointer"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	js "github.com/santhosh-tekuri/jsonschema/v6"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

const arkSchemaRootURL = "urn:ark-tool:root"
const arkSchemaMaxNodes = 10000
const arkSchemaMaxDepth = 64

// The compiler, not raw dictionary keys, identifies actual schema nodes and resolves refs.
// The existing Gemini cleaner drops constraints and turns cycles into hints; it
// cannot satisfy this Ark adapter's lossless contract.
type arkSchemaInliner struct {
	raw            any
	children       map[*js.Schema]map[string]*js.Schema
	remaining      int
	remainingBytes int
}

func inlineArkSchema(raw any) (any, error) {
	if err := arkSchemaInputBudget(raw, 0, new(int)); err != nil {
		return nil, err
	}
	c := js.NewCompiler()
	c.UseLoader(arkSchemaNoLoad{})
	c.DefaultDraft(js.Draft2020)
	c.AssertVocabs()
	if err := c.AddResource(arkSchemaRootURL, raw); err != nil {
		return nil, err
	}
	root, err := c.Compile(arkSchemaRootURL)
	if err != nil {
		return nil, err
	}
	x := &arkSchemaInliner{raw: raw, children: map[*js.Schema]map[string]*js.Schema{}, remaining: arkSchemaMaxNodes, remainingBytes: 2 << 20}
	seen := map[*js.Schema]bool{}
	refs := false
	var collect func(*js.Schema) error
	collect = func(s *js.Schema) error {
		if s == nil || seen[s] {
			return nil
		}
		seen[s] = true
		if len(seen) > arkSchemaMaxNodes {
			return fmt.Errorf("schema node budget")
		}
		children := map[string]*js.Schema{}
		x.children[s] = children
		structural := true
		if s.Ref != nil || s.DynamicRef != nil || s.RecursiveRef != nil {
			refs = true
		}
		// Traverse only the public compiler graph's schema-valued fields. Annotation instances stay opaque.
		var visit func(reflect.Value) error
		visit = func(v reflect.Value) error {
			if !v.IsValid() {
				return nil
			}
			if v.Type() == reflect.TypeOf((*js.Schema)(nil)) {
				if !v.IsNil() {
					child := v.Interface().(*js.Schema)
					if structural {
						location, errLocation := url.Parse(child.Location)
						if errLocation != nil {
							return errLocation
						}
						children[location.Fragment] = child
					}
					return collect(child)
				}
				return nil
			}
			switch v.Kind() {
			case reflect.Interface:
				if !v.IsNil() {
					return visit(v.Elem())
				}
			case reflect.Slice:
				for i := 0; i < v.Len(); i++ {
					if err := visit(v.Index(i)); err != nil {
						return err
					}
				}
			case reflect.Map:
				if v.Type().Elem() == reflect.TypeOf((*js.Schema)(nil)) {
					it := v.MapRange()
					for it.Next() {
						if err := visit(it.Value()); err != nil {
							return err
						}
					}
				}
			}
			return nil
		}
		v := reflect.ValueOf(s).Elem()
		t := v.Type()
		for i := 0; i < v.NumField(); i++ {
			if t.Field(i).IsExported() {
				// Reference targets have their own schema occurrence; they do
				// not turn annotation data at that source location into a
				// structural child of the referring schema.
				structural = t.Field(i).Name != "Ref" && t.Field(i).Name != "RecursiveRef" && t.Field(i).Name != "DynamicRef"
				if err := visit(v.Field(i)); err != nil {
					return err
				}
			}
		}
		return nil
	}
	if err := collect(root); err != nil {
		return nil, err
	}
	if !refs {
		return raw, nil
	}
	return x.expand(root, map[*js.Schema]bool{}, 0)
}
func (x *arkSchemaInliner) rawAt(s *js.Schema) (any, error) {
	u, err := url.Parse(s.Location)
	if err != nil {
		return nil, err
	}
	if strings.Split(s.Location, "#")[0] != arkSchemaRootURL {
		return nil, fmt.Errorf("nonlocal resource")
	}
	p, err := jsonpointer.New(u.Fragment)
	if err != nil {
		return nil, err
	}
	v, _, err := p.Get(x.raw)
	return v, err
}
func (x *arkSchemaInliner) expand(s *js.Schema, active map[*js.Schema]bool, depth int) (any, error) {
	x.remaining--
	x.remainingBytes -= 32
	if x.remaining < 0 || x.remainingBytes < 0 || depth > arkSchemaMaxDepth {
		return nil, fmt.Errorf("expansion budget")
	}
	if active[s] {
		return nil, fmt.Errorf("cyclic reference")
	}
	active[s] = true
	defer delete(active, s)
	if s.DraftVersion != 2019 && s.DraftVersion != 2020 {
		return nil, fmt.Errorf("unsupported dialect")
	}
	if s.DynamicRef != nil || s.RecursiveRef != nil {
		return nil, fmt.Errorf("dynamic reference")
	}
	raw, err := x.rawAt(s)
	if err != nil {
		return nil, err
	}
	m, ok := raw.(map[string]any)
	if !ok {
		return raw, nil
	}
	if _, id := m["$id"]; id && s.Location != arkSchemaRootURL+"#" {
		return nil, fmt.Errorf("nested resource scope")
	}
	out := map[string]any{}
	for key, v := range m {
		if key == "$ref" || key == "$defs" || key == "definitions" || key == "$anchor" {
			continue
		}
		// An instance can also be a reference target. Its original annotation
		// occurrence remains data even when the compiler graph compiles that
		// location as a target schema for a different occurrence.
		if key == "default" || key == "examples" || key == "enum" || key == "const" {
			nodes := 0
			if err := arkSchemaInputBudget(v, depth+1, &nodes); err != nil {
				return nil, err
			}
			encoded, err := json.Marshal(v)
			if err != nil {
				return nil, err
			}
			x.remaining -= nodes
			x.remainingBytes -= len(encoded)
			if x.remaining < 0 || x.remainingBytes < 0 {
				return nil, fmt.Errorf("expansion budget")
			}
			out[key] = v
			continue
		}
		location, errLocation := url.Parse(s.Location)
		if errLocation != nil {
			return nil, errLocation
		}
		child, err := x.container(v, location.Fragment+"/"+jsonpointer.Escape(key), x.children[s], active, depth+1)
		if err != nil {
			return nil, err
		}
		out[key] = child
	}
	if s.Ref == nil {
		return out, nil
	}
	ref, _ := m["$ref"].(string)
	if !strings.HasPrefix(ref, "#") {
		return nil, fmt.Errorf("external reference")
	}
	target, err := x.expand(s.Ref, active, depth+1)
	if err != nil {
		return nil, err
	}
	if len(out) == 0 {
		return target, nil
	}
	annotations := map[string]bool{"default": true, "description": true, "title": true, "$comment": true, "$schema": true}
	simple := true
	for k := range out {
		simple = simple && annotations[k]
	}
	if tm, ok := target.(map[string]any); ok && simple {
		conflict := false
		for k, v := range out {
			if tv, exists := tm[k]; exists && !reflect.DeepEqual(v, tv) {
				if k != "$schema" {
					return nil, fmt.Errorf("annotation conflict")
				}
				conflict = true
			}
		}
		if !conflict {
			for k, v := range out {
				tm[k] = v
			}
			return tm, nil
		}
	}
	if _, ok := out["unevaluatedProperties"]; ok {
		return nil, fmt.Errorf("unevaluated sibling")
	}
	if _, ok := out["unevaluatedItems"]; ok {
		return nil, fmt.Errorf("unevaluated sibling")
	}
	result := map[string]any{}
	if d, ok := out["$schema"]; ok {
		result["$schema"] = d
		delete(out, "$schema")
	}
	branches := []any{target}
	if len(out) > 0 {
		branches = append(branches, out)
	}
	result["allOf"] = branches
	return result, nil
}
func (x *arkSchemaInliner) container(v any, loc string, children map[string]*js.Schema, active map[*js.Schema]bool, depth int) (any, error) {
	x.remaining--
	x.remainingBytes -= 32
	if text, ok := v.(string); ok {
		x.remainingBytes -= len(text)
	}
	if x.remaining < 0 || x.remainingBytes < 0 || depth > arkSchemaMaxDepth {
		return nil, fmt.Errorf("expansion budget")
	}

	if s, ok := children[loc]; ok {
		return x.expand(s, active, depth)
	}
	switch a := v.(type) {
	case map[string]any:
		out := map[string]any{}
		for k, v := range a {
			n, e := x.container(v, loc+"/"+jsonpointer.Escape(k), children, active, depth+1)
			if e != nil {
				return nil, e
			}
			out[k] = n
		}
		return out, nil
	case []any:
		out := make([]any, len(a))
		for i, v := range a {
			n, e := x.container(v, loc+"/"+strconv.Itoa(i), children, active, depth+1)
			if e != nil {
				return nil, e
			}
			out[i] = n
		}
		return out, nil
	default:
		return v, nil
	}
}

// Ark rejects reference-form tool schemas. Keep the native protocol translators and
// usage pipeline; only adapt the final Chat function parameters for an explicitly
// declared Ark credential. The JSON Schema compiler owns dialects and resolution;
// jsonpointer owns RFC 6901. Raw JSON owns serialization, including annotations.
func (e *OpenAICompatExecutor) normalizeArkToolSchemas(auth *cliproxyauth.Auth, model string, payload []byte, chat bool) ([]byte, error) {
	if !chat {
		return payload, nil
	}
	protocol, err := e.configuredUpstreamProtocol(auth, model)
	if err != nil {
		return nil, err
	}
	if protocol != registry.UpstreamProtocolVolcengineArkTextV3 {
		return payload, nil
	}
	for i, tool := range gjson.GetBytes(payload, "tools").Array() {
		if tool.Get("type").String() != "function" {
			continue
		}
		schema := tool.Get("function.parameters")
		if !schema.Exists() {
			continue
		}
		raw := []byte(schema.Raw)
		if !bytes.Contains(raw, []byte(`"$ref"`)) && !bytes.Contains(raw, []byte(`"$dynamicRef"`)) && !bytes.Contains(raw, []byte(`"$recursiveRef"`)) {
			continue
		}
		toolName := tool.Get("function.name").String()
		if len(toolName) > 128 {
			toolName = toolName[:128]
		}
		fail := func() ([]byte, error) {
			return nil, statusErr{code: http.StatusBadRequest, msg: fmt.Sprintf("Ark tool %q schema at tools[%d].function.parameters cannot be losslessly inlined", toolName, i)}
		}
		if len(raw) > 2<<20 {
			return fail()
		}
		decoder := json.NewDecoder(bytes.NewReader(raw))
		decoder.UseNumber()
		var input any
		if decoder.Decode(&input) != nil {
			return fail()
		}
		output, errInline := inlineArkSchema(input)
		if errInline != nil {
			return fail()
		}
		if reflect.DeepEqual(output, input) {
			continue
		}
		encoded, errEncode := json.Marshal(output)
		if errEncode != nil || len(encoded) > 2<<20 {
			return fail()
		}
		payload, err = sjson.SetRawBytes(payload, fmt.Sprintf("tools.%d.function.parameters", i), encoded)
		if err != nil {
			return fail()
		}
	}
	return payload, nil
}

type arkSchemaNoLoad struct{}

func (arkSchemaNoLoad) Load(string) (any, error) {
	return nil, fmt.Errorf("external schema resources are disabled")
}
func arkSchemaInputBudget(v any, depth int, nodes *int) error {
	(*nodes)++
	if *nodes > arkSchemaMaxNodes || depth > arkSchemaMaxDepth {
		return fmt.Errorf("schema input budget")
	}
	switch x := v.(type) {
	case map[string]any:
		for _, child := range x {
			if err := arkSchemaInputBudget(child, depth+1, nodes); err != nil {
				return err
			}
		}
	case []any:
		for _, child := range x {
			if err := arkSchemaInputBudget(child, depth+1, nodes); err != nil {
				return err
			}
		}
	}
	return nil
}
