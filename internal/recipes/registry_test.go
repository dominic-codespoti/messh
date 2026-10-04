package recipes

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func fixtureDefinition(schema string) Definition {
	value := "--count"
	return Definition{Name: "train", Version: "1.0.0", Command: "printf", Args: []Arg{{Value: &value}, {Parameter: "count"}}, Parameters: json.RawMessage(schema), Results: []string{"model.bin"}}
}
func TestSchemaValidationAndImmutableDisable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "recipes.json")
	r, err := New(path)
	if err != nil {
		t.Fatal(err)
	}
	published, err := r.Publish(fixtureDefinition("{\"type\":\"object\",\"properties\":{\"count\":{\"type\":\"integer\",\"minimum\":1,\"maximum\":4}},\"required\":[\"count\"],\"additionalProperties\":false}"))
	if err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"{}", "{\"count\":5}", "{\"count\":2,\"typo\":true}", "{\"count\":\"2\"}"} {
		if _, err := r.Resolve("train", "1.0.0", published.Digest, json.RawMessage(bad)); err == nil {
			t.Fatalf("accepted invalid parameters %s", bad)
		} else {
			var coded interface{ ErrorCode() string }
			if !errors.As(err, &coded) || coded.ErrorCode() != "invalid_recipe_parameters" {
				t.Fatalf("error code = %v", err)
			}
		}
	}
	inv, err := r.Resolve("train", "1.0.0", published.Digest, json.RawMessage("{\"count\":3}"))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(inv.Args, []string{"--count", "3"}) || inv.Command != "printf" {
		t.Fatalf("invocation=%+v", inv)
	}
	if _, err := r.Publish(fixtureDefinition("{\"type\":\"object\",\"properties\":{\"count\":{\"type\":\"integer\",\"minimum\":1,\"maximum\":4}},\"required\":[\"count\"],\"additionalProperties\":false}")); err == nil {
		t.Fatal("accepted conflicting immutable version")
	} else {
		var coded interface{ ErrorCode() string }
		if !errors.As(err, &coded) || coded.ErrorCode() != "recipe_version_conflict" {
			t.Fatalf("conflict error=%v", err)
		}
	}
	disabled, err := r.Disable("train", "1.0.0")
	if err != nil {
		t.Fatal(err)
	}
	if !disabled.Disabled || disabled.Digest != published.Digest {
		t.Fatalf("disabled=%+v", disabled)
	}
	if _, err := r.Resolve("train", "1.0.0", published.Digest, json.RawMessage("{\"count\":3}")); err == nil {
		t.Fatal("disabled recipe resolved")
	} else {
		var coded interface{ ErrorCode() string }
		if !errors.As(err, &coded) || coded.ErrorCode() != "recipe_disabled" {
			t.Fatalf("disabled error=%v", err)
		}
	}
	reloaded, err := New(path)
	if err != nil {
		t.Fatal(err)
	}
	got, err := reloaded.Get("train", "1.0.0", published.Digest)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Disabled || got.Digest != published.Digest || !reflect.DeepEqual(got.Definition, disabled.Definition) {
		t.Fatalf("reloaded=%+v", got)
	}
}
func TestDefinitionDigestCanonicalizesSchemaAndStrictReload(t *testing.T) {
	a, err := New(filepath.Join(t.TempDir(), "a.json"))
	if err != nil {
		t.Fatal(err)
	}
	b, err := New(filepath.Join(t.TempDir(), "b.json"))
	if err != nil {
		t.Fatal(err)
	}
	defA := fixtureDefinition("{\"type\":\"object\",\"properties\":{\"x\":{\"type\":\"string\"}},\"additionalProperties\":false}")
	defA.Args = nil
	da, err := a.Publish(defA)
	if err != nil {
		t.Fatal(err)
	}
	defB := fixtureDefinition("{\"additionalProperties\":false,\"properties\":{\"x\":{\"type\":\"string\"}},\"type\":\"object\"}")
	defB.Args = nil
	db, err := b.Publish(defB)
	if err != nil {
		t.Fatal(err)
	}
	if da.Digest != db.Digest {
		t.Fatalf("semantic schema digest differs: %s != %s", da.Digest, db.Digest)
	}
	corrupt := filepath.Join(t.TempDir(), "recipes.json")
	if err := os.WriteFile(corrupt, []byte("{\"version\":1,\"recipes\":[{\"junk\":true}] }"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := New(corrupt); err == nil {
		t.Fatal("strict load accepted unknown persisted fields")
	}
	insecure := filepath.Join(t.TempDir(), "recipes.json")
	if err := os.WriteFile(insecure, []byte("{\"version\":1,\"recipes\":[]}"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := New(insecure); err == nil {
		t.Fatal("registry loaded with permissive file mode")
	}
}
func TestParameterBindingStaysOneArgAndRejectsExternalRefs(t *testing.T) {
	r, err := New(filepath.Join(t.TempDir(), "recipes.json"))
	if err != nil {
		t.Fatal(err)
	}
	d := Definition{Name: "safe", Version: "1", Command: "printf", Args: []Arg{{Parameter: "text"}}, Parameters: json.RawMessage(`{"type":"object","properties":{"text":{"type":"string"}},"required":["text"],"additionalProperties":false}`)}
	published, err := r.Publish(d)
	if err != nil {
		t.Fatal(err)
	}
	payload := `; touch /tmp/not-executed; echo "x"`
	parameters, _ := json.Marshal(map[string]string{"text": payload})
	inv, err := r.Resolve("safe", "1", published.Digest, parameters)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(inv.Args, []string{payload}) {
		t.Fatalf("parameter was not preserved as one argv item: %#v", inv.Args)
	}
	bad := Definition{Name: "external", Version: "1", Command: "true", Parameters: json.RawMessage(`{"type":"object","$ref":"https://example.invalid/schema.json"}`)}
	if _, err := r.Publish(bad); err == nil {
		t.Fatal("accepted network-resolved parameter schema")
	}
}
func TestRegistryDoesNotFollowSymlinks(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target.json")
	link := filepath.Join(dir, "recipes.json")
	if err := os.WriteFile(target, []byte(`{"version":1,"recipes":[]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if _, err := New(link); err == nil {
		t.Fatal("registry followed symlink")
	}
}
func TestReturnedDefinitionCannotMutateRegistry(t *testing.T) {
	r, err := New(filepath.Join(t.TempDir(), "recipes.json"))
	if err != nil {
		t.Fatal(err)
	}
	fixed := "--safe"
	published, err := r.Publish(Definition{Name: "immutable", Version: "1", Command: "echo", Args: []Arg{{Value: &fixed}}, Env: map[string]string{"MODE": "safe"}, Parameters: json.RawMessage(`{"type":"object"}`)})
	if err != nil {
		t.Fatal(err)
	}
	fixed = "--source-changed"
	*published.Args[0].Value = "--changed"
	published.Env["MODE"] = "changed"
	got, err := r.Get("immutable", "1", published.Digest)
	if err != nil {
		t.Fatal(err)
	}
	if *got.Args[0].Value != "--safe" || got.Env["MODE"] != "safe" {
		t.Fatalf("returned definition mutated registry: %+v", got.Definition)
	}
}
