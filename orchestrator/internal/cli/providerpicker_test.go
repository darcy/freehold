package cli

import (
	"testing"

	"github.com/charmbracelet/bubbles/list"
	tea "github.com/charmbracelet/bubbletea"
)

// TestProviderItemFilterValue: the type-ahead search key is the provider name
// (a FilterValue on every item drives the list filter by typing).
func TestProviderItemFilterValue(t *testing.T) {
	it := providerItem{name: "route53"}
	if it.FilterValue() != "route53" || it.Title() != "route53" {
		t.Errorf("item = %q / %q", it.FilterValue(), it.Title())
	}
}

// TestProviderPickerEnterSelects: pressing Enter on the (single) item returns a
// Quit with that provider chosen.
func TestProviderPickerEnterSelects(t *testing.T) {
	m := &providerPicker{list: list.New(
		[]list.Item{providerItem{"route53"}},
		list.NewDefaultDelegate(), 40, 10,
	)}
	got, _ := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	pm, ok := got.(*providerPicker)
	if !ok || pm.chosen == nil || *pm.chosen != "route53" {
		t.Fatalf("enter did not select route53: %+v", got)
	}
	if !pm.done {
		t.Errorf("picker should be done after Enter")
	}
}

// TestProviderPickerQuitNoChoice: q aborts (outside filtering) without choosing.
func TestProviderPickerQuitNoChoice(t *testing.T) {
	m := &providerPicker{list: list.New([]list.Item{providerItem{"route53"}}, list.NewDefaultDelegate(), 40, 10)}
	got, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("q")})
	if pm, ok := got.(*providerPicker); !ok || pm.chosen != nil {
		t.Errorf("q should leave chosen nil, got %+v", got)
	}
}

// TestProviderPickerQWhileFiltering: with type-ahead filtering active, typing
// 'q' must reach the filter box (providers like "httpreq" contain 'q') rather
// than quit — the failure mode the unconditional-quit guard lived in.
func TestProviderPickerQWhileFiltering(t *testing.T) {
	m := &providerPicker{list: list.New(
		[]list.Item{providerItem{"httpreq"}, providerItem{"acmedns"}},
		list.NewDefaultDelegate(), 40, 10,
	)}
	m.list.Filter = list.DefaultFilter
	m.list.SetFilteringEnabled(true)
	// '/' toggles into filter mode (bubbles default), then printable chars type
	m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("/")})
	if m.list.FilterState() != list.Filtering {
		t.Fatalf("expected Filtering after '/', got %v", m.list.FilterState())
	}
	// now 'q' must NOT quit while filtering
	got, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("q")})
	pm, ok := got.(*providerPicker)
	if !ok {
		t.Fatalf("q during filtering must not terminate the picker, got %T", got)
	}
	if pm.chosen != nil || pm.done {
		t.Errorf("q during filtering must not select/quit, done=%v chosen=%v", pm.done, pm.chosen)
	}
	// the filter term should now include the typed 'q'
	if pm.list.FilterState() != list.Filtering {
		t.Errorf("filter state = %v, want still Filtering", pm.list.FilterState())
	}
}
