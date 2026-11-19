package executor

import (
	"bytes"
	"compress/gzip"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"reflect"
	"strings"
	"testing"

	js "github.com/santhosh-tekuri/jsonschema/v6"
)

func arkDecode(t *testing.T, s string) any {
	t.Helper()
	d := json.NewDecoder(strings.NewReader(s))
	d.UseNumber()
	var v any
	if err := d.Decode(&v); err != nil {
		t.Fatal(err)
	}
	return v
}
func arkCompile(t *testing.T, v any) *js.Schema {
	t.Helper()
	c := js.NewCompiler()
	c.DefaultDraft(js.Draft2020)
	c.UseLoader(arkSchemaNoLoad{})
	if err := c.AddResource(arkSchemaRootURL, v); err != nil {
		t.Fatal(err)
	}
	s, err := c.Compile(arkSchemaRootURL)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestArkToolSchemaSemanticEquivalence(t *testing.T) {
	for _, tc := range []struct {
		name, s   string
		instances []string
	}{
		{"sibling conjunction", `{"$defs":{"x":{"type":"integer","minimum":2}},"type":"object","properties":{"x":{"$ref":"#/$defs/x","maximum":5}},"required":["x"],"additionalProperties":false}`, []string{`{"x":2}`, `{"x":5}`, `{"x":1}`, `{"x":6}`, `{"x":"2"}`, `{}`, `{"x":2,"y":1}`}},
		{"2019 tuple inherited", `{"$schema":"https://json-schema.org/draft/2019-09/schema","$defs":{"x":{"type":"array","items":[{"type":"string"},{"type":"integer"}],"additionalItems":false}},"properties":{"x":{"$ref":"#/$defs/x"}}}`, []string{`{"x":["a",1]}`, `{"x":[1,"a"]}`, `{"x":["a",1,2]}`}},
		{"anchor and escaped pointer", `{"$defs":{"a~/ b":{"$anchor":"item","type":"string","minLength":2}},"properties":{"a":{"$ref":"#/$defs/a~0~1%20b"},"b":{"$ref":"#item"}}}`, []string{`{"a":"ab","b":"cd"}`, `{"a":"a"}`, `{"b":3}`}},
		{"instance values stay opaque", `{"$defs":{"x":{"type":"object"}},"properties":{"x":{"$ref":"#/$defs/x"}},"default":{"$ref":"https://example.invalid/never-fetch"},"examples":[{"$ref":"#bad"}],"const":{"$ref":"literal"}}`, []string{`{"$ref":"literal"}`, `{"x":{}}`}},
		{"instance target stays data", `{"$defs":{"x":{"type":"object"}},"properties":{"x":{"$ref":"#/default"}},"default":{"$ref":"#/$defs/x","description":"template"}}`, []string{`{"x":{}}`, `{"x":1}`}},
		{"custom annotation target stays data", `{"$defs":{"x":{"type":"object"}},"properties":{"x":{"$ref":"#/x-data"}},"x-data":{"$ref":"#/$defs/x","description":"template"}}`, []string{`{"x":{}}`, `{"x":1}`}},
		{"boolean target", `{"$defs":{"yes":true,"no":false},"properties":{"x":{"$ref":"#/$defs/yes"},"y":{"$ref":"#/$defs/no"}}}`, []string{`{"x":123}`, `{"y":false}`, `{}`}},
		{"large exact integer", `{"$defs":{"x":{"const":9007199254740993}},"properties":{"x":{"$ref":"#/$defs/x"}}}`, []string{`{"x":9007199254740993}`, `{"x":9007199254740992}`}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			input := arkDecode(t, tc.s)
			before, _ := json.Marshal(input)
			got, err := inlineArkSchema(input)
			if err != nil {
				t.Fatal(err)
			}
			after, _ := json.Marshal(input)
			if !bytes.Equal(before, after) {
				t.Fatal("canonical schema mutated")
			}
			a, b := arkCompile(t, input), arkCompile(t, got)
			for _, instance := range tc.instances {
				v := arkDecode(t, instance)
				if (a.Validate(v) == nil) != (b.Validate(v) == nil) {
					t.Fatalf("semantic drift for %s", instance)
				}
			}
			if tc.name == "instance values stay opaque" || tc.name == "instance target stays data" || tc.name == "custom annotation target stays data" {
				out := got.(map[string]any)
				original := input.(map[string]any)
				for _, key := range []string{"default", "examples", "const", "x-data"} {
					if !reflect.DeepEqual(out[key], original[key]) {
						t.Fatalf("instance %s changed", key)
					}
				}
			}
		})
	}
}

