package controller

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/pai801/myapi/common"
	"github.com/pai801/myapi/common/config"
	"github.com/pai801/myapi/common/ctxkey"
	"github.com/pai801/myapi/common/helper"
	"github.com/pai801/myapi/common/logger"
	"github.com/pai801/myapi/middleware"
	dbmodel "github.com/pai801/myapi/model"
	"github.com/pai801/myapi/monitor"
	"github.com/pai801/myapi/relay/active"
	"github.com/pai801/myapi/relay/controller"
	metaPkg "github.com/pai801/myapi/relay/meta"
	"github.com/pai801/myapi/relay/model"
	"github.com/pai801/myapi/relay/relaymode"
)

// https://platform.openai.com/docs/api-reference/chat

func relayHelper(c *gin.Context, relayMode int) *model.ErrorWithStatusCode {
	var err *model.ErrorWithStatusCode
	switch relayMode {
	case relaymode.Responses:
		fallthrough
	case relaymode.ResponsesCompact:
		err = controller.RelayResponsesHelper(c)
	case relaymode.ImagesGenerations:
		err = controller.RelayImageHelper(c, relayMode)
	case relaymode.AudioSpeech:
		fallthrough
	case relaymode.AudioTranslation:
		fallthrough
	case relaymode.AudioTranscription:
		err = controller.RelayAudioHelper(c, relayMode)
	case relaymode.Proxy:
		err = controller.RelayProxyHelper(c, relayMode)
	default:
		err = controller.RelayTextHelper(c)
	}
	return err
}

func Relay(c *gin.Context) {
	ctx := c.Request.Context()
	relayMode := relaymode.GetByPath(c.Request.URL.Path)
	if config.DebugEnabled {
		requestBody, _ := common.GetRequestBody(c)
		logger.Log.Debugf("request body: %s", string(requestBody))
	}
	channelId := c.GetInt(ctxkey.ChannelId)
	userId := c.GetInt(ctxkey.Id)
	requestModel := c.GetString(ctxkey.RequestModel)
	if requestModel == "" {
		requestModel = "auto"
	}
	// 亲和 scope 由 Distribute 中间件写入。理论上不经该中间件的调用方会取不到，
	// 兜底为 user 层（行为退化为现状，属可接受降级，不报错）。
	rawScope, _ := c.Get(ctxkey.AffinityScope)
	scope, ok := rawScope.(middleware.AffinityScope)
	if !ok {
		// 兜底 scope 的 Group 必须与 Distribute 的 tokenGroup 同源（"" → "default"），
		// 否则同一用户的亲和键会被切成两个命名空间（见 middleware/distributor.go Distribute）。
		group := c.GetString(ctxkey.Group)
		if group == "" {
			group = "default"
		}
		scope = middleware.AffinityScope{UserID: userId, Group: group}
	}
	requestId := c.GetString(helper.RequestIdKey)
	meta := metaPkg.GetByContext(c)
	if activeReq := buildActiveRequest(c, meta, requestId); activeReq != nil {
		active.Global.Add(activeReq)
		defer active.Global.Remove(requestId)
	}
	// 每次尝试前清空「实际渠道」记录：ActualChannelId 由 relay 路径（DoRequest 后）写入，
	// 清空可避免上一次尝试的残留值跨尝试污染归因（见 resolveActualChannelId）。
	c.Set(ctxkey.ActualChannelId, 0)
	c.Set(ctxkey.ActualChannelName, "")
	bizErr := relayHelper(c, relayMode)
	if bizErr == nil {
		// 归因按「实际服务渠道」：sticky 可能把请求改到另一个渠道（见 resolveActualChannelId）。
		actualChannelId := recordSuccessAttribution(c, channelId, requestModel, scope)
		monitor.Emit(actualChannelId, true)
		return
	}
	lastFailedChannelId := channelId
	channelName := resolveActualChannelName(c, c.GetString(ctxkey.ChannelName))
	group := c.GetString(ctxkey.Group)
	failedModel := c.GetString(ctxkey.SuggestedModel)
	// 失败归因同样按「实际服务渠道」：否则可能因实际渠道 B 的失败而冷却 / 禁用选路渠道 A。
	go processChannelRelayError(ctx, userId, resolveActualChannelId(c, channelId), channelName, failedModel, *bizErr)
	retryTimes := config.RetryTimes
	if !shouldRetry(c, bizErr) {
		logger.Log.Infof("shouldRetry=false statusCode=%d requestId=%s lastFailedChannel=%d", bizErr.StatusCode, requestId, lastFailedChannelId)
		retryTimes = 0
	} else {
		logger.Log.Debugf("shouldRetry=true statusCode=%d retryTimes=%d requestId=%s", bizErr.StatusCode, retryTimes, requestId)
	}
	// Use the original request model (could be "auto" or specific model)
	// to maintain proper distribution behavior during retries.
	for i := retryTimes; i > 0; i-- {
		channel, suggestedModel, err := middleware.SelectChannel(ctx, group, requestModel, lastFailedChannelId, scope)
		if err != nil {
			logger.Log.Errorf("DistributeForRetry failed: %+v", err)
			break
		}
		logger.Log.Infof("retry attempt=%d remaining=%d failedChannel=%d selectedChannel=%d model=%s requestId=%s",
			retryTimes-i+1, i, lastFailedChannelId, channel.Id, suggestedModel, requestId)
		middleware.SetupContextForSelectedChannel(c, channel, suggestedModel)
		if active.Global.Get(requestId) != nil {
			active.Global.Update(requestId, func(req *active.ActiveRequest) {
				req.ChannelID = channel.Id
				req.ChannelName = channel.Name
				req.ModelName = suggestedModel
			})
		}
		requestBody, err := common.GetRequestBody(c)
		c.Request.Body = io.NopCloser(bytes.NewBuffer(requestBody))
		c.Set(ctxkey.ActualChannelId, 0) // 清空上一次尝试的实际渠道记录，避免跨尝试残留
		c.Set(ctxkey.ActualChannelName, "")
		bizErr = relayHelper(c, relayMode)
		if bizErr == nil {
			logger.Log.Infof("retry succeeded on channel #%d requestId=%s", resolveActualChannelId(c, channel.Id), requestId)
			// 归因按「实际服务渠道」，与首轮同口径；重试成功原不上报监控，此处保持原行为。
			recordSuccessAttribution(c, channel.Id, requestModel, scope)
			return
		}
		channelId = c.GetInt(ctxkey.ChannelId)
		lastFailedChannelId = channelId
		channelName = resolveActualChannelName(c, c.GetString(ctxkey.ChannelName))
		failedModel = c.GetString(ctxkey.SuggestedModel)
		logger.Log.Debugf("retry failed channel #%d status=%d requestId=%s", resolveActualChannelId(c, channelId), bizErr.StatusCode, requestId)
		// 失败归因同样按「实际服务渠道」，与首轮口径一致。
		go processChannelRelayError(ctx, userId, resolveActualChannelId(c, channelId), channelName, failedModel, *bizErr)
		// 本次重试已向客户端提交 SSE：继续重试会追加第二段流，必须停止（失败记账已在上方完成）。
		if c.Writer.Written() {
			logger.Log.Infof("retry response already committed, stop retrying requestId=%s", requestId)
			break
		}
	}
	if bizErr != nil {
		logger.Log.Infof("all retries exhausted lastFailedChannel=%d status=%d requestId=%s model=%s group=%s",
			lastFailedChannelId, bizErr.StatusCode, requestId, requestModel, group)
		if bizErr.StatusCode == http.StatusTooManyRequests {
			bizErr.Error.Message = "当前分组上游负载已饱和，请稍后再试"
		}

		// BUG: bizErr is in race condition
		bizErr.Error.Message = helper.MessageWithRequestId(bizErr.Error.Message, requestId)
		renderFinalRelayError(c, bizErr)
		recordFailureLog(c, bizErr, channelName)
	}
}

