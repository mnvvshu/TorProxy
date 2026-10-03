package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// TestShippedDefaultConfigMatchesCode guarantees configs/default_config.json
// stays in sync with DefaultSettings(): every key present, no unknown keys,
// and identical values.
func TestShippedDefaultConfigMatchesCode(t *testing.T) {
	path := filepath.Join("..", "..", "configs", "default_config.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}

	var shipped map[string]interface{}
	if err := json.Unmarshal(data, &shipped); err != nil {
		t.Fatalf("parse shipped config: %v", err)
	}
	defData, _ := json.Marshal(DefaultSettings())
	var want map[string]interface{}
	_ = json.Unmarshal(defData, &want)

	for k := range want {
		if _, ok := shipped[k]; !ok {
			t.Errorf("default_config.json is missing key %q", k)
		}
	}
	for k, v := range shipped {
		w, ok := want[k]
		if !ok {
			t.Errorf("default_config.json has unknown key %q", k)
			continue
		}
		if !reflect.DeepEqual(v, w) {
			t.Errorf("default_config.json %q = %v, DefaultSettings() = %v", k, v, w)
		}
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if errs := cfg.Validate(); len(errs) > 0 {
		t.Errorf("shipped config does not validate: %v", errs)
	}
}
