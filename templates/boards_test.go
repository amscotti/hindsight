package templates

import "testing"

func TestDefaultTemplateIsWentWellBetterKudos(t *testing.T) {
	formats := BoardTemplates()
	if len(formats) == 0 {
		t.Fatal("BoardTemplates is empty")
	}
	got := formats[0]
	if got.Key != "went-well-better-kudos" {
		t.Fatalf("default template key = %q, want went-well-better-kudos", got.Key)
	}
	want := []string{"Went well", "Could be better", "Kudos"}
	if len(got.Columns) != len(want) {
		t.Fatalf("default columns = %d, want %d", len(got.Columns), len(want))
	}
	for i, title := range want {
		if got.Columns[i].Title != title {
			t.Errorf("default column %d = %q, want %q", i, got.Columns[i].Title, title)
		}
		if columnColor(got.Columns[i].Color) == "transparent" {
			t.Errorf("default column %q color %q rejected by columnColor", title, got.Columns[i].Color)
		}
	}
}

func TestLookupTemplateFindsEveryListedKey(t *testing.T) {
	seen := map[string]bool{}
	for _, format := range BoardTemplates() {
		if format.Key == "" || format.Name == "" {
			t.Errorf("template missing key or name: %+v", format)
		}
		if seen[format.Key] {
			t.Errorf("duplicate template key %q", format.Key)
		}
		seen[format.Key] = true
		got, ok := LookupTemplate(format.Key)
		if !ok || got.Key != format.Key {
			t.Errorf("LookupTemplate(%q) = (%q, %v), want a hit", format.Key, got.Key, ok)
		}
		if len(format.Columns) < 2 {
			t.Errorf("template %q has %d columns, want at least 2", format.Key, len(format.Columns))
		}
	}
	if _, ok := LookupTemplate("not-a-template"); ok {
		t.Error("LookupTemplate must reject unknown keys")
	}
	if _, ok := LookupTemplate(""); ok {
		t.Error("LookupTemplate must reject the empty key")
	}
}