// resolveActualChannelId 返回归因应使用的渠道：若 relay 路径记录了 ActualChannelId
// （adaptor 层 sticky 强制改写了实际服务渠道），以实际渠道为准；否则退化为选路渠道 selected。
// 归因（亲和 / 冷却 / 监控）必须按实际服务渠道，否则会把成功 / 失败记到没实际服务的渠道上
// （见 relay/controller/helper.go recordActualChannel 与 relay/adaptor/chatgptsub sticky）。
func resolveActualChannelId(c *gin.Context, selected int) int {
	if actual := c.GetInt(ctxkey.ActualChannelId); actual > 0 && actual != selected {
		return actual
	}
	return selected
}

// resolveActualChannelName 返回失败日志应使用的渠道名：若 relay 路径记录了 ActualChannelName
// （adaptor 层 sticky 改写了实际服务渠道），以实际渠道名为准；否则退化为 fallback（选路渠道名）。
// 与 resolveActualChannelId 同口径：否则失败日志会出现「channel #B（A的名字）」的自相矛盾。
func resolveActualChannelName(c *gin.Context, fallback string) string {
	if name := c.GetString(ctxkey.ActualChannelName); name != "" {
		return name
	}
	return fallback
}

// recordSuccessAttribution 记录一次成功转发的归因：亲和写入 + 冷却清零，统一按「实际服务渠道」。
// 返回实际归因渠道，供调用方上报监控（monitor.Emit）。首轮与重试两处成功分支共用，避免归因口径分裂。
//
// 亲和写入全部可用层（turn + session + user），绝不能只写 keys[0]（最细层）：turn id 每轮换新，
// 只写 turn 会让下一轮 turn 键 miss、而 session / user 层从未写过也 miss，跨轮亲和彻底断链。
// auto 请求走 autoDistribute、从不查亲和，写入只会产生死键，故跳过（ShouldRecordAffinity）。
func recordSuccessAttribution(c *gin.Context, selectedChannelId int, requestModel string, scope middleware.AffinityScope) int {
	actualChannelId := resolveActualChannelId(c, selectedChannelId)
	if middleware.ShouldRecordAffinity(requestModel) {
		for _, key := range scope.KeysToSet(requestModel) {
			middleware.AffinityGlobal.Set(key, actualChannelId)
		}
	}
	middleware.CooldownGlobal.ResetSuccess(actualChannelId, c.GetString(ctxkey.SuggestedModel))
	return actualChannelId
}

