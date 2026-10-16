package run

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

var updateSupportSchemas = flag.Bool("update-support-schema-vectors", false, "regenerate Go-validated support schema examples")

type supportSchemaVector struct {
	SchemaSHA256 string          `json:"schema_sha256"`
	Definition   json.RawMessage `json:"definition"`
}

func TestSupportDeploymentSchemas(t *testing.T) {
	paths, err := filepath.Glob("../../api/support/*/profile-schema.json")
	if err != nil || len(paths) != 16 {
		t.Fatal("deployment schema inventory", err)
	}
	vectors := make(map[string]supportSchemaVector)
	for _, path := range paths {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		var schema map[string]any
		if json.Unmarshal(raw, &schema) != nil {
			t.Fatal(path)
		}
		d := supportDefinitionFixture()
		d.Model.ObservedOn, d.Price.ObservedOn = "2026-10-16", "2026-10-16"
		d.Action = &SupportActionDefinition{Operation: "apply_ticket_resolution", Origin: "http://business:8092", KeyID: "synthetic-key", PublicKeySHA256: strings.Repeat("a", 64)}
		final := d.Model
		d.FinalModel = &final
		body, _ := json.Marshal(d)
		var sample map[string]any
		_ = json.Unmarshal(body, &sample)
		// Existing typed values supply unconstrained identities. Schema constants
		// must still be accepted by the production builder and hash validation.
		applySchemaConstants(sample, schema)
		body, _ = json.Marshal(sample)
		d, err = DecodeSupportDefinition(body)
		if err != nil {
			t.Fatalf("%s: Go rejects raw schema example: %v", path, err)
		}
		p, err := BuildSupportProfile("schema-contract", d)
		if err != nil || ValidateSupportProfile(p) != nil {
			t.Fatalf("%s: Go rejects schema constants: %v", path, err)
		}
		dataset := schema["properties"].(map[string]any)["resources"].(map[string]any)["properties"].(map[string]any)["dataset_id"].(map[string]any)
		if values, ok := dataset["enum"].([]any); ok {
			for _, value := range values {
				changed := d
				changed.Resources.DatasetID = value.(string)
				if _, err := BuildSupportProfile("schema-dataset", changed); err != nil {
					t.Fatalf("%s: Go rejects dataset %s: %v", path, value, err)
				}
			}
		}
		digest := sha256.Sum256(raw)
		vectors[filepath.Base(filepath.Dir(path))] = supportSchemaVector{SchemaSHA256: fmt.Sprintf("%x", digest), Definition: body}
	}
	body, _ := json.MarshalIndent(vectors, "", "  ")
	body = append(body, '\n')
	path := "../../tests/testdata/support-profile-schema-vectors.json"
	if *updateSupportSchemas {
		if err := os.WriteFile(path, body, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	actual, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(actual, body) {
		t.Fatal("regenerate support schema vectors from Go")
	}
}

func applySchemaConstants(sample, schema map[string]any) {
	properties := schema["properties"].(map[string]any)
	for key := range sample {
		if _, exists := properties[key]; !exists {
			delete(sample, key)
		}
	}
	for key, raw := range properties {
		property := raw.(map[string]any)
		if constant, ok := property["const"]; ok {
			sample[key] = constant
			continue
		}
		if enum, ok := property["enum"].([]any); ok {
			sample[key] = enum[0]
			continue
		}
		if _, ok := property["properties"]; ok {
			child, ok := sample[key].(map[string]any)
			if !ok {
				child = make(map[string]any)
				sample[key] = child
			}
			applySchemaConstants(child, property)
		} else if _, ok := sample[key]; !ok {
			sample[key] = strings.Repeat("a", 64)
		}
	}
}
