package monitor

import (
	"testing"

	"github.com/pai801/myapi/common/config"
	"github.com/pai801/myapi/relay/model"
	. "github.com/smartystreets/goconvey/convey"
)

func TestShouldDisableChannel(t *testing.T) {
	// Save and restore config
	original := config.AutomaticDisableChannelEnabled
	config.AutomaticDisableChannelEnabled = true
	defer func() { config.AutomaticDisableChannelEnabled = original }()

	Convey("ShouldDisableChannel rules", t, func() {
		Convey("returns false when disabled in config", func() {
			config.AutomaticDisableChannelEnabled = false
			defer func() { config.AutomaticDisableChannelEnabled = true }()
			err := &model.Error{Message: "test", Type: "insufficient_quota", Code: "test"}
			So(ShouldDisableChannel(err, 400), ShouldBeFalse)
		})

		Convey("returns false for nil error", func() {
			So(ShouldDisableChannel(nil, 500), ShouldBeFalse)
		})

		Convey("returns true for 401 Unauthorized", func() {
			err := &model.Error{Message: "unauthorized", Type: "error", Code: "test"}
			So(ShouldDisableChannel(err, 401), ShouldBeTrue)
		})

		Convey("returns true for insufficient_quota type", func() {
			err := &model.Error{Message: "quota exceeded", Type: "insufficient_quota", Code: "test"}
			So(ShouldDisableChannel(err, 400), ShouldBeTrue)
		})

		Convey("returns true for authentication_error type", func() {
			err := &model.Error{Message: "auth failed", Type: "authentication_error", Code: "test"}
			So(ShouldDisableChannel(err, 401), ShouldBeTrue)
		})

		Convey("returns true for invalid_api_key code", func() {
			err := &model.Error{Message: "key invalid", Type: "error", Code: "invalid_api_key"}
			So(ShouldDisableChannel(err, 400), ShouldBeTrue)
		})

		Convey("returns true for credit balance message", func() {
			err := &model.Error{Message: "your credit balance is too low", Type: "error", Code: "test"}
			So(ShouldDisableChannel(err, 400), ShouldBeTrue)
		})

		Convey("returns true for permission denied message", func() {
			err := &model.Error{Message: "permission denied", Type: "error", Code: "test"}
			So(ShouldDisableChannel(err, 403), ShouldBeTrue)
		})

		Convey("returns false for generic error", func() {
			err := &model.Error{Message: "rate limit exceeded", Type: "error", Code: "test"}
			So(ShouldDisableChannel(err, 429), ShouldBeFalse)
		})
	})
}
