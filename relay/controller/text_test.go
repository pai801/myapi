package controller

import (
	"errors"
	"fmt"
	"net/http"
	"testing"

	"github.com/pai801/myapi/relay/model"
	"github.com/stretchr/testify/assert"
)

// TestClassifyConversionErrorMapsProtocolErrorToBadRequest 锁定 T2 Error Propagation：
// *model.ProtocolConversionError → HTTP 400 + invalid_request_error + 稳定机器码；
// 其他转换基础设施错误（如包装过的同类之外的错误）→ 500。
func TestClassifyConversionErrorMapsProtocolErrorToBadRequest(t *testing.T) {
	convErr := &model.ProtocolConversionError{
		Code:      model.CodeUnsupportedMapping,
		Direction: model.DirectionChatRequestToResponses,
		Path:      "stop",
		Cause:     errors.New("Responses request has no stop field"),
	}

	// 直接命中
	ewe := classifyConversionError(convErr)
	assert.Equal(t, http.StatusBadRequest, ewe.StatusCode)
	assert.Equal(t, "invalid_request_error", ewe.Type)
	assert.Equal(t, model.CodeUnsupportedMapping, ewe.Code)
	assert.Equal(t, "stop", ewe.Param)

	// wrapped 错误仍经 errors.As 归类为 400
	ewe = classifyConversionError(fmt.Errorf("convert failed: %w", convErr))
	assert.Equal(t, http.StatusBadRequest, ewe.StatusCode)
	assert.Equal(t, "invalid_request_error", ewe.Type)

	// 非协议转换错误保持 500 基础设施归类
	ewe = classifyConversionError(errors.New("json marshal failed"))
	assert.Equal(t, http.StatusInternalServerError, ewe.StatusCode)
	assert.Equal(t, "convert_request_failed", ewe.Code)
}

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
