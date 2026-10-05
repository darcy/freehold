package build

import (
	"fmt"
	"os"
	"strings"

	"github.com/charmbracelet/bubbles/list"
	tea "github.com/charmbracelet/bubbletea"

	"freehold/contract/litellm"
)

// The litellm gateway setup: at first provision the operator picks ONE
// provider (single-API-key auth only — no Azure/Bedrock/Vertex) from the
// curated table, enters its API key, and confirms/overrides its default
// model. The choice backs ALL the gateway's aliases and rides the CP's
// litellm store (provider-prefix / provider-model), so a rebuild re-registers
// it; the AI department retargets or adds providers later through the
// litellm-api-admin door.

// gatewaySetup is one collected provider choice.
type gatewaySetup struct {
	Provider string // the litellm prefix (e.g. fireworks_ai)
	Model    string // the model id after the prefix
	Key      string // the provider API key
}

// providerItem is one provider in the picker list.
type providerItem struct{ litellm.Provider }

func (p providerItem) Title() string       { return p.Name }
func (p providerItem) Description() string { return p.Desc }
func (p providerItem) FilterValue() string { return p.Name }

// providerPicker is a scrollable/paginated, filterable single-select list
// over the curated provider table (the certcred DNS picker's shape).
type providerPicker struct {
	list   list.Model
	chosen *litellm.Provider
	done   bool
}

// runProviderPicker shows the picker and returns the chosen provider.
func runProviderPicker() (*litellm.Provider, error) {
	tbl := litellm.Providers()
	items := make([]list.Item, 0, len(tbl))
	for _, p := range tbl {
		items = append(items, providerItem{p})
	}
	delegate := list.NewDefaultDelegate()
	delegate.ShowDescription = true
	delegate.SetHeight(1)
	delegate.SetSpacing(0)
	m := &providerPicker{list: list.New(items, delegate, 70, 14)}
	m.list.Title = "Which AI provider backs the litellm gateway? (all aliases ride it)"
	m.list.SetShowStatusBar(false)
	m.list.Filter = list.DefaultFilter
	m.list.SetFilteringEnabled(true)

	p := tea.NewProgram(m, tea.WithInput(os.Stdin))
	model, err := p.Run()
	if err != nil {
		return nil, err
	}
	picked, ok := model.(*providerPicker)
	if !ok || picked.chosen == nil {
		return nil, fmt.Errorf("provider selection aborted")
	}
	return picked.chosen, nil
}

func (m *providerPicker) Init() tea.Cmd { return nil }

func (m *providerPicker) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		switch msg.String() {
		case "enter":
			if it, ok := m.list.SelectedItem().(providerItem); ok {
				p := it.Provider
				m.chosen = &p
				m.done = true
				return m, tea.Quit
			}
		case "q":
			if m.list.FilterState() != list.Filtering {
				return m, tea.Quit
			}
		case "ctrl+c":
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

// collectGatewaySetup runs the interactive provider -> key -> model flow.
func (e *buildEngine) collectGatewaySetup() (gatewaySetup, error) {
	p, err := runProviderPicker()
	if err != nil {
		return gatewaySetup{}, err
	}
	cc := e.cc()
	fmt.Fprintf(e.Out, "  · %s — one API key, %s's prompt format (editable model)\n", p.Desc, p.Prefix)
	key, err := cc.PromptSecret(fmt.Sprintf("%s API key", p.Name))
	if err != nil {
		return gatewaySetup{}, err
	}
	key = strings.TrimSpace(key)
	if key == "" {
		return gatewaySetup{}, fmt.Errorf("no %s API key supplied", p.Name)
	}
	model, err := cc.Prompt(fmt.Sprintf("model (Enter for %s)", p.DefaultModel))
	if err != nil {
		return gatewaySetup{}, err
	}
	model = strings.TrimSpace(model)
	if model == "" {
		model = p.DefaultModel
	}
	return gatewaySetup{Provider: p.Prefix, Model: model, Key: key}, nil
}
