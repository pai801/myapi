package controller

import (
	"testing"
)

// TestRelayTextHelper_ShouldUseSuggestedModel verifies that when SuggestedModel
// is set in the gin context, it overrides textRequest.Model before model mapping.
//
// Requires database connection; full e2e coverage is in Task 3.1.
func TestRelayTextHelper_ShouldUseSuggestedModel(t *testing.T) {
	t.Skip("requires database connection; covered by Task 3.1 e2e tests")
}

// TestRelayTextHelper_ShouldUseOriginalModelWhenSuggestedNotSet verifies that
// when SuggestedModel is NOT set, textRequest.Model stays unchanged.
//
// Requires database connection; full e2e coverage is in Task 3.1.
func TestRelayTextHelper_ShouldUseOriginalModelWhenSuggestedNotSet(t *testing.T) {
	t.Skip("requires database connection; covered by Task 3.1 e2e tests")
}
