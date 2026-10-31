package approval

import "testing"

func TestPolicyCloneIsIndependent(t *testing.T) {
	base := NewPolicy(Auto, true)
	clone := base.Clone()
	if clone.Mode() != Auto || !clone.Publishing() {
		t.Fatalf("clone = %s/%v, want auto/publishing", clone.Mode(), clone.Publishing())
	}
	if err := clone.SetMode(Plan); err != nil {
		t.Fatal(err)
	}
	if base.Mode() != Auto {
		t.Fatalf("base mode = %s after the clone changed", base.Mode())
	}
	if (*Policy)(nil).Clone() != nil {
		t.Fatal("nil policy cloned to non-nil")
	}
}
