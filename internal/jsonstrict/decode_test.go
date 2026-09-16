package jsonstrict

import (
	"strings"
	"testing"
)

func TestDecodeRejectsAmbiguityAndLimits(t *testing.T) {
	for _, raw := range []string{
		`{"name":"first","name":"second"}`, `{"name":"x","nested":{"x":1,"x":2}}`,
		`{"name":"x"} null`, `{"name":"x","unknown":1}`, `{"name":NaN}`,
		`{"name":"first","NAME":"second"}`, `{"name":null}`,
		"{\"name\":\"\xff\"}", strings.Repeat("[", 34) + "0" + strings.Repeat("]", 34),
	} {
		var value struct {
			Name string `json:"name"`
		}
		if Decode([]byte(raw), &value) == nil {
			t.Fatalf("accepted invalid JSON %q", raw)
		}
	}
	var value struct {
		Name string `json:"name"`
	}
	if err := Decode([]byte(`{"name":"valid"}`), &value); err != nil || value.Name != "valid" {
		t.Fatalf("valid JSON: %v", err)
	}
}

func TestDecodeNumericNullAndNullableRelation(t *testing.T) {
	type input struct {
		Vector  []float64 `json:"vector"`
		OrderID *string   `json:"order_id"`
	}
	var value input
	if Decode([]byte(`{"vector":[1,null,0],"order_id":null}`), &value) == nil {
		t.Fatal("null vector element accepted")
	}
	if err := Decode([]byte(`{"vector":[1,0,0],"order_id":null}`), &value); err != nil {
		t.Fatalf("nullable relationship rejected: %v", err)
	}
}
