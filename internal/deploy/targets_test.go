package deploy

import "testing"

func TestDiscoverTargets(t *testing.T) {
	targets, err := DiscoverTargets([]string{`D:\code`})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("发现 %d 个目标路径", len(targets))
	byCat := map[string]int{}
	for _, tp := range targets {
		byCat[tp.Category]++
		t.Logf("  [%s] %-30s %s", tp.Category, tp.Label, tp.Path)
	}
	for cat, n := range byCat {
		t.Logf("  %s: %d", cat, n)
	}
	if len(targets) < 5 {
		t.Errorf("至少应发现 5 个目标路径，实际 %d", len(targets))
	}
}
