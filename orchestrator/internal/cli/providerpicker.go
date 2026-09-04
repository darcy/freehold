package cli

import (
	"fmt"
	"os"

	"github.com/charmbracelet/bubbles/list"
	tea "github.com/charmbracelet/bubbletea"
)

// providerItem is one DNS-01 provider in the picker list.
type providerItem struct{ name string }

func (p providerItem) Title() string       { return p.name }
func (p providerItem) Description() string { return "DNS-01 provider" }
func (p providerItem) FilterValue() string { return p.name }

// providerPicker is a scrollable/paginated, filterable list over lego's full
// provider registry — replacing the unreadable enumerate-and-type prompt for
// the 201 supported providers (roadmap/CORE_TLS.md F3). Uses the SIMPLE
// delegate: one provider per line, no description padding; type-ahead filtering
// (FilterValue = provider name) still works.
type providerPicker struct {
	list   list.Model
	chosen *string
	done   bool
}

func runProviderPicker(providers []string) (string, error) {
	items := make([]list.Item, 0, len(providers))
	for _, n := range providers {
		items = append(items, providerItem{n})
	}
	delegate := list.NewDefaultDelegate()
	// Compact single-line rows: hide the (identical) description subtitle and
	// collapse the per-item padding, so the 201 providers show densely.
	delegate.ShowDescription = false
	delegate.SetHeight(1)
	delegate.SetSpacing(0)
	m := &providerPicker{list: list.New(items, delegate, 40, 18)}
	m.list.Title = "Choose the DNS-01 provider for the wildcard cert"
	m.list.SetShowStatusBar(false)

	p := tea.NewProgram(m, tea.WithInput(os.Stdin))
	model, err := p.Run()
	if err != nil {
		return "", err
	}
	picked, ok := model.(*providerPicker)
	if !ok || picked.chosen == nil {
		return "", fmt.Errorf("provider selection aborted")
	}
	return *picked.chosen, nil
}

func (m *providerPicker) Init() tea.Cmd { return nil }

func (m *providerPicker) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		switch msg.String() {
		case "enter":
			if it, ok := m.list.SelectedItem().(providerItem); ok {
				name := it.name
				m.chosen = &name
				m.done = true
				return m, tea.Quit
			}
		case "q", "ctrl+c":
			return m, tea.Quit
		}
	}
	var cmd tea.Cmd
	m.list, cmd = m.list.Update(msg)
	return m, cmd
}

func (m *providerPicker) View() string {
	if m.done {
		return ""
	}
	return m.list.View()
}