// renderFinalRelayError 仅在响应未提交时写最终 JSON 错误体。
// SSE 已提交后禁止追加 JSON HTTP 错误体（协议契约 §1.3）；渠道失败记账与失败日志不受此判断影响。
func renderFinalRelayError(c *gin.Context, bizErr *model.ErrorWithStatusCode) {
	if c.Writer.Written() {
		return
	}
	c.JSON(bizErr.StatusCode, gin.H{
		"error": bizErr.Error,
	})
}

func shouldRetry(c *gin.Context, bizErr *model.ErrorWithStatusCode) bool {
	// 响应体已提交（SSE 已开始写 body）后重试会向同一响应追加第二段流，禁止重试。
	if c.Writer.Written() {
		return false
	}
	if _, ok := c.Get(ctxkey.SpecificChannelId); ok {
		return false
	}
	if bizErr == nil {
		return false
	}
	statusCode := bizErr.StatusCode
	if statusCode == http.StatusTooManyRequests {
		return true
	}
	if statusCode/100 == 5 {
		return true
	}
	if isExplicitAdapterFailure(bizErr) {
		return true
	}
	if statusCode/100 == 2 {
		return false
	}
	if statusCode == http.StatusBadRequest {
		return isProviderCompatibilityError(bizErr)
	}
	if isClientSideStatus(statusCode) {
		return isProviderCompatibilityError(bizErr)
	}
	if bizErr.LooksLikeRequestShapeFailure() {
		return false
	}
	return true
}

func isClientSideStatus(statusCode int) bool {
	return statusCode >= http.StatusBadRequest && statusCode < http.StatusInternalServerError
}

func isExplicitAdapterFailure(bizErr *model.ErrorWithStatusCode) bool {
	if bizErr == nil {
		return false
	}
	text := bizErr.SemanticText()
	if strings.Contains(text, "bad_response") {
		return true
	}
	transportMarkers := []string{
		"failed to parse",
		"parse upstream response",
		"malformed response",
		"invalid response",
		"bad response",
		"empty response",
		"resp is nil",
		"transport error",
		"response handling failure",
	}
	for _, marker := range transportMarkers {
		if strings.Contains(text, marker) {
			return true
		}
	}
	return false
}

func isProviderCompatibilityError(bizErr *model.ErrorWithStatusCode) bool {
	if bizErr == nil {
		return false
	}
	text := bizErr.SemanticText()
	compatibilityMarkers := []string{
		"unsupported_model",
		"model_not_supported",
		"model is not supported",
		"unsupported by this provider",
		"unsupported by this channel",
		"not compatible with this provider",
		"does not support this model",
	}
	for _, marker := range compatibilityMarkers {
		if strings.Contains(text, marker) {
			return true
		}
	}
	return false
}

func buildFailureLog(c *gin.Context, bizErr *model.ErrorWithStatusCode, channelName string) *dbmodel.Log {
	respBody, _ := json.Marshal(bizErr.Error)
	requestBody, _ := common.GetRequestBody(c)
	return &dbmodel.Log{
		UserId: c.GetInt(ctxkey.Id),
		// 失败日志的渠道 id 与 channelName 同口径：sticky 覆盖后按实际服务渠道 B 归因，
		// 否则同一失败会被记到「选路渠道 A 的 id + 实际渠道 B 的名字」两个渠道上。
		ChannelId:     resolveActualChannelId(c, c.GetInt(ctxkey.ChannelId)),
		Quota:         0,
		Content:       fmt.Sprintf("HTTP status: %d, error: %s", bizErr.StatusCode, bizErr.Error.Message),
		ChannelName:   channelName,
		TokenName:     c.GetString(ctxkey.TokenName),
		ModelName:     c.GetString(ctxkey.RequestModel),
		ResponseBody:  string(respBody),
		RequestBody:   string(requestBody),
		RequestHeader: controller.MaskAuthorizationHeader(c.Request.Header),
	}
}

