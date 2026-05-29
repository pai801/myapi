package model

import (
	"testing"

	. "github.com/smartystreets/goconvey/convey"
)

func TestLogDetailFields(t *testing.T) {
	Convey("Log struct should have detail fields", t, func() {
		l := Log{
			ChannelName:   "test-channel",
			RequestBody:   "{\"key\":\"value\"}",
			ResponseBody:  "{\"result\":\"ok\"}",
			RequestHeader: "Content-Type: application/json",
		}
		So(l.ChannelName, ShouldEqual, "test-channel")
		So(l.RequestBody, ShouldEqual, "{\"key\":\"value\"}")
		So(l.ResponseBody, ShouldEqual, "{\"result\":\"ok\"}")
		So(l.RequestHeader, ShouldEqual, "Content-Type: application/json")
	})
}