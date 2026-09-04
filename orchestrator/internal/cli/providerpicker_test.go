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

// TestProviderPickerQuitNoChoice: q aborts without choosing.
func TestProviderPickerQuitNoChoice(t *testing.T) {
	m := &providerPicker{list: list.New([]list.Item{providerItem{"route53"}}, list.NewDefaultDelegate(), 40, 10)}
	got, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("q")})
	if pm, ok := got.(*providerPicker); !ok || pm.chosen != nil {
		t.Errorf("q should leave chosen nil, got %+v", got)
	}
}