func recordFailureLog(c *gin.Context, bizErr *model.ErrorWithStatusCode, channelName string) {
	log := buildFailureLog(c, bizErr, channelName)
	dbmodel.RecordConsumeLog(c.Request.Context(), log)
}

func processChannelRelayError(ctx context.Context, userId int, channelId int, channelName string, failedModel string, err model.ErrorWithStatusCode) {
	logger.Log.Errorf("relay error (channel id %d, user id: %d, model: %s): %s", channelId, userId, failedModel, err.Message)
	// https://platform.openai.com/docs/guides/error-codes/api-errors
	if monitor.ShouldDisableChannel(&err.Error, err.StatusCode) {
		logger.Log.Infof("processChannelRelayError: disabling channel #%d (%s) reason=%q statusCode=%d", channelId, channelName, err.Message, err.StatusCode)
		monitor.DisableChannel(channelId, channelName, err.Message)
	} else {
		weight := cooldownErrorWeight(&err)
		logger.Log.Infof("processChannelRelayError: reporting failure channel #%d (%s) model=%s reason=%q statusCode=%d weight=%d",
			channelId, channelName, failedModel, err.Message, err.StatusCode, weight)
		monitor.Emit(channelId, false)
		// 权重 0（客户端参数类错误）只 Emit 不计数不冷却
		middleware.CooldownGlobal.ReportFailure(channelId, failedModel, weight)
	}
}

// cooldownErrorWeight 根据错误语义为单次失败赋予累计权重：
// - LooksLikeRequestShapeFailure 命中 → 0（客户端参数类，不计数不冷却）
// - 401/403 → 2（确定型失败，较快触发冷却）
// - 其余（5xx、超时、连接失败、适配器解析失败等）→ 1
func cooldownErrorWeight(err *model.ErrorWithStatusCode) int {
	if err == nil {
		return 0
	}
	if err.LooksLikeRequestShapeFailure() {
		return 0
	}
	if err.StatusCode == http.StatusUnauthorized || err.StatusCode == http.StatusForbidden {
		return 2
	}
	return 1
}

func RelayNotImplemented(c *gin.Context) {
	err := model.Error{
		Message: "API not implemented",
		Type:    "myapi_error",
		Param:   "",
		Code:    "api_not_implemented",
	}
	c.JSON(http.StatusNotImplemented, gin.H{
		"error": err,
	})
}

func RelayNotFound(c *gin.Context) {
	err := model.Error{
		Message: fmt.Sprintf("Invalid URL (%s %s)", c.Request.Method, c.Request.URL.Path),
		Type:    "invalid_request_error",
		Param:   "",
		Code:    "",
	}
	c.JSON(http.StatusNotFound, gin.H{
		"error": err,
	})
}

// detectStreamFromBody 从请求体中解析 stream 字段，判断是否为流式请求
func detectStreamFromBody(c *gin.Context) bool {
	rawBody, err := common.GetRequestBody(c)
	if err != nil || len(rawBody) == 0 {
		return false
	}
	var bodyMap map[string]any
	if err := json.Unmarshal(rawBody, &bodyMap); err != nil {
		return false
	}
	if stream, ok := bodyMap["stream"]; ok {
		if b, ok := stream.(bool); ok {
			return b
		}
	}
	return false
}

// buildActiveRequest 构造活跃请求对象，非流式请求返回 nil
func buildActiveRequest(c *gin.Context, m *metaPkg.Meta, requestId string) *active.ActiveRequest {
	if !detectStreamFromBody(c) {
		return nil
	}
	req := &active.ActiveRequest{
		RequestID:   requestId,
		UserID:      m.UserId,
		TokenName:   m.TokenName,
		ModelName:   m.OriginModelName,
		ChannelID:   m.ChannelId,
		ChannelName: m.ChannelName,
		Group:       m.Group,
		IsStream:    true,
		StartedAt:   time.Now().UnixMilli(),
		RelayMode:   m.Mode,
	}
	rawBody, _ := common.GetRequestBody(c)
	if len(rawBody) > 0 {
		bodyStr := string(rawBody)
		if len(bodyStr) <= config.MaxLoggedBodySize {
			req.RequestBody = bodyStr
		} else {
			req.RequestBody = fmt.Sprintf("[body too large: %d bytes]", len(rawBody))
		}
		req.HasRequestBody = true
	}
	// MaskAuthorizationHeader 内部已 JSON 序列化，直接使用返回值，避免双重编码
	headerStr := controller.MaskAuthorizationHeader(c.Request.Header)
	if headerStr != "{}" {
		req.RequestHeader = headerStr
		req.HasRequestHeader = true
	}
	req.Username = c.GetString(ctxkey.Username)
	if req.Username == "" {
		// TokenAuth 中间件不设置 username（仅 WebAuth 设置），手动查
		if user, err := dbmodel.GetUserById(req.UserID, false); err == nil {
			req.Username = user.Username
		}
	}
	return req
}
