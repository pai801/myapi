package model

import (
	"testing"

	. "github.com/smartystreets/goconvey/convey"
)

func TestSimplifyModelsField(t *testing.T) {
	Convey("SimplifyModelsField converts model names to aliases", t, func() {
		Convey("simplifies multiple models", func() {
			models := "gpt-4-turbo,gpt-3.5-turbo"
			result := SimplifyModelsField(&models)
			So(*result, ShouldEqual, "gpt4turbo,gpt35turbo")
		})

		Convey("simplifies empty string", func() {
			models := ""
			result := SimplifyModelsField(&models)
			So(*result, ShouldEqual, "")
		})

		Convey("returns nil for nil input", func() {
			result := SimplifyModelsField(nil)
			So(result, ShouldBeNil)
		})
	})
}