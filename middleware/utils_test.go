package middleware

import (
	"testing"

	. "github.com/smartystreets/goconvey/convey"
)

func TestIsModelInListAliasExact(t *testing.T) {
	Convey("isModelInList with alias exact match", t, func() {
		// modelName="gpt4turbo" (already simplified), models="gpt4turbo,gpt35turbo"
		So(isModelInList("gpt4turbo", "gpt4turbo,gpt35turbo"), ShouldBeTrue)
	})
}

func TestIsModelInListAliasPrefix(t *testing.T) {
	Convey("isModelInList with alias prefix match", t, func() {
		// modelName="gpt-4" simplifies to "gpt4", which is a prefix of "gpt4turbo"
		So(isModelInList("gpt-4", "gpt4turbo,gpt35turbo"), ShouldBeTrue)
	})
}

func TestIsModelInListEmptyModels(t *testing.T) {
	Convey("isModelInList with empty models returns true", t, func() {
		// empty models means no restriction
		So(isModelInList("gpt4", ""), ShouldBeTrue)
	})
}

func TestIsModelInListUnknownModel(t *testing.T) {
	Convey("isModelInList with unknown model returns false", t, func() {
		// model not in the list
		So(isModelInList("claude3", "gpt4turbo,gpt35turbo"), ShouldBeFalse)
	})
}

func TestIsModelInListAuto(t *testing.T) {
	Convey("isModelInList with auto returns true", t, func() {
		// auto models should pass through
		So(isModelInList("auto", "gpt4turbo,gpt35turbo"), ShouldBeTrue)
		So(isModelInList("auto", ""), ShouldBeTrue)
	})
}

// ─── Original (unsimplified) model names in stored list ───
// After the bugfix, Token.Models now stores the original model name as-is
// (e.g. "gpt-4-turbo" instead of "gpt4turbo"). These tests verify
// isModelInList still works with unsimplified names in the stored list.

func TestIsModelInListOriginalNameExact(t *testing.T) {
	Convey("original name in stored list matches exact", t, func() {
		// Stored models contain original names with special chars
		So(isModelInList("gpt-4-turbo", "gpt-4-turbo,gpt-3.5-turbo"), ShouldBeTrue)
	})
}

func TestIsModelInListOriginalNamePrefix(t *testing.T) {
	Convey("simplified request matches prefix of original stored name", t, func() {
		// Request "gpt-4" simplifies to "gpt4", stored "gpt-4-turbo" simplifies to "gpt4turbo"
		// "gpt4" is a prefix of "gpt4turbo"
		So(isModelInList("gpt-4", "gpt-4-turbo,gpt-3.5-turbo"), ShouldBeTrue)
	})
}

func TestIsModelInListOriginalNameUnknown(t *testing.T) {
	Convey("unknown model returns false with original names", t, func() {
		So(isModelInList("claude-3", "gpt-4-turbo,gpt-3.5-turbo"), ShouldBeFalse)
	})
}

func TestIsModelInListOriginalNameCaseInsensitive(t *testing.T) {
	Convey("matching is case-insensitive", t, func() {
		So(isModelInList("GPT-4-Turbo", "gpt-4-turbo"), ShouldBeTrue)
	})
}

func TestIsModelInListMixedOldNewData(t *testing.T) {
	Convey("mixed old (simplified) and new (original) data", t, func() {
		// Backward compat: old tokens stored simplified names, new tokens store original names
		// Both must work with the same matching logic
		So(isModelInList("gpt-4", "gpt4turbo,gpt-3.5-turbo"), ShouldBeTrue)
		So(isModelInList("gpt-3.5", "gpt-4-turbo,gpt35turbo"), ShouldBeTrue)
	})
}