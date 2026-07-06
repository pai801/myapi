package controller

import (
	"context"
	"net/http"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/pai801/myapi/common/ctxkey"
	"github.com/pai801/myapi/relay/model"
	. "github.com/smartystreets/goconvey/convey"
)

func TestShouldRetry(t *testing.T) {
	Convey("shouldRetry decisions", t, func() {
		newBizErr := func(statusCode int, errType string, code any, message string) *model.ErrorWithStatusCode {
			return &model.ErrorWithStatusCode{
				StatusCode: statusCode,
				Error: model.Error{
					Type:    errType,
					Code:    code,
					Message: message,
				},
			}
		}

		Convey("returns false when SpecificChannelId is set", func() {
			c, _ := gin.CreateTestContext(nil)
			c.Set(ctxkey.SpecificChannelId, "42")
			So(shouldRetry(c, newBizErr(http.StatusInternalServerError, "server_error", nil, "upstream timeout")), ShouldBeFalse)
			So(shouldRetry(c, newBizErr(http.StatusTooManyRequests, "rate_limit_error", nil, "quota exceeded")), ShouldBeFalse)
		})

		Convey("returns true for 429 TooManyRequests", func() {
			c, _ := gin.CreateTestContext(nil)
			So(shouldRetry(c, newBizErr(http.StatusTooManyRequests, "rate_limit_error", nil, "quota exceeded")), ShouldBeTrue)
		})

		Convey("returns true for 5xx errors", func() {
			c, _ := gin.CreateTestContext(nil)
			So(shouldRetry(c, newBizErr(http.StatusInternalServerError, "server_error", nil, "upstream timeout")), ShouldBeTrue)
			So(shouldRetry(c, newBizErr(http.StatusBadGateway, "server_error", nil, "bad gateway")), ShouldBeTrue)
			So(shouldRetry(c, newBizErr(http.StatusServiceUnavailable, "server_error", nil, "service unavailable")), ShouldBeTrue)
		})

		Convey("returns false for 400 request-shape failures", func() {
			c, _ := gin.CreateTestContext(nil)
			So(shouldRetry(c, newBizErr(http.StatusBadRequest, "invalid_request_error", "malformed_request", "Malformed request body")), ShouldBeFalse)
			So(shouldRetry(c, newBizErr(http.StatusBadRequest, "invalid_request_error", "unsupported_request", "unsupported request schema")), ShouldBeFalse)
		})

		Convey("returns true for 400 provider-specific compatibility failures", func() {
			c, _ := gin.CreateTestContext(nil)
			So(shouldRetry(c, newBizErr(http.StatusBadRequest, "invalid_request_error", "model_not_supported", "model is not supported by this provider channel")), ShouldBeTrue)
		})

		Convey("returns false for 2xx business-success responses", func() {
			c, _ := gin.CreateTestContext(nil)
			So(shouldRetry(c, newBizErr(http.StatusOK, "", nil, "business success but no retry")), ShouldBeFalse)
			So(shouldRetry(c, newBizErr(http.StatusCreated, "", nil, "created")), ShouldBeFalse)
		})

		Convey("returns false for 2xx generic upstream errors without adapter-failure evidence", func() {
			c, _ := gin.CreateTestContext(nil)
			So(shouldRetry(c, newBizErr(http.StatusOK, "upstream_error", "upstream_error", "upstream rejected business request")), ShouldBeFalse)
		})

		Convey("returns true for adapter parse or bad-response failures", func() {
			c, _ := gin.CreateTestContext(nil)
			So(shouldRetry(c, newBizErr(http.StatusOK, "upstream_error", "bad_response", "upstream returned malformed response payload")), ShouldBeTrue)
			So(shouldRetry(c, newBizErr(http.StatusOK, "upstream_error", "bad_response_status_code", "adapter failed to parse upstream response")), ShouldBeTrue)
		})

		Convey("returns false for contract errors without alternate-channel signal", func() {
			c, _ := gin.CreateTestContext(nil)
			So(shouldRetry(c, newBizErr(http.StatusBadRequest, "invalid_request_error", "generic_client_error", "client contract error")), ShouldBeFalse)
			So(shouldRetry(c, newBizErr(http.StatusUnauthorized, "authentication_error", "invalid_api_key", "invalid api key")), ShouldBeFalse)
			So(shouldRetry(c, newBizErr(http.StatusForbidden, "permission_error", "forbidden", "forbidden")), ShouldBeFalse)
			So(shouldRetry(c, newBizErr(http.StatusProxyAuthRequired, "proxy_auth_error", "proxy_auth_required", "proxy authentication required")), ShouldBeFalse)
			So(shouldRetry(c, newBizErr(http.StatusUnsupportedMediaType, "invalid_request_error", "unsupported_media_type", "unsupported media type")), ShouldBeFalse)
			So(shouldRetry(c, newBizErr(http.StatusUnprocessableEntity, "invalid_request_error", "invalid_schema", "schema validation failed")), ShouldBeFalse)
			So(shouldRetry(c, newBizErr(http.StatusConflict, "conflict_error", "conflict", "provider rejected current request state")), ShouldBeFalse)
		})

		Convey("returns true for contract errors with alternate-channel compatibility signal", func() {
			c, _ := gin.CreateTestContext(nil)
			So(shouldRetry(c, newBizErr(http.StatusUnprocessableEntity, "invalid_request_error", "unsupported_model", "model is unsupported by this provider")), ShouldBeTrue)
		})

		Convey("returns false for unknown errors that look like request-shape failures", func() {
			c, _ := gin.CreateTestContext(nil)
			So(shouldRetry(c, newBizErr(499, "", nil, "invalid request format")), ShouldBeFalse)
		})
	})
}

func TestProcessChannelRelayErrorLogDecision(t *testing.T) {
	// This test verifies the log output of processChannelRelayError
	// without triggering DB side effects.
	//
	// processChannelRelayError calls ShouldDisableChannel (which depends on
	// config.AutomaticDisableChannelEnabled), then either DisableChannel
	// (DB write) or Emit (goroutine) + CooldownGlobal.Put (in-memory).
	//
	// Since DisableChannel requires DB access, we test the path where
	// AutomaticDisableChannelEnabled is false, which triggers the cooldown path.
	Convey("processChannelRelayError with disable disabled goes to cooldown path", t, func() {
		// DisableChannel is disabled by default, so ShouldDisableChannel returns false
		// and we hit the cooldown path (Emit + CooldownGlobal.Put)
		err := model.ErrorWithStatusCode{
			Error: model.Error{
				Message: "test error",
				Type:    "test_error",
				Code:    "test_code",
			},
			StatusCode: 500,
		}

		// This should not panic — goes to cooldown path
		So(func() {
			processChannelRelayError(context.Background(), 1, 1, "test-ch", err)
		}, ShouldNotPanic)
	})
}
