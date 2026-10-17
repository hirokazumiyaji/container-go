package domain

import (
	"testing"
)

func TestCheckReuseLabels(t *testing.T) {
	if err := CheckReuseLabels("myctr", nil); err == nil {
		t.Error("nil labels should return error")
	}
	if err := CheckReuseLabels("myctr", map[string]string{}); err == nil {
		t.Error("missing managed label should return error")
	}
	if err := CheckReuseLabels("myctr", map[string]string{LabelManaged: "true"}); err == nil {
		t.Error("missing reuse label should return error")
	}
	if err := CheckReuseLabels("myctr", map[string]string{LabelManaged: "true", LabelReuse: "true"}); err != nil {
		t.Errorf("valid labels returned error: %v", err)
	}
}

func TestCheckReuseGroup(t *testing.T) {
	labels := map[string]string{LabelReuseGroup: "web"}
	if err := CheckReuseGroup("myctr", labels, "web"); err != nil {
		t.Errorf("matching group returned error: %v", err)
	}
	if err := CheckReuseGroup("myctr", labels, "db"); err == nil {
		t.Error("mismatched group should return error")
	}
	if err := CheckReuseGroup("myctr", labels, ""); err != nil {
		t.Errorf("empty expected group should return nil: %v", err)
	}
}
