package hierarchy

import (
	"encoding/json"
	"reflect"
	"testing"
)

func mustJSON(t *testing.T, s string) map[string]interface{} {
	t.Helper()
	var m map[string]interface{}
	if err := json.Unmarshal([]byte(s), &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func TestCoerceArguments(t *testing.T) {
	schema := mustJSON(t, `{"properties":{
		"all":{"type":"boolean"},
		"n":{"type":"integer"},
		"f":{"type":"number"},
		"path":{"type":"string"},
		"paths":{"type":"array","items":{"type":"string"}},
		"ids":{"type":"array","items":{"type":"integer"}},
		"opts":{"type":"object","properties":{"force":{"type":"boolean"}}}
	}}`)

	tests := []struct {
		name, in, want string
	}{
		{"bool strings", `{"all":"true","path":"p"}`, `{"all":true,"path":"p"}`},
		{"false string", `{"all":"False"}`, `{"all":false}`},
		{"numbers", `{"n":"42","f":"1.5"}`, `{"n":42,"f":1.5}`},
		{"xml item wrapper scalar", `{"paths":{"item":"a.md"}}`, `{"paths":["a.md"]}`},
		{"xml item wrapper array", `{"paths":{"item":["a","b"]}}`, `{"paths":["a","b"]}`},
		{"json string array", `{"paths":"[\"a\",\"b\"]"}`, `{"paths":["a","b"]}`},
		{"bare string to array", `{"paths":"a"}`, `{"paths":["a"]}`},
		{"array elements", `{"ids":["1","2"]}`, `{"ids":[1,2]}`},
		{"nested object", `{"opts":{"force":"true"}}`, `{"opts":{"force":true}}`},
		{"object json string", `{"opts":"{\"force\":\"true\"}"}`, `{"opts":{"force":true}}`},
		{"already correct", `{"all":true,"paths":["a"],"n":3}`, `{"all":true,"paths":["a"],"n":3}`},
		{"unrepairable left alone", `{"all":"maybe","n":"x"}`, `{"all":"maybe","n":"x"}`},
		{"unknown key kept", `{"zzz":"true"}`, `{"zzz":"true"}`},
		{"string prop untouched", `{"path":"true"}`, `{"path":"true"}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			in := mustJSON(t, tt.in)
			orig := mustJSON(t, tt.in)
			got, _ := coerceArguments(in, schema)
			if !reflect.DeepEqual(in, orig) {
				t.Fatalf("input mutated: %v", in)
			}
			// Normalise numeric types via JSON round trip.
			b, _ := json.Marshal(got)
			if !reflect.DeepEqual(mustJSON(t, string(b)), mustJSON(t, tt.want)) {
				t.Fatalf("got %s want %s", b, tt.want)
			}
		})
	}
}

func TestCoerceArgumentsNoSchema(t *testing.T) {
	in := map[string]interface{}{"a": "true"}
	got, notes := coerceArguments(in, nil)
	if len(notes) != 0 || !reflect.DeepEqual(got, in) {
		t.Fatalf("got %v %v", got, notes)
	}
}
