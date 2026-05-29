package model

import (
	"testing"

	. "github.com/smartystreets/goconvey/convey"
)

func TestChannelModelsAlias(t *testing.T) {
	Convey("Channel struct should have ModelsAlias field", t, func() {
		c := Channel{ModelsAlias: "test-model"}
		So(c.ModelsAlias, ShouldEqual, "test-model")
	})
}

func TestSimplifyModelName(t *testing.T) {
	Convey("SimplifyModelName should remove non-alphanumeric chars and lowercase", t, func() {
		So(SimplifyModelName("gpt-4-turbo"), ShouldEqual, "gpt4turbo")
	})
}

func TestAutoGenerateModelsAlias(t *testing.T) {
	Convey("autoGenerateModelsAlias generates alias from Models", t, func() {
		c := Channel{Models: "gpt-4-turbo,gpt-3.5-turbo"}
		c.autoGenerateModelsAlias()
		So(c.ModelsAlias, ShouldEqual, "gpt4turbo,gpt35turbo")
	})

	Convey("autoGenerateModelsAlias sets empty when Models is empty", t, func() {
		c := Channel{Models: ""}
		c.autoGenerateModelsAlias()
		So(c.ModelsAlias, ShouldEqual, "")
	})
}