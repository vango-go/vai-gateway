package gem

import "testing"

func TestApplyPartialArgPath(t *testing.T) {
	t.Parallel()

	root := map[string]any{}
	if err := applyPartialArgPath(root, "$.a[0].b", 1.5); err != nil {
		t.Fatalf("applyPartialArgPath nested failed: %v", err)
	}

	a, ok := root["a"].([]any)
	if !ok || len(a) != 1 {
		t.Fatalf("unexpected a: %#v", root["a"])
	}
	obj, ok := a[0].(map[string]any)
	if !ok || obj["b"] != 1.5 {
		t.Fatalf("unexpected nested object: %#v", a[0])
	}

	if err := applyPartialArgPath(root, "$['meta']['name']", "x"); err != nil {
		t.Fatalf("applyPartialArgPath bracket key failed: %v", err)
	}
	meta, ok := root["meta"].(map[string]any)
	if !ok || meta["name"] != "x" {
		t.Fatalf("unexpected meta: %#v", root["meta"])
	}
}

func TestApplyPartialArgPath_Unsupported(t *testing.T) {
	t.Parallel()

	root := map[string]any{}
	if err := applyPartialArgPath(root, "$..x", "bad"); err == nil {
		t.Fatalf("expected unsupported JSONPath error")
	}
}

