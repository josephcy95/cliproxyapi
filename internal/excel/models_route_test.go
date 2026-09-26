package excel

import "testing"

func TestIsRoute(t *testing.T) {
	for _, model := range []string{"gpt-6-astra-excel", "kiro/gpt-6-astra-excel", "paid/gpt-5.6-sol-excel(high)", " GPT-6-ASTRA-EXCEL "} {
		if !IsRoute(model) {
			t.Errorf("route missed: %q", model)
		}
	}
	for _, model := range []string{"gpt-6-astra", "kiro/gpt-6-astra", "gpt-5.6-sol(high)", "ordinary-excel", ""} {
		if IsRoute(model) {
			t.Errorf("ordinary model captured: %q", model)
		}
	}
}