func TestArkToolSchemaRefusals(t *testing.T) {
	for name, s := range map[string]string{
		"cycle":               `{"properties":{"child":{"$ref":"#"}}}`,
		"external":            `{"$ref":"https://example.invalid/private"}`,
		"missing":             `{"$ref":"#/$defs/missing"}`,
		"nested resource":     `{"$defs":{"x":{"$id":"urn:separate","type":"string"}},"properties":{"x":{"$ref":"urn:separate"}}}`,
		"dynamic":             `{"$dynamicAnchor":"x","properties":{"x":{"$dynamicRef":"#x"}}}`,
		"recursive":           `{"$schema":"https://json-schema.org/draft/2019-09/schema","$recursiveAnchor":true,"properties":{"x":{"$recursiveRef":"#"}}}`,
		"older dialect":       `{"$schema":"http://json-schema.org/draft-07/schema#","$defs":{"x":{"type":"string"}},"$ref":"#/$defs/x"}`,
		"annotation conflict": `{"$defs":{"x":{"title":"target","type":"string"}},"$ref":"#/$defs/x","title":"different"}`,
		"unevaluated sibling": `{"$defs":{"x":{"type":"object"}},"$ref":"#/$defs/x","unevaluatedProperties":false}`,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := inlineArkSchema(arkDecode(t, s)); err == nil {
				t.Fatal("unsafe schema accepted")
			}
		})
	}
}

func TestArkToolSchemaBudgets(t *testing.T) {
	t.Run("input depth", func(t *testing.T) {
		var v any = map[string]any{"$ref": "#"}
		for i := 0; i < arkSchemaMaxDepth+1; i++ {
			v = map[string]any{"properties": map[string]any{"x": v}}
		}
		if _, err := inlineArkSchema(v); err == nil {
			t.Fatal("unbounded depth accepted")
		}
	})
	t.Run("input nodes", func(t *testing.T) {
		values := make([]any, arkSchemaMaxNodes)
		v := map[string]any{"default": values, "$ref": "#"}
		if _, err := inlineArkSchema(v); err == nil {
			t.Fatal("unbounded nodes accepted")
		}
	})
	t.Run("expanded bytes", func(t *testing.T) {
		target := map[string]any{"type": "string", "default": strings.Repeat("x", 750000)}
		ref := map[string]any{"$ref": "#/$defs/x"}
		input := map[string]any{"$defs": map[string]any{"x": target}, "properties": map[string]any{"a": ref, "b": ref, "c": ref}}
		if _, err := inlineArkSchema(input); err == nil {
			t.Fatal("unbounded expanded bytes accepted")
		}
	})
	t.Run("acyclic expansion", func(t *testing.T) {
		defs := map[string]any{"n0": map[string]any{"type": "string"}}
		for i := 1; i < 15; i++ {
			ref := map[string]any{"$ref": fmt.Sprintf("#/$defs/n%d", i-1)}
			defs[fmt.Sprintf("n%d", i)] = map[string]any{"anyOf": []any{ref, ref}}
		}
		if _, err := inlineArkSchema(map[string]any{"$defs": defs, "$ref": "#/$defs/n14"}); err == nil {
			t.Fatal("unbounded expansion accepted")
		}
	})
}

func TestArkToolSchemaObserved103Tools(t *testing.T) {
	// Parameters only, compressed to keep the captured synthetic incident fixture small.
	// Expected schemas came from the independently accepted fourth Ark probe, before this implementation.
	compressed, err := base64.StdEncoding.DecodeString(arkObservedSchemas)
	if err != nil {
		t.Fatal(err)
	}
	reader, err := gzip.NewReader(bytes.NewReader(compressed))
	if err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(reader)
	if err != nil {
		t.Fatal(err)
	}
	if err = reader.Close(); err != nil {
		t.Fatal(err)
	}
	var fixture []struct {
		Name     string
		Original json.RawMessage
		Accepted json.RawMessage
	}
	if err = json.Unmarshal(data, &fixture); err != nil {
		t.Fatal(err)
	}
	if len(fixture) != 103 {
		t.Fatalf("incident fixture has %d tools", len(fixture))
	}
	for _, tool := range fixture {
		t.Run(tool.Name, func(t *testing.T) {
			input := arkDecode(t, string(tool.Original))
			before, _ := json.Marshal(input)
			actual, err := inlineArkSchema(input)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(actual, arkDecode(t, string(tool.Accepted))) {
				t.Fatal("differs from independently accepted candidate")
			}
			after, _ := json.Marshal(input)
			if !bytes.Equal(before, after) {
				t.Fatal("canonical input mutated")
			}
		})
	}
}

const arkObservedSchemas = "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"
