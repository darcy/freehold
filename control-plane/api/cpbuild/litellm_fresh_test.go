package cpbuild

import (
	"errors"
	"strings"
	"testing"
)

// TestIsFreshGatewayNoModels: a fresh gateway's empty-model 500 (body riding
// litellmGet's error text) is recognized as the zero-registry shape this
// stage fills — any other failure (401, wrapped shapes, nil) stays loud.
func TestIsFreshGatewayNoModels(t *testing.T) {
	fresh := errors.New("GET /model/info: HTTP 500: {\"detail\":{\"error\":\"LLM Model List not loaded in. Make sure you passed models in your config.yaml or on the LiteLLM Admin UI. - https://docs.litellm.ai/docs/proxy/configs\"}}")
	if !isFreshGatewayNoModels(fresh) {
		t.Fatal("fresh-gateway 500 not recognized")
	}
	if isFreshGatewayNoModels(errors.New("GET /model/info: HTTP 401: unauthorized")) {
		t.Fatal("401 misread as the fresh shape")
	}
	if isFreshGatewayNoModels(errors.New("GET /model/info: HTTP 500: {\"detail\":{\"error\":\"connection refused\"}}")) {
		t.Fatal("unrelated 500 misread as the fresh shape")
	}
	if isFreshGatewayNoModels(nil) {
		t.Fatal("nil misread as the fresh shape")
	}
	if !strings.Contains(fresh.Error(), freshGatewayNoModels) {
		t.Fatal("sentinel missing from the live body")
	}
}
