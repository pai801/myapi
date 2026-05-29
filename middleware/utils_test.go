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