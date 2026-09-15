package controller

import (
	"context"
	"net/http"
	"net/http/httptest"
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
	// (DB write) or Emit (goroutine) + CooldownGlobal.ReportFailure (in-memory).
	//
	// Since DisableChannel requires DB access, we test the path where
	// AutomaticDisableChannelEnabled is false, which triggers the cooldown path.
	Convey("processChannelRelayError with disable disabled goes to cooldown path", t, func() {
		// DisableChannel is disabled by default, so ShouldDisableChannel returns false
		// and we hit the cooldown path (Emit + CooldownGlobal.ReportFailure)
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
			processChannelRelayError(context.Background(), 1, 1, "test-ch", "test-model", err)
		}, ShouldNotPanic)
	})
}

// TestShouldRetry_CommittedResponseSuppressesRetry 锁定契约 §1.3：SSE 响应已提交后不得重试，
// 否则会向同一响应追加第二段流。
func TestShouldRetry_CommittedResponseSuppressesRetry(t *testing.T) {
	Convey("响应已提交后 shouldRetry 必须为 false", t, func() {
		bizErr := &model.ErrorWithStatusCode{
			StatusCode: http.StatusBadGateway,
			Error:      model.Error{Message: "responses stream failed", Type: "upstream_error", Code: "invalid_upstream_response"},
		}

		recorder := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(recorder)
		// 未提交：5xx 语义允许重试（保持既有行为）
		So(shouldRetry(c, bizErr), ShouldBeTrue)

		// 一旦写出 body（SSE 已提交），不得再重试
		_, _ = c.Writer.WriteString("data: {\"error\":\"x\"}\n\n")
		So(c.Writer.Written(), ShouldBeTrue)
		So(shouldRetry(c, bizErr), ShouldBeFalse)
	})
}

// TestResponseCommittedDetection_WriteHeaderAloneIsNotCommitted 记录 gin Written() 语义（本修复的判定依据）：
// 仅 WriteHeader 不置位（非流式只写状态码时不得据此抑制重试/JSON），写出 body 或 Flush 后才算已提交。
// 两条失败路径（codex StreamResponsesHandler handler.go:163、chatgptsub streamChatFromResponses main.go:261）
// 均在 WriteHeader 后立即调用 c.Writer.Flush()，Flush 内部 WriteHeaderNow 会置位 Written()，故返回错误时可靠。
func TestResponseCommittedDetection_WriteHeaderAloneIsNotCommitted(t *testing.T) {
	Convey("gin Written() 判定", t, func() {
		recorder := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(recorder)
		So(c.Writer.Written(), ShouldBeFalse)

		c.Writer.WriteHeader(http.StatusOK)
		So(c.Writer.Written(), ShouldBeFalse)

		_, _ = c.Writer.WriteString("data: {\"error\":\"x\"}\n\n")
		So(c.Writer.Written(), ShouldBeTrue)
	})
}

// TestRenderFinalRelayError_SkipsCommittedResponse 锁定契约 §1.3：最终 JSON 错误体只在响应
// 未提交时写出；已提交时不得向 SSE 响应追加任何内容。
func TestRenderFinalRelayError_SkipsCommittedResponse(t *testing.T) {
	bizErr := &model.ErrorWithStatusCode{
		StatusCode: http.StatusBadGateway,
		Error:      model.Error{Message: "responses stream failed", Type: "upstream_error", Code: "invalid_upstream_response"},
	}

	Convey("最终错误体渲染", t, func() {
		Convey("未提交：写出 502 JSON 错误体", func() {
			recorder := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(recorder)
			renderFinalRelayError(c, bizErr)
			So(recorder.Code, ShouldEqual, http.StatusBadGateway)
			So(recorder.Body.String(), ShouldContainSubstring, `"error"`)
			So(recorder.Body.String(), ShouldContainSubstring, "invalid_upstream_response")
		})

		Convey("已提交：不追加任何内容", func() {
			recorder := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(recorder)
			_, _ = c.Writer.WriteString("data: {\"error\":\"x\"}\n\n")
			committed := recorder.Body.String()
			renderFinalRelayError(c, bizErr)
			So(recorder.Body.String(), ShouldEqual, committed)
		})
	})
}
