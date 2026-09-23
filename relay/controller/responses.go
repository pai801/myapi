package controller

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/pai801/myapi/common"
	"github.com/pai801/myapi/common/config"
	"github.com/pai801/myapi/common/ctxkey"
	"github.com/pai801/myapi/common/helper"
	"github.com/pai801/myapi/common/logger"
	dbmodel "github.com/pai801/myapi/model"
	relay2 "github.com/pai801/myapi/relay"
	"github.com/pai801/myapi/relay/active"
	"github.com/pai801/myapi/relay/adaptor"
	"github.com/pai801/myapi/relay/adaptor/codex"
	"github.com/pai801/myapi/relay/adaptor/openai"
	"github.com/pai801/myapi/relay/apitype"
	billingratio "github.com/pai801/myapi/relay/billing/ratio"
	"github.com/pai801/myapi/relay/channeltype"
	"github.com/pai801/myapi/relay/constant"
	metaPkg "github.com/pai801/myapi/relay/meta"
	"github.com/pai801/myapi/relay/model"
	"github.com/pai801/myapi/relay/relaymode"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

func RelayResponsesHelper(c *gin.Context) *model.ErrorWithStatusCode {
	ctxMeta := metaPkg.GetByContext(c)
	// 包装 ResponseWriter 记录流式首字耗时（TTFT）
	wrapTTFTWriter(c, ctxMeta)

	// 对于 /v1/responses/compact 接口，只允许 Codex 渠道，否则返回错误
	if ctxMeta.Mode == relaymode.ResponsesCompact {
		if ctxMeta.APIType != apitype.Codex {
			return &model.ErrorWithStatusCode{
				Error: model.Error{
					Message: "unsupported endpoint \"/v1/responses/compact\", only Codex channels are supported",
					Type:    "invalid_request_error",
					Code:    "invalid_request",
				},
				StatusCode: http.StatusBadRequest,
			}
		}
		// Codex 渠道直接转发
		return relayResponsesDirect(c, ctxMeta)
	}

	// 普通 /v1/responses 接口的原有处理逻辑
	// DeepSeek 官方已原生支持 Responses 协议（V4-Flash 起），直接透传避免转换层损失（如 effort=max 被误映射为 auto 导致 400）
	if ctxMeta.APIType == apitype.Codex || ctxMeta.APIType == apitype.ChatGPTSub || ctxMeta.ChannelType == channeltype.DeepSeek {
		return relayResponsesDirect(c, ctxMeta)
	}

	return relayResponsesConverted(c, ctxMeta)
}

func relayResponsesDirect(c *gin.Context, ctxMeta *metaPkg.Meta) *model.ErrorWithStatusCode {
	ctx := c.Request.Context()
	relayAdaptor := relay2.GetAdaptor(ctxMeta.APIType)
	if relayAdaptor == nil {
		logger.Log.Errorf("[%s] %+v", "invalid api type", nil)
		return openai.ErrorWrapper(nil, "invalid api type", http.StatusBadRequest)
	}
	relayAdaptor.Init(ctxMeta)

	requestBody, err := common.GetRequestBody(c)
	if err != nil {
		logger.Log.Errorf("[%s] %+v", "get request body failed", err)
		return openai.ErrorWrapper(err, "get request body failed", http.StatusInternalServerError)
	}

	// 请求体惰性视图：只按需读取 model / stream，不再物化整份 map。
	// 良构性结论优先采信 middleware 阶段写入的共享元数据缓存（TokenAuth 已扫过同一份 body）；
	// 缓存缺失时才判定一次，且使用与 middleware 相同的 json.Valid（嵌套深度上限 10000），
	// 保证「缓存命中」与「回退」两条路径对同一份 bytes 的良构结论完全一致——缓存只是优化、
	// 不是正确性依赖，但两路不得分叉（缺陷 3）。
	requestJSON := gjson.ParseBytes(requestBody)
	// 单次读取共享缓存（优化项 4）：良构性闸门与下方 model/stream 派生共用同一次读取结论。
	metadata, hasMetadata := ctxkey.GetRequestBodyMetadata(c)
	wellFormed := false
	if hasMetadata {
		// ok=true 表示缓存结论权威（包括缓存的 WellFormed=false），不得再用其它校验器复判。
		wellFormed = metadata.WellFormed
	} else {
		// 缓存缺失：仅判定一次，供字段提取与两个估算器共享。
		wellFormed = json.Valid(requestBody)
	}
	// 语义级良构闸门（缺陷 4）：json.Valid 只做语法校验，而旧实现 `json.Unmarshal(body, &map[string]any)`
	// 还会因「超出 float64 表示范围的数值字面量」（如 1e400）整体失败。缓存的 WellFormed 同样源自
	// json.Valid，故缓存命中与缓存缺失两路都必须在同一惰性根上补一次超范围数值检测，否则同一份 bytes
	// 会得出分叉结论（缓存 WellFormed=true → 派生 model、max_output_tokens 被 clamp 到 1e6 → 403）。
	wellFormed = responsesSemanticWellFormed(requestJSON, wellFormed)
	// 复刻旧实现 `json.Unmarshal(body, &map[string]interface{})` 的根语义：
	//   - 对象根 → 成功，按键提取；
	//   - null 根 → 成功但 map 保持 nil（不派生任何字段），旧实现静默继续，不得产生 warn；
	//   - 数组/字符串/数字/布尔根 → 反序列化失败（仅 warn，软失败继续）；
	//   - 畸形 JSON / 超深嵌套（json.Valid=false）→ 反序列化失败（仅 warn，软失败继续）。
	// 仅对象根派生 model / stream，故这里区分「对象 / null / 非对象 / 畸形」四类。
	rootKind := responsesRootKindOf(requestJSON, wellFormed)
	if rootKind == responsesRootMalformed || rootKind == responsesRootNonObject {
		// 软失败语义：畸形或非对象根仅 warn 并继续，绝不从半截 JSON 派生 model / stream。
		logger.Log.Warnf("[responses] failed to parse request body: %v", "body is not a well-formed JSON object")
	}

	if rootKind == responsesRootObject {
		var bodyModel string
		var bodyModelValid bool
		var bodyStream bool
		var bodyStreamValid bool
		if hasMetadata {
			// 缓存命中：直接采信 middleware 的结论（Model 大小写不敏感、Stream 精确匹配）
			bodyModel, bodyModelValid = metadata.Model, metadata.ModelValid
			bodyStream, bodyStreamValid = metadata.Stream, metadata.StreamValid
		} else {
			// 回退：语义与缓存完全对齐——model 大小写不敏感（Unicode 简单折叠）、stream 精确键。
			// model 提取点的非法 UTF-8 库间差异见 extractModelCaseInsensitive 的注释。
			bodyModel, bodyModelValid = extractModelCaseInsensitive(requestBody)
			// 重复键取值差异（契约 AC-9）：gjson.Get 取首个 `stream` 键（first-wins），
			// encoding/json 取最后一个（last-wins）。本回退路径与 middleware 共享缓存一致，
			// 均为 first-wins，故缓存命中/缺失两路结论不分叉。
			// 由 TestRelayResponsesDirectNormalRequestMetaEquivalence 的 "stream 重复键 first-wins"
			// 用例锁定。
			if streamValue := requestJSON.Get("stream"); streamValue.Type == gjson.True || streamValue.Type == gjson.False {
				bodyStream, bodyStreamValid = streamValue.Bool(), true
			} else if streamValue.Exists() && streamValue.Type != gjson.Null {
				bodyStreamValid = false
			} else {
				bodyStreamValid = true
			}
		}
		if bodyModelValid {
			ctxMeta.OriginModelName = bodyModel
		}
		if ctxMeta.ActualModelName == "" && bodyModelValid {
			if mapped, ok := getMappedModelName(bodyModel, ctxMeta.ModelMapping); ok {
				ctxMeta.ActualModelName = mapped
			} else {
				ctxMeta.ActualModelName = bodyModel
			}
		}
		if bodyStreamValid {
			ctxMeta.IsStream = bodyStream
		}
	}

	// 替换请求体中的 model 字段为映射后的实际模型名（单字段原地替换，不重序列化整份 body）。
	// 仅在 body 为词法良构的 JSON 对象时改写：sjson.SetBytes 对畸形 JSON 不报错，而是静默产出污染/非法
	// 内容（如 `{"model":"x` → `,"model":"m"}`），若不守卫会把损坏的 body 转发上游。
	// 注意此处刻意使用词法良构性（gjson.Valid + 对象根）而非 json.Valid：sjson / rebuildModelField
	// 直接操作原始字节，可安全处理超深嵌套（json.Valid 因 10000 层上限判 false）等 encoding/json
	// 无法反序列化的输入，与任务 3.4「非 model 字段保留原始字节表示」一致（契约
	// `Sanctioned Model-Rewrite Guard Difference` 登记的 sanctioned 例外）。
	// 守卫对**完整 body**（而非惰性根的 Raw）判定：gjson.Parse 会跳过前导非空白字节（如 `\x00{...}`），
	// 若只看 requestJSON.Raw 会把前缀垃圾一并放行、被 sjson 原样保留而转发上游（优化项 3）。
	// gjson.ValidBytes 对完整 body 仍无深度上限，故 3.4 的超深嵌套用例不受影响。
	// 非良构时保留原 body：畸形 JSON 记 warn（与改造前「失败时使用原 body」语义一致），
	// null 等良构非对象根无 model 字段可改写，静默跳过以免日志噪声。
	lexicalObject := isValidResponsesRoot(requestJSON, requestBody)
	upstreamBody := requestBody
	if ctxMeta.OriginModelName != ctxMeta.ActualModelName && ctxMeta.ActualModelName != "" {
		if !lexicalObject {
			if rootKind == responsesRootMalformed {
				logger.Log.Warnf("[responsesDirect] skip model rewrite: request body is not a well-formed JSON object")
			}
		} else if countModelKeys(requestBody) > 1 {
			// 罕见路径：model 键重复（含大小写变体/转义等价键）。sjson 只替换首个匹配键，
			// 上游按文档序 last-wins 会取到未被映射的后续键，导致渠道模型映射被静默绕过。
			// 改用不会失败的键重建：剔除全部 model 变体键后追加单个规范 model 键，产出唯一
			// model 键的良构 body。非 model 字段保留原始字节表示（含 1e400、超深嵌套等
			// encoding/json 无法反序列化的值），因此不依赖 json.Unmarshal 的成功。
			// 与 sjson 快速路径使用同一编码器，模型名转义逐字节一致。
			if rebuilt, err := rebuildModelField(requestBody, ctxMeta.ActualModelName); err == nil {
				upstreamBody = rebuilt
				logger.Log.Debugf("[responsesDirect] model mapped (duplicate keys normalized): %s -> %s", ctxMeta.OriginModelName, ctxMeta.ActualModelName)
			} else {
				logger.Log.Warnf("[responsesDirect] failed to rebuild request body model field: %v", err)
			}
		} else if modifiedBody, err := sjson.SetBytes(requestBody, "model", ctxMeta.ActualModelName); err == nil {
			upstreamBody = modifiedBody
			logger.Log.Debugf("[responsesDirect] model mapped: %s -> %s", ctxMeta.OriginModelName, ctxMeta.ActualModelName)
		} else {
			logger.Log.Warnf("[responsesDirect] failed to rewrite request body model field: %v", err)
		}
	}

	// 存储请求体和请求头到 context 中
	if len(upstreamBody) <= config.MaxLoggedBodySize {
		ctx = context.WithValue(ctx, CtxKeyRequestBody, string(upstreamBody))
	} else {
		ctx = context.WithValue(ctx, CtxKeyRequestBody, fmt.Sprintf("[body too large: %d bytes]", len(upstreamBody)))
	}
	ctx = context.WithValue(ctx, CtxKeyRequestHeader, MaskAuthorizationHeader(c.Request.Header))

	// 获取模型比率和分组比率
	modelRatio := billingratio.GetModelRatio(ctxMeta.ActualModelName, ctxMeta.ChannelType)
	groupRatio := dbmodel.GetGroupModelRatio(ctxMeta.Group)
	ratio := modelRatio * groupRatio

	userQuota, err := dbmodel.CacheGetUserQuota(ctx, ctxMeta.UserId)
	if err != nil {
		return openai.ErrorWrapper(err, "get_user_quota_failed", http.StatusInternalServerError)
	}
	// 估算预扣额度：复用上方惰性解析的请求体视图，避免重复解析。
	// 估算器信任上方良构性闸门，仅做纯字段读取与算术，不再重复全量校验（缺陷 2）。
	// 仅良构对象根参与估算，复刻旧实现 `req == nil`（畸形 / null / 非对象根反序列化后 map 为 nil）
	// 时两个估算器均返回 0 的语义。
	promptEstimate, maxOutputEstimate := responsesPreConsumeEstimates(requestJSON, rootKind)
	estimatedQuota := int64(float64(500+promptEstimate) * ratio)
	// 预扣估算需纳入输出上限 max_output_tokens，否则低余额用户可用大输出参数把余额打成大负数
	if maxOutputEstimate > 0 {
		estimatedQuota += int64(float64(maxOutputEstimate) * ratio)
	}
	if userQuota < estimatedQuota {
		return openai.ErrorWrapper(errors.New("user quota is not enough"), "insufficient_user_quota", http.StatusForbidden)
	}

	// Pre-consume to close race window between check and actual consumption
	if err := dbmodel.DecreaseUserQuota(ctxMeta.UserId, estimatedQuota); err != nil {
		logger.Log.Errorf("pre-consume quota failed for user %d: %v", ctxMeta.UserId, err)
		return openai.ErrorWrapper(err, "pre_consume_quota_failed", http.StatusInternalServerError)
	} else {
		ctx = context.WithValue(ctx, CtxKeyPreConsumedQuota, estimatedQuota)
	}

	resp, err := relayAdaptor.DoRequest(c, ctxMeta, bytes.NewBuffer(upstreamBody))
	// sticky 在 DoRequest 内部改写 ctxMeta.ChannelId；返回后记录实际服务渠道供归因使用
	recordActualChannel(c, ctxMeta)
	if err != nil {
		rollbackResponsesPreConsumedQuota(ctx, ctxMeta.UserId)
		logger.Log.Errorf("[%s] %+v", "do request failed", err)
		return openai.ErrorWrapper(err, "do request failed", http.StatusInternalServerError)
	}

	if isErrorResp(resp) {
		rollbackResponsesPreConsumedQuota(ctx, ctxMeta.UserId)
		return relayErrorHandler(resp)
	}

	usage, relayErr := handleResponsesDirect(c, resp, ctxMeta, relayAdaptor)
	if respBody := c.GetString(ctxkey.ResponseBody); respBody != "" {
		ctx = context.WithValue(ctx, CtxKeyResponseBody, respBody)
	}
	if ttft := c.GetInt64(ctxkey.FirstTokenTime); ttft > 0 {
		ctx = context.WithValue(ctx, CtxKeyFirstTokenTime, ttft)
	}
	if relayErr != nil {
		rollbackResponsesPreConsumedQuota(ctx, ctxMeta.UserId)
		logger.Log.Errorf("DoResponse failed: %+v", relayErr)
		return relayErr
	}

	// 后消费逻辑 - 在 goroutine 外提取需要从 ctx 读取的值
	reqBody := ""
	respBody := ""
	reqHeader := ""
	if v := ctx.Value(CtxKeyRequestBody); v != nil {
		reqBody = v.(string)
	}
	if v := ctx.Value(CtxKeyResponseBody); v != nil {
		respBody = v.(string)
	}
	if v := ctx.Value(CtxKeyRequestHeader); v != nil {
		reqHeader = v.(string)
	}
	go postConsumeQuotaForResponses(ctx, usage, ctxMeta, ratio, modelRatio, reqBody, respBody, reqHeader)

	return nil
}

func relayResponsesConverted(c *gin.Context, ctxMeta *metaPkg.Meta) *model.ErrorWithStatusCode {
	ctx := c.Request.Context()
	relayAdaptor := relay2.GetAdaptor(ctxMeta.APIType)
	if relayAdaptor == nil {
		logger.Log.Errorf("[%s] %+v", "failed to get openai adaptor", nil)
		return openai.ErrorWrapper(nil, "failed to get openai adaptor", http.StatusInternalServerError)
	}
	relayAdaptor.Init(ctxMeta)

	requestBody, err := common.GetRequestBody(c)
	if err != nil {
		logger.Log.Errorf("[%s] %+v", "get request body failed", err)
		return openai.ErrorWrapper(err, "get request body failed", http.StatusInternalServerError)
	}

	// 请求体惰性视图：先建立 well-formedness，再按需读取 model / stream，不物化整份 map。
	// 硬失败语义：非法 JSON 必须在任何字段被接受、任何转换或上游工作开始之前返回 400。
	// 良构性结论优先采信 middleware 共享缓存；缓存缺失时用与 middleware 相同的 json.Valid
	// （嵌套深度上限 10000）只判定一次，保证缓存命中与回退两路结论一致（缺陷 3）。
	requestJSON := gjson.ParseBytes(requestBody)
	// 单次读取共享缓存：良构性闸门与下方 model/stream 派生共用同一次读取结论。
	metadata, hasMetadata := ctxkey.GetRequestBodyMetadata(c)
	wellFormed := false
	if hasMetadata {
		wellFormed = metadata.WellFormed
	} else {
		wellFormed = json.Valid(requestBody)
	}
	// 语义级良构闸门（缺陷 4）：复刻旧实现 `json.Unmarshal(body, &map[string]any)` 的失败语义——
	// json.Valid 为 true 但存在超出 float64 表示范围的数值字面量（如 1e400）时，整体视为解析失败，
	// 返回 CodeInvalidSourceJSON 且不派生 model/stream。缓存 WellFormed 源自 json.Valid，故缓存命中
	// 与缓存缺失两路都需在同一惰性根上补此检测，避免同一份 bytes 结论分叉。
	wellFormed = responsesSemanticWellFormed(requestJSON, wellFormed)
	// 复刻旧实现 `json.Unmarshal(body, &map[string]interface{})` 的根语义（缺陷 1）：
	//   - 对象根 → 成功，继续转换；
	//   - null 根 → 反序列化成功（map 保持 nil），继续交给转换层产出既有
	//     invalid_request_error / invalid_source_shape（不得在此返回 invalid_source_json）；
	//   - 数组/字符串/数字/布尔根、畸形 JSON、超深嵌套 → 反序列化失败 → CodeInvalidSourceJSON。
	rootKind := responsesRootKindOf(requestJSON, wellFormed)
	if rootKind != responsesRootObject && rootKind != responsesRootNull {
		logger.Log.Errorf("[%s] %+v", "invalid request body", "body is not a well-formed JSON object")
		return &model.ErrorWithStatusCode{
			Error:      model.Error{Message: "invalid request body: body is not a well-formed JSON object", Type: "invalid_request_error", Code: model.CodeInvalidSourceJSON},
			StatusCode: http.StatusBadRequest,
		}
	}

	modelName := ctxMeta.ActualModelName
	if modelName == "" {
		// 优先复用 middleware 写入的共享元数据缓存；缺失时回退惰性根提取。
		// Model 语义与缓存一致（大小写不敏感，来自 getRequestModel 的 encoding/json struct 解码）。
		// 回退提取点的非法 UTF-8 库间差异见 extractModelCaseInsensitive 的注释。
		if hasMetadata {
			if metadata.ModelValid {
				modelName = metadata.Model
			}
		} else if fallbackModel, fallbackValid := extractModelCaseInsensitive(requestBody); fallbackValid {
			modelName = fallbackModel
		}
	}
	modelName, _ = getMappedModelName(modelName, ctxMeta.ModelMapping)

	// stream 精确键匹配（与所有 stream 消费方现状一致）。
	// 重复键取值差异（契约 AC-9）：gjson.Get 取首个 `stream` 键（first-wins），encoding/json
	// 取最后一个（last-wins）；本回退路径与 middleware 共享缓存一致，均为 first-wins。
	// 由 TestRelayResponsesConvertedStreamDuplicateKeysFirstWins 锁定。
	stream := false
	if hasMetadata {
		if metadata.StreamValid {
			stream = metadata.Stream
		}
	} else if streamValue := requestJSON.Get("stream"); streamValue.Type == gjson.True || streamValue.Type == gjson.False {
		stream = streamValue.Bool()
	}
	ctxMeta.IsStream = stream

	// 决定是否对仅 reasoning 无 content 的响应兜底生成 message 事件
	fallbackReasoning := false
	if strings.Contains(strings.ToLower(modelName), "deepseek") {
		fallbackReasoning = true
	}

	// 请求转换错误（invalid_source_json / unsupported_mapping）必须在创建 chatRequestReader、
	// 查询/减少额度与 DoRequest 之前消费（Error Propagation Contract），原 Responses body 不得发往上游。
	chatRequest, convErr := codex.ConvertResponsesToChatRequest(modelName, requestBody, stream)
	if convErr != nil {
		logger.Log.Errorf("[responses-converted] request conversion failed: %+v", convErr)
		return responsesConversionClientError(convErr)
	}

	chatRequestReader := bytes.NewBuffer(chatRequest)

	chatMeta := &metaPkg.Meta{
		Mode:               relaymode.ChatCompletions,
		ChannelType:        ctxMeta.ChannelType,
		ChannelId:          ctxMeta.ChannelId,
		TokenId:            ctxMeta.TokenId,
		TokenName:          ctxMeta.TokenName,
		UserId:             ctxMeta.UserId,
		Group:              ctxMeta.Group,
		ModelMapping:       ctxMeta.ModelMapping,
		OriginModelName:    modelName,
		ActualModelName:    modelName,
		BaseURL:            ctxMeta.BaseURL,
		APIKey:             ctxMeta.APIKey,
		APIType:            apitype.OpenAI,
		Config:             ctxMeta.Config,
		IsStream:           stream,
		RequestURLPath:     "/v1/chat/completions",
		ForcedSystemPrompt: ctxMeta.ForcedSystemPrompt,
		StartTime:          ctxMeta.StartTime,
		ChannelName:        ctxMeta.ChannelName,
	}

	// 存储请求体和请求头到 context 中
	if len(chatRequest) <= config.MaxLoggedBodySize {
		ctx = context.WithValue(ctx, CtxKeyRequestBody, string(chatRequest))
	} else {
		ctx = context.WithValue(ctx, CtxKeyRequestBody, fmt.Sprintf("[body too large: %d bytes]", len(chatRequest)))
	}
	ctx = context.WithValue(ctx, CtxKeyRequestHeader, MaskAuthorizationHeader(c.Request.Header))

	// 获取模型比率和分组比率
	modelRatio := billingratio.GetModelRatio(modelName, ctxMeta.ChannelType)
	groupRatio := dbmodel.GetGroupModelRatio(ctxMeta.Group)
	ratio := modelRatio * groupRatio

	userQuota, err := dbmodel.CacheGetUserQuota(ctx, ctxMeta.UserId)
	if err != nil {
		return openai.ErrorWrapper(err, "get_user_quota_failed", http.StatusInternalServerError)
	}
	// 估算预扣额度：复用上方已建立的惰性请求体视图，避免重复解析。
	// 估算器信任上方良构性闸门，仅做纯字段读取与算术，不再重复全量校验（缺陷 2）。
	// 此处根必为良构对象（null / 非对象 / 畸形根已在转换前返回 400）。
	promptEstimate, maxOutputEstimate := responsesPreConsumeEstimates(requestJSON, rootKind)
	estimatedQuota := int64(float64(500+promptEstimate) * ratio)
	// 预扣估算需纳入输出上限 max_output_tokens，否则低余额用户可用大输出参数把余额打成大负数
	if maxOutputEstimate > 0 {
		estimatedQuota += int64(float64(maxOutputEstimate) * ratio)
	}
	if userQuota < estimatedQuota {
		return openai.ErrorWrapper(errors.New("user quota is not enough"), "insufficient_user_quota", http.StatusForbidden)
	}

	// Pre-consume to close race window between check and actual consumption
	if err := dbmodel.DecreaseUserQuota(ctxMeta.UserId, estimatedQuota); err != nil {
		logger.Log.Errorf("pre-consume quota failed for user %d: %v", ctxMeta.UserId, err)
		return openai.ErrorWrapper(err, "pre_consume_quota_failed", http.StatusInternalServerError)
	} else {
		ctx = context.WithValue(ctx, CtxKeyPreConsumedQuota, estimatedQuota)
	}

	relayAdaptor.Init(chatMeta)

	resp, err := relayAdaptor.DoRequest(c, chatMeta, chatRequestReader)
	// sticky 在 DoRequest 内部改写 chatMeta.ChannelId；返回后记录实际服务渠道供归因使用
	recordActualChannel(c, chatMeta)
	if err != nil {
		rollbackResponsesPreConsumedQuota(ctx, ctxMeta.UserId)
		logger.Log.Errorf("[%s] %+v", "do request failed", err)
		return openai.ErrorWrapper(err, "do request failed", http.StatusInternalServerError)
	}

	if isErrorResp(resp) {
		rollbackResponsesPreConsumedQuota(ctx, ctxMeta.UserId)
		return relayErrorHandler(resp)
	}

	finalUsage := &model.Usage{}

	if stream {
		// 流式响应处理
		common.SetEventStreamHeaders(c)
		c.Writer.WriteHeader(http.StatusOK)
		var converterState any
		streamResult, streamErr := forwardChatResponsesStream(c, resp.Body, requestBody, &converterState, fallbackReasoning)
		if streamErr != nil {
			logger.Log.Errorf("[responses-converted] stream terminated with error: %v", streamErr)
		}
		// 转换错误/失败终态（契约 §1.3）：SSE header 已提交，错误只能经事件终态表达——
		// forwarder 已保证写出唯一 failed/error 事件且不附加 JSON error body；
		// 此处消费状态、回滚预扣费并回传 502 失败错误：不得进入 post-consume，
		// 由 relay.go 依据 Written() 抑制重试与 JSON 渲染，同时保留渠道失败记账。
		if streamResult.FailureError != nil || streamResult.FailedTerminal || streamErr != nil {
			rollbackResponsesPreConsumedQuota(ctx, ctxMeta.UserId)
			if streamResult.FailureError != nil {
				logger.Log.Errorf("[%s] %+v", "scan response failed", streamResult.FailureError)
			}
			if streamResult.FailedTerminal {
				logger.Log.Warnf("responses stream failed after SSE headers committed")
			}
			if err := resp.Body.Close(); err != nil {
				logger.Log.Warnf("failed to close response body: %v", err)
			}
			return responsesStreamFailureError(streamResult, streamErr)
		}

		// 从流状态中提取 usage 和完整的响应体用于日志记录
		if converterState != nil {
			pt, ct, tt, cachedT := codex.GetStreamUsage(converterState)
			finalUsage.PromptTokens = pt
			finalUsage.CompletionTokens = ct
			finalUsage.TotalTokens = tt
			// 如果有缓存命中的token，设置到 PromptTokensDetails 中
			if cachedT > 0 {
				finalUsage.PromptTokensDetails = &model.PromptTokensDetails{
					CachedTokens: cachedT,
				}
			}

			if !streamResult.StreamErrored {
				completedBody := codex.GetStreamCompletedBody(converterState, requestBody)
				if completedBody != nil {
					ctx = context.WithValue(ctx, CtxKeyResponseBody, string(completedBody))
				}
			}
		}

		if err := resp.Body.Close(); err != nil {
			logger.Log.Warnf("failed to close response body: %v", err)
		}
	} else {
		// 非流式响应处理
		respBody, err := io.ReadAll(resp.Body)
		if err != nil {
			rollbackResponsesPreConsumedQuota(ctx, ctxMeta.UserId)
			logger.Log.Errorf("[%s] %+v", "read response body failed", err)
			return openai.ErrorWrapper(err, "read response body failed", http.StatusInternalServerError)
		}
		if err := resp.Body.Close(); err != nil {
			logger.Log.Warnf("failed to close response body: %v", err)
		}

		convertedBody, usage, relayErr := convertAndWriteResponsesNonStream(c, ctx, ctxMeta, respBody, modelName, fallbackReasoning, requestBody)
		if relayErr != nil {
			// 转换失败已在 helper 内回滚预扣费；502 时未写 200、未设置成功 body、未透传 chat.completion
			return relayErr
		}
		*finalUsage = *usage
		ctx = context.WithValue(ctx, CtxKeyResponseBody, string(convertedBody))
	}

	// 后消费逻辑 - 在 goroutine 外提取需要从 ctx 读取的值
	if ttft := c.GetInt64(ctxkey.FirstTokenTime); ttft > 0 {
		ctx = context.WithValue(ctx, CtxKeyFirstTokenTime, ttft)
	}
	reqBody := ""
	respBody := ""
	reqHeader := ""
	if v := ctx.Value(CtxKeyRequestBody); v != nil {
		reqBody = v.(string)
	}
	if v := ctx.Value(CtxKeyResponseBody); v != nil {
		respBody = v.(string)
	}
	if v := ctx.Value(CtxKeyRequestHeader); v != nil {
		reqHeader = v.(string)
	}
	go postConsumeQuotaForResponses(ctx, finalUsage, ctxMeta, ratio, modelRatio, reqBody, respBody, reqHeader)

	return nil
}

// responsesStreamFailureError 把已提交 SSE 后的流失败终态映射为 HTTP 502 upstream_error（契约 §1.3）。
// forwarder 已把错误经 failed/error 事件写回客户端，这里只回传渠道失败记账所需的错误对象；
// 复用 forwarder 已归类的 FailureError（含稳定机器码），仅在其缺失时按 streamErr/通用信息兜底。
func responsesStreamFailureError(result chatResponsesStreamResult, streamErr error) *model.ErrorWithStatusCode {
	errInfo := model.Error{}
	if result.FailureError != nil {
		errInfo = *result.FailureError
	}
	if errInfo.Message == "" {
		if streamErr != nil {
			errInfo.Message = streamErr.Error()
		} else {
			errInfo.Message = "responses stream failed after SSE headers committed"
		}
	}
	if errInfo.Type == "" {
		errInfo.Type = "upstream_error"
	}
	if errInfo.Code == nil || errInfo.Code == "" {
		errInfo.Code = "invalid_upstream_response"
	}
	return &model.ErrorWithStatusCode{Error: errInfo, StatusCode: http.StatusBadGateway}
}

// responsesConversionClientError 将请求侧协议转换错误映射为客户端错误（契约 §1.3 / T6）：
// HTTP 400 invalid_request_error，Code 保留稳定机器码（unsupported_mapping / invalid_source_json 等），
// 不再走仅 logger 的降级路径。
func responsesConversionClientError(err error) *model.ErrorWithStatusCode {
	code := model.CodeUnsupportedMapping
	var pce *model.ProtocolConversionError
	if errors.As(err, &pce) {
		code = pce.Code
	}
	return &model.ErrorWithStatusCode{
		Error:      model.Error{Message: err.Error(), Type: "invalid_request_error", Code: code},
		StatusCode: http.StatusBadRequest,
	}
}

// convertAndWriteResponsesNonStream 把上游 Chat 非流式响应转换为 Responses 协议并写回客户端。
// 转换失败：回滚预扣费并返回 HTTP 502 upstream_error/invalid_upstream_response ——
// 不得把 chat.completion 原样发给 Responses 客户端，也不得在转换前写 200/设置成功 body（报告一 P0-1）。
// 成功：返回转换后的 body 与从上游 usage 提取的内部 Usage（口径与既有实现一致）。
func convertAndWriteResponsesNonStream(c *gin.Context, ctx context.Context, ctxMeta *metaPkg.Meta, respBody []byte, modelName string, fallbackReasoning bool, requestBody []byte) ([]byte, *model.Usage, *model.ErrorWithStatusCode) {
	responsesResponse, convErr := codex.ConvertChatResponseToResponsesWithContext(respBody, modelName, fallbackReasoning, requestBody)
	if convErr != nil {
		rollbackResponsesPreConsumedQuota(ctx, ctxMeta.UserId)
		logger.Log.Errorf("[responses-converted] upstream chat response rejected by converter: %+v", convErr)
		return nil, nil, &model.ErrorWithStatusCode{
			Error:      model.Error{Message: convErr.Error(), Type: "upstream_error", Code: "invalid_upstream_response"},
			StatusCode: http.StatusBadGateway,
		}
	}

	c.JSON(http.StatusOK, json.RawMessage(responsesResponse))

	// 解析 usage
	finalUsage := &model.Usage{}
	var chatResponse map[string]interface{}
	if err := json.Unmarshal(respBody, &chatResponse); err == nil {
		if usage, ok := chatResponse["usage"].(map[string]interface{}); ok {
			if pt, ok := usage["prompt_tokens"].(float64); ok {
				finalUsage.PromptTokens = int(pt)
			}
			if ct, ok := usage["completion_tokens"].(float64); ok {
				finalUsage.CompletionTokens = int(ct)
			}
			if tt, ok := usage["total_tokens"].(float64); ok {
				finalUsage.TotalTokens = int(tt)
			}
			// 解析 prompt_tokens_details.cached_tokens
			if promptTokensDetails, ok := usage["prompt_tokens_details"].(map[string]interface{}); ok {
				if cachedTokens, ok := promptTokensDetails["cached_tokens"].(float64); ok && int(cachedTokens) > 0 {
					finalUsage.PromptTokensDetails = &model.PromptTokensDetails{
						CachedTokens: int(cachedTokens),
					}
				}
			}
		}
	}
	return responsesResponse, finalUsage, nil
}

// responsesUsage 解析 responses 协议的 usage 字段（DeepSeek 原生透传用）。
// responses 协议 token 字段为 input_tokens/output_tokens，与 chat 协议（prompt_tokens/completion_tokens）不同。
type responsesUsage struct {
	InputTokens         int                        `json:"input_tokens"`
	OutputTokens        int                        `json:"output_tokens"`
	TotalTokens         int                        `json:"total_tokens"`
	InputTokensDetails  *model.InputTokensDetails  `json:"input_tokens_details"`
	OutputTokensDetails *model.OutputTokensDetails `json:"output_tokens_details"`
}

// responsesStreamPayloadResult 报告累积器单次解码帧时观测到的元数据。
// Usage 仅在 Type == "response.completed" 且嵌套 usage 满足计费字段语义时非 nil。
type responsesStreamPayloadResult struct {
	Type  string
	Usage *responsesUsage
}

// strictResponsesInt 复刻旧 typed 解码对 Go int 字段的语义：缺失/null → 0 且合法；
// 其余必须是精确的十进制 int64（strconv.ParseInt 会拒绝小数、指数形式如 1e2、
// 以及 int64 溢出），否则整体 usage 不可用。与 C1 模块的 strictInt64Value 同构。
//
// 注意：ParseInt 会接受 `+5`/`007` 等非 JSON 合法整数形态，但本函数仅在
// json.Unmarshal 成功后被调用（见 addPayload），届时这些形态已被拒绝，故不可达。
//
// 平台假设：仅在 64 位平台（int == int64）下与旧 typed int 语义完全一致；32 位平台会截断。
// 当前构建目标仅 linux/amd64 与 linux/arm64（见 .github/workflows/docker-build.yml）。
func strictResponsesInt(result gjson.Result) (int, bool) {
	if !result.Exists() || result.Type == gjson.Null {
		return 0, true
	}
	if result.Type != gjson.Number {
		return 0, false
	}
	value, err := strconv.ParseInt(result.Raw, 10, 64)
	if err != nil {
		return 0, false
	}
	return int(value), true
}

// inputTokensDetailsFromResult 从 usage 对象提取 input_tokens_details，语义与旧 typed
// json.Unmarshal 一致：缺失/null → nil 指针；非对象（string/number/array/bool）→
// 旧 typed 整体失败，这里以 ok=false 上报由调用方整体置 nil；对象内 int 字段类型不符
// （含小数/指数）同样整体失败。
func inputTokensDetailsFromResult(result gjson.Result) (*model.InputTokensDetails, bool) {
	if !result.Exists() || result.Type == gjson.Null {
		return nil, true
	}
	if !result.IsObject() {
		return nil, false
	}
	details := &model.InputTokensDetails{}
	var ok bool
	if details.CachedTokens, ok = strictResponsesInt(result.Get("cached_tokens")); !ok {
		return nil, false
	}
	if details.CacheWriteTokens, ok = strictResponsesInt(result.Get("cache_write_tokens")); !ok {
		return nil, false
	}
	return details, true
}

// outputTokensDetailsFromResult 与 inputTokensDetailsFromResult 同构，对应 output_tokens_details。
func outputTokensDetailsFromResult(result gjson.Result) (*model.OutputTokensDetails, bool) {
	if !result.Exists() || result.Type == gjson.Null {
		return nil, true
	}
	if !result.IsObject() {
		return nil, false
	}
	details := &model.OutputTokensDetails{}
	var ok bool
	if details.ReasoningTokens, ok = strictResponsesInt(result.Get("reasoning_tokens")); !ok {
		return nil, false
	}
	if details.AcceptedPredictionTokens, ok = strictResponsesInt(result.Get("accepted_prediction_tokens")); !ok {
		return nil, false
	}
	if details.RejectedPredictionTokens, ok = strictResponsesInt(result.Get("rejected_prediction_tokens")); !ok {
		return nil, false
	}
	if details.AudioTokens, ok = strictResponsesInt(result.Get("audio_tokens")); !ok {
		return nil, false
	}
	if details.TextTokens, ok = strictResponsesInt(result.Get("text_tokens")); !ok {
		return nil, false
	}
	return details, true
}

// responsesUsageFromResult 从已完成帧的惰性根提取嵌套 usage，语义与旧实现
// json.Unmarshal 到 typed responsesUsage 一致：三个 basis 任一不可解析则整体返回 nil
// （旧 typed 解码整体失败 → usage 保持前值）。details 字段语义区分如下：
//   - details 缺失 / null → 保留 basis，该 details 指针为 nil；
//   - details 非对象 / 内部字段类型不符 → 整个 usage 不可用（返回 nil，对齐旧 typed 整体 decode 失败）。
//
// 重复键取值差异（契约 AC-9 声明的 sanctioned exception）：本函数经 gjson 按路径取值，
// 重复键取「第一个」（first-wins）；而旧实现经 encoding/json typed 解码取「最后一个」
// （last-wins）。例如 `{"response":{"usage":{"input_tokens":1,...},"usage":{"input_tokens":9,...}}}`
// 旧实现得 9、新实现得 1；`{"input_tokens":1,"input_tokens":9,...}` 旧实现得 9、新实现得 1。
// 注意：帧的 `type` 字段仍经 map 解码（last-wins），与旧 typed 行为一致，无此差异。
// 该差异由 TestResponsesStreamUsageDuplicateKeysFirstWins 锁定（同时覆盖契约 7.4 对
// Responses usage 重复键测试的要求，7.4 执行时交叉引用即可）。
func responsesUsageFromResult(responseResult gjson.Result) *responsesUsage {
	if !responseResult.Exists() || !responseResult.IsObject() {
		return nil
	}
	usageResult := responseResult.Get("usage")
	if !usageResult.Exists() || usageResult.Type == gjson.Null {
		return nil
	}
	if !usageResult.IsObject() {
		return nil
	}
	usage := &responsesUsage{}
	var ok bool
	if usage.InputTokens, ok = strictResponsesInt(usageResult.Get("input_tokens")); !ok {
		return nil
	}
	if usage.OutputTokens, ok = strictResponsesInt(usageResult.Get("output_tokens")); !ok {
		return nil
	}
	if usage.TotalTokens, ok = strictResponsesInt(usageResult.Get("total_tokens")); !ok {
		return nil
	}
	if usage.InputTokensDetails, ok = inputTokensDetailsFromResult(usageResult.Get("input_tokens_details")); !ok {
		return nil
	}
	if usage.OutputTokensDetails, ok = outputTokensDetailsFromResult(usageResult.Get("output_tokens_details")); !ok {
		return nil
	}
	return usage
}

// toModelUsage 把 responses usage 映射到网关内部 Usage（用于扣费与日志）。
// 计费只看总数与 cached_tokens；cache_write/reasoning 等 details 仅进入内部 detail 承载，不改 quota 公式。
func (u *responsesUsage) toModelUsage() *model.Usage {
	if u == nil {
		return nil
	}
	usage := &model.Usage{
		PromptTokens:     u.InputTokens,
		CompletionTokens: u.OutputTokens,
		TotalTokens:      u.TotalTokens,
	}
	if d := u.InputTokensDetails; d != nil && (d.CachedTokens != 0 || d.CacheWriteTokens != 0) {
		usage.PromptTokensDetails = &model.PromptTokensDetails{
			CachedTokens:     d.CachedTokens,
			CacheWriteTokens: d.CacheWriteTokens,
		}
	}
	if d := u.OutputTokensDetails; d != nil && (d.ReasoningTokens != 0 || d.AcceptedPredictionTokens != 0 || d.RejectedPredictionTokens != 0 || d.AudioTokens != 0 || d.TextTokens != 0) {
		usage.CompletionTokensDetails = &model.CompletionTokensDetails{
			ReasoningTokens:          d.ReasoningTokens,
			AcceptedPredictionTokens: d.AcceptedPredictionTokens,
			RejectedPredictionTokens: d.RejectedPredictionTokens,
			AudioTokens:              d.AudioTokens,
			TextTokens:               d.TextTokens,
		}
	}
	return usage
}

// responsesNonStreamExtraction captures safe observations from a direct non-stream Responses body
// without changing its passthrough status.
//
// UsageInvalid 区分 DEC-C2-1 与普通缺失：UsageInvalid == true 要求 Usage == nil 且调用方告警，
// 任何「部分解析」的 usage 都不得逃逸（旧实现忽略 json.Unmarshal 错误会按部分字段扣费）。
// 计费语义（spec: Billing-critical fields SHALL fail rather than degrade）：三个 basis
// （input_tokens/output_tokens/total_tokens）严格失败；details 宽容降级为缺失。`null` 映射为零值。
//
// 重复键差异（契约 AC-9 sanctioned exception）：本路径经 gjson 路径取值，字节完全相同的重复键
// 取「第一个」（first-wins）；旧 `encoding/json` typed 解码取「最后一个」（last-wins）。
// 键匹配为精确匹配（大小写敏感），与同包 responsesUsageFromResult 的既有口径一致。
// 由 TestResponsesNonStreamDuplicateKeysFirstWins 锁定。
type responsesNonStreamExtraction struct {
	WellFormed   bool
	Error        *model.Error
	Usage        *responsesUsage
	UsageInvalid bool
}

// extractResponsesDirectNonStreamPayload extracts error and usage without partial billing;
// malformed JSON produces no values, invalid billing fields set UsageInvalid with nil Usage,
// null integers map to zero, and invalid details are omitted.
func extractResponsesDirectNonStreamPayload(responseBody []byte) responsesNonStreamExtraction {
	if !json.Valid(responseBody) {
		return responsesNonStreamExtraction{}
	}
	result := responsesNonStreamExtraction{WellFormed: true}
	root := gjson.ParseBytes(responseBody)
	if !root.IsObject() {
		// 根为 null/数组/字符串/数字/布尔：旧 payload 匿名字段解码得到零值（null）或整份失败但被
		// 忽略（其余），两种情况下 Error 与 Usage 均为 nil，此处等价返回零值。
		return result
	}

	// 上游错误：仅在 `error` 为非 null 对象时非 nil；是否作为 relay 错误返回由消费方的
	// `Message != ""` 判据决定（与旧实现 `payload.Error != nil && payload.Error.Message != ""` 一致）。
	if errorResult := root.Get("error"); errorResult.IsObject() {
		result.Error = parseTolerantResponsesError(errorResult)
	}

	usageResult := root.Get("usage")
	if !usageResult.Exists() || usageResult.Type == gjson.Null {
		// usage 缺失 / null：旧 typed 解码为零值指针 nil → 不扣费。
		return result
	}
	if !usageResult.IsObject() {
		// usage 非对象：旧 typed 解码失败（被忽略）→ Usage 指针保持 nil。DEC-C2-1 下显式标记无效。
		result.UsageInvalid = true
		return result
	}
	parsed, ok := tolerantResponsesUsageFromResult(usageResult)
	if !ok {
		result.UsageInvalid = true
		return result
	}
	result.Usage = parsed
	return result
}

// parseTolerantResponsesError 宽容解析上游 error 对象：字段类型不符按零值处理（复刻
// encoding/json 对 struct 指针的部分填充语义，旧实现忽略该解码错误）。
func parseTolerantResponsesError(errorResult gjson.Result) *model.Error {
	parsed := &model.Error{}
	if value := errorResult.Get("message"); value.Type == gjson.String {
		parsed.Message = value.Str
	}
	if value := errorResult.Get("type"); value.Type == gjson.String {
		parsed.Type = value.Str
	}
	if value := errorResult.Get("param"); value.Type == gjson.String {
		parsed.Param = value.Str
	}
	if value := errorResult.Get("code"); value.Exists() && value.Type != gjson.Null {
		parsed.Code = value.Value()
	}
	return parsed
}

// tolerantResponsesUsageFromResult 严格解析三个 basis、宽容解析 details。
// 任一 basis 不可解析 → ok=false（DEC-C2-1：整体视为无效，不扣费）；details 不可解析 → 该 detail
// 视为缺失（指针 nil）或内部字段按零值处理，不影响 basis。
func tolerantResponsesUsageFromResult(usageResult gjson.Result) (*responsesUsage, bool) {
	usage := &responsesUsage{}
	var ok bool
	if usage.InputTokens, ok = strictResponsesInt(usageResult.Get("input_tokens")); !ok {
		return nil, false
	}
	if usage.OutputTokens, ok = strictResponsesInt(usageResult.Get("output_tokens")); !ok {
		return nil, false
	}
	if usage.TotalTokens, ok = strictResponsesInt(usageResult.Get("total_tokens")); !ok {
		return nil, false
	}
	usage.InputTokensDetails = tolerantResponsesInputDetails(usageResult.Get("input_tokens_details"))
	usage.OutputTokensDetails = tolerantResponsesOutputDetails(usageResult.Get("output_tokens_details"))
	return usage, true
}

// tolerantResponsesInputDetails：非对象/null/缺失 → nil；对象内任一 int 子字段不可解析 → 该字段零值。
func tolerantResponsesInputDetails(result gjson.Result) *model.InputTokensDetails {
	if !result.IsObject() {
		return nil
	}
	details := &model.InputTokensDetails{}
	if value, ok := strictResponsesInt(result.Get("cached_tokens")); ok {
		details.CachedTokens = value
	}
	if value, ok := strictResponsesInt(result.Get("cache_write_tokens")); ok {
		details.CacheWriteTokens = value
	}
	return details
}

// tolerantResponsesOutputDetails：与 tolerantResponsesInputDetails 同构，对应 output_tokens_details。
func tolerantResponsesOutputDetails(result gjson.Result) *model.OutputTokensDetails {
	if !result.IsObject() {
		return nil
	}
	details := &model.OutputTokensDetails{}
	if value, ok := strictResponsesInt(result.Get("reasoning_tokens")); ok {
		details.ReasoningTokens = value
	}
	if value, ok := strictResponsesInt(result.Get("accepted_prediction_tokens")); ok {
		details.AcceptedPredictionTokens = value
	}
	if value, ok := strictResponsesInt(result.Get("rejected_prediction_tokens")); ok {
		details.RejectedPredictionTokens = value
	}
	if value, ok := strictResponsesInt(result.Get("audio_tokens")); ok {
		details.AudioTokens = value
	}
	if value, ok := strictResponsesInt(result.Get("text_tokens")); ok {
		details.TextTokens = value
	}
	return details
}

// handleResponsesDirect 处理 responses 原生透传的响应。
// 上游为 DeepSeek 等原生支持 Responses 协议的 OpenAI 兼容渠道时使用：
// openai adaptor 的 DoResponse 只认 chat 格式（choices/usage.prompt_tokens），
// responses 格式（顶层 usage.input_tokens）会提取失真，流式下甚至不转发任何 SSE 数据，
// 因此这里直接原样透传响应，并单独按 responses 格式提取 usage。
func handleResponsesDirect(c *gin.Context, resp *http.Response, meta *metaPkg.Meta, relayAdaptor adaptor.Adaptor) (*model.Usage, *model.ErrorWithStatusCode) {
	if meta.ChannelType == channeltype.DeepSeek {
		if meta.IsStream {
			return handleResponsesDirectStream(c, resp)
		}
		return handleResponsesDirectNonStream(c, resp)
	}
	// Codex / ChatGPTSub 等渠道走各自适配器（已支持 responses 格式）
	return relayAdaptor.DoResponse(c, resp, meta)
}

func handleResponsesDirectNonStream(c *gin.Context, resp *http.Response) (*model.Usage, *model.ErrorWithStatusCode) {
	responseBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, openai.ErrorWrapper(err, "read_response_body_failed", http.StatusInternalServerError)
	}
	_ = resp.Body.Close()

	// 按需扫描上游 error/usage（契约 6.2）。畸形 JSON 不作为新的 relay error：旧实现同样忽略
	// `json.Unmarshal` 错误并继续透传（exact 透传 + 不派生 usage）。
	// 重复键差异（AC-9 sanctioned exception）：本路径经 gjson 取 first-wins，旧 typed 解码为 last-wins。
	extraction := extractResponsesDirectNonStreamPayload(responseBody)

	// 兜底：上游 200 但 body 为错误 JSON（如 effort 非法）时直接返回错误，不转发
	if extraction.Error != nil && extraction.Error.Message != "" {
		return nil, &model.ErrorWithStatusCode{Error: *extraction.Error, StatusCode: resp.StatusCode}
	}

	for k, v := range resp.Header {
		c.Writer.Header().Set(k, v[0])
	}
	c.Writer.WriteHeader(resp.StatusCode)
	_, _ = c.Writer.Write(responseBody)

	c.Set(ctxkey.ResponseBody, string(responseBody))

	// DEC-C2-1：主计费字段不可解析时整体视为 usage 缺失、不扣费。旧实现会按 `json.Unmarshal`
	// 的部分解析值扣费（欠费优于按部分数据扣费）；此处必须发出可观测 warning。
	if extraction.UsageInvalid {
		logger.Log.Warnf("[responsesDirect] usage discarded: billing basis fields are unparseable; no charge applied (DEC-C2-1)")
		return nil, nil
	}
	return extraction.Usage.toModelUsage(), nil
}

func handleResponsesDirectStream(c *gin.Context, resp *http.Response) (*model.Usage, *model.ErrorWithStatusCode) {
	common.SetEventStreamHeaders(c)
	c.Writer.WriteHeader(http.StatusOK)

	// 逐行原样转发 SSE 事件（response.created → ... → response.completed，无 [DONE]），
	// 同时从 response.completed 事件中提取 usage 用于扣费。
	// 响应体记录：SSE 流无整体 body，通过 responsesStreamAccumulator 把各事件合并成
	// 等价于非流式响应的完整 JSON（快照整体吸收、delta 增量拼接），存入 ctxkey.ResponseBody 供日志展示。
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, constant.ScannerBufferInitial), constant.ScannerBufferMax)
	var usage *model.Usage
	acc := newResponsesStreamAccumulator()
	for scanner.Scan() {
		line := scanner.Text()
		if _, err := c.Writer.WriteString(line + "\n"); err != nil {
			break
		}
		if strings.HasPrefix(line, "data: ") {
			payload := strings.TrimPrefix(line, "data: ")
			// 注意：此处有意忽略 addPayload 的 error。畸形/类型不符帧的派生值已为零值，
			// 且无论提取成功与否，帧都必须原样转发给客户端（转发行为与旧实现一致，不因 err 改变）。
			result, _ := acc.addPayload([]byte(payload))
			// usage 仅可来自 response.completed；除 AC-9 声明的重复键 first-wins 差异外，与旧 typed 解码等价。
			// 覆盖语义除 AC-9 声明的重复键 first-wins 差异外，与旧实现等价
			// `if err == nil && evt.Type == "response.completed" && evt.Response != nil`：
			//   - response 为非 null 对象且 usage 缺失/null → 覆盖为 nil（旧实现 toModelUsage 对 nil 返回 nil）；
			//   - response 为非 null 对象且 usage 合法 → 覆盖为映射值；
			//   - typed 解码会失败（response/usage 非对象、usage 字段类型不符）或 response 缺失 → 保持前值。
			// 实测：旧实现仅在 typed 解码成功且 Response != nil 时覆盖，故这里需区分「usage 缺失/null」
			// 与「usage 类型不符」，否则多 completed 帧流会出现 0/0/0 与保持前值的差异。
			if result.Type == "response.completed" {
				if result.Usage != nil {
					usage = result.Usage.toModelUsage()
				} else if responseResult := gjson.ParseBytes([]byte(payload)).Get("response"); responseResult.IsObject() {
					usageResult := responseResult.Get("usage")
					if !usageResult.Exists() || usageResult.Type == gjson.Null {
						usage = nil
					}
				}
			}
		}
	}
	c.Writer.Flush()
	_ = resp.Body.Close()

	if body := acc.buildResponseBody(); body != "" {
		c.Set(ctxkey.ResponseBody, body)
	}

	if usage == nil {
		usage = &model.Usage{}
	}
	return usage, nil
}

// responsesStreamAccumulator 把 responses 协议 SSE 事件合并为完整 response JSON（日志记录用）。
// 对齐 openai adaptor 的 chatStreamAccumulator 模式：快照类事件（response.created/output_item.done/
// response.completed）整体吸收字段，增量类事件（output_text.delta/function_call_arguments.delta）
// 拼接文本，最终 buildResponseBody 输出等价于非流式响应体的完整 JSON。
type responsesStreamAccumulator struct {
	resp    map[string]any // 顶层 response 对象（含 output 数组）
	output  []any          // 累积的 output items
	curItem map[string]any // 当前正在累积文本/参数的 output item
}

func newResponsesStreamAccumulator() *responsesStreamAccumulator {
	return &responsesStreamAccumulator{
		resp: map[string]any{
			"object": "response",
			"output": []any{},
		},
	}
}

// addPayload 校验并累积一个 Responses 帧，并从同一次观测中返回帧类型与 completed 响应的 usage。
// 畸形帧或类型不符的元数据返回 error 且不产出派生值；调用方有意忽略该 error（见调用点注释），
// 因为帧无论如何都需原样转发。
//
// 累积行为与改造前逐条保持一致（宽松 map 语义）；usage 提取走词法路径以复刻旧 typed int 语义
// （map[string]any 会把数字归一化为 float64，无法区分 1e2/100.0/100，见 4.5 的决策记录）。
//
// 权衡：本函数对同一帧做两次局部解析——map 解码用于累积（宽松语义），gjson 惰性根用于 usage
// 词法提取（typed 等价语义）。这是必要权衡：map 无法复刻 typed int 语义，而 gjson 无法复刻
// 累积所需的宽松 map 行为。相比改造前「map 累积 + 第二次 typed 全量解码」，仍消除了重复的
// 全量 typed 解码。
func (a *responsesStreamAccumulator) addPayload(payload []byte) (result responsesStreamPayloadResult, err error) {
	var evt map[string]any
	if unmarshalErr := json.Unmarshal(payload, &evt); unmarshalErr != nil {
		// 畸形帧：不累积、不产出派生值；返回 error（调用方有意忽略，见调用点注释），帧仍由调用方原样转发。
		return responsesStreamPayloadResult{}, unmarshalErr
	}
	evtType, typeIsString := evt["type"].(string)
	if !typeIsString && evt["type"] != nil {
		// type 存在但非字符串：类型不符的元数据，返回 error 且不产出派生值。
		return responsesStreamPayloadResult{}, fmt.Errorf("responses stream frame type must be a string")
	}
	result.Type = evtType
	// usage 仅可来自 response.completed；词法提取保证与旧 typed 解码等价（重复键 first-wins 差异见 responsesUsageFromResult）。
	if evtType == "response.completed" {
		result.Usage = responsesUsageFromResult(gjson.ParseBytes(payload).Get("response"))
	}
	switch evtType {
	case "response.created", "response.in_progress", "response.completed", "response.failed":
		if resp, ok := evt["response"].(map[string]any); ok {
			a.absorbResponse(resp)
		}
	case "response.output_item.added":
		if item, ok := evt["item"].(map[string]any); ok {
			a.absorbItem(item)
		}
	case "response.content_part.added":
		if part, ok := evt["part"].(map[string]any); ok {
			a.appendContentPart(part)
		}
	case "response.output_text.delta":
		if delta, ok := evt["delta"].(string); ok {
			a.appendOutputText(delta)
		}
	case "response.output_text.done":
		if text, ok := evt["text"].(string); ok {
			a.setOutputText(text)
		}
	case "response.function_call_arguments.delta":
		if delta, ok := evt["delta"].(string); ok {
			a.appendFunctionArgs(delta)
		}
	case "response.function_call_arguments.done":
		if args, ok := evt["arguments"].(string); ok {
			a.setFunctionArgs(args)
		}
	case "response.output_item.done":
		if item, ok := evt["item"].(map[string]any); ok {
			a.replaceItem(item)
		}
	}
	return result, nil
}

// absorbResponse 吸收 response 快照字段；快照带完整 output 时整体替换，否则保留已累积的 output。
func (a *responsesStreamAccumulator) absorbResponse(resp map[string]any) {
	if output, ok := resp["output"].([]any); ok && len(output) > 0 {
		a.output = output
		a.resp["output"] = output
	}
	for k, v := range resp {
		if k == "output" {
			continue
		}
		a.resp[k] = v
	}
}

// absorbItem 追加新的 output item（output_item.added）。
func (a *responsesStreamAccumulator) absorbItem(item map[string]any) {
	a.curItem = item
	a.output = append(a.output, item)
	a.resp["output"] = a.output
}

// appendContentPart 追加 content part；若当前 item 已有同类型占位 part 则跳过（避免重复）。
func (a *responsesStreamAccumulator) appendContentPart(part map[string]any) {
	if a.curItem == nil {
		return
	}
	content, _ := a.curItem["content"].([]any)
	partType, _ := part["type"].(string)
	for _, cv := range content {
		if cm, ok := cv.(map[string]any); ok {
			if t, _ := cm["type"].(string); t == partType && t == "output_text" {
				return
			}
		}
	}
	a.curItem["content"] = append(content, part)
}

// lastOutputTextPart 返回当前 item 的最后一个 output_text part（delta 文本的落点）。
func (a *responsesStreamAccumulator) lastOutputTextPart() map[string]any {
	if a.curItem == nil {
		return nil
	}
	content, _ := a.curItem["content"].([]any)
	for i := len(content) - 1; i >= 0; i-- {
		if cm, ok := content[i].(map[string]any); ok {
			if t, _ := cm["type"].(string); t == "output_text" {
				return cm
			}
		}
	}
	return nil
}

// appendOutputText 把 output_text.delta 拼接到当前文本后。
func (a *responsesStreamAccumulator) appendOutputText(delta string) {
	part := a.lastOutputTextPart()
	if part == nil {
		return
	}
	text, _ := part["text"].(string)
	part["text"] = text + delta
}

// setOutputText 用 output_text.done 的完整文本覆盖。
func (a *responsesStreamAccumulator) setOutputText(text string) {
	if part := a.lastOutputTextPart(); part != nil {
		part["text"] = text
	}
}

// appendFunctionArgs 把 function_call_arguments.delta 拼接到当前 item 的 arguments 后。
func (a *responsesStreamAccumulator) appendFunctionArgs(delta string) {
	if a.curItem == nil {
		return
	}
	args, _ := a.curItem["arguments"].(string)
	a.curItem["arguments"] = args + delta
}

// setFunctionArgs 用 function_call_arguments.done 的完整 arguments 覆盖。
func (a *responsesStreamAccumulator) setFunctionArgs(args string) {
	if a.curItem != nil {
		a.curItem["arguments"] = args
	}
}

// replaceItem 用 output_item.done 的完整 item 快照替换同 id 的累积 item（未匹配则追加）。
func (a *responsesStreamAccumulator) replaceItem(item map[string]any) {
	id, _ := item["id"].(string)
	for i, v := range a.output {
		m, ok := v.(map[string]any)
		if !ok {
			continue
		}
		if mid, _ := m["id"].(string); mid == id {
			a.output[i] = item
			if a.curItem != nil {
				if curID, _ := a.curItem["id"].(string); curID == id {
					a.curItem = item
				}
			}
			a.resp["output"] = a.output
			return
		}
	}
	a.absorbItem(item)
}

// buildResponseBody 序列化合并后的完整 response JSON。
func (a *responsesStreamAccumulator) buildResponseBody() string {
	body, err := json.Marshal(a.resp)
	if err != nil {
		logger.Log.Errorf("buildResponseBody marshal failed: " + err.Error())
		return ""
	}
	return string(body)
}

type chatResponsesStreamResult struct {
	StreamErrored   bool
	FailedTerminal  bool
	SuccessTerminal bool
	// IncompleteTerminal 标记终态为 response.incomplete（截断成功）：终态识别上并入 SuccessTerminal，
	// 该位仅用于与 response.completed 区分，消费方无需改动即按成功路径计费/提取
	IncompleteTerminal bool
	TerminalSeen       bool
	FailureError       *model.Error
}

type convertedEventMeta struct {
	EventName  string
	Failed     bool
	Completed  bool
	Incomplete bool
	StreamErr  *model.Error
}

func parseConvertedEventMeta(converted string) convertedEventMeta {
	meta := convertedEventMeta{}
	lines := strings.Split(converted, "\n")
	var payloadLines []string
	for _, line := range lines {
		if strings.HasPrefix(line, "event: ") {
			meta.EventName = strings.TrimSpace(strings.TrimPrefix(line, "event: "))
			continue
		}
		if strings.HasPrefix(line, "data: ") {
			payloadLines = append(payloadLines, strings.TrimPrefix(line, "data: "))
		}
	}
	payload := strings.Join(payloadLines, "\n")
	if meta.EventName == "response.completed" {
		meta.Completed = true
		return meta
	}
	// response.incomplete 是合法成功的终止状态（上游 finish_reason=length/content_filter
	// 被转换器映射为截断终态），非失败：按 completed 的终态语义接入，区分由事件 status 保留
	if meta.EventName == "response.incomplete" {
		meta.Incomplete = true
		return meta
	}
	if meta.EventName == "response.failed" {
		meta.Failed = true
		if payload != "" {
			var evt struct {
				Response *struct {
					Error *model.Error `json:"error"`
				} `json:"response"`
			}
			if err := json.Unmarshal([]byte(payload), &evt); err == nil && evt.Response != nil && evt.Response.Error != nil {
				meta.StreamErr = evt.Response.Error
			}
		}
		return meta
	}
	if meta.EventName == "error" {
		meta.Failed = true
		if payload != "" {
			var evt model.ResponseStreamErrorEvent
			if err := json.Unmarshal([]byte(payload), &evt); err == nil {
				e := model.Error{Message: evt.Message, Type: "upstream_error", Code: evt.Code}
				if e.Message == "" {
					e.Message = "upstream stream error"
				}
				if e.Code == nil || e.Code == "" {
					e.Code = "server_error"
				}
				meta.StreamErr = &e
			}
		}
	}
	return meta
}

func inspectConvertedResponsesEvents(c *gin.Context, convertedEvents []string) chatResponsesStreamResult {
	result := chatResponsesStreamResult{}
	for _, converted := range convertedEvents {
		_, _ = c.Writer.WriteString(converted)
		meta := parseConvertedEventMeta(converted)
		if meta.Completed {
			result.SuccessTerminal = true
			result.TerminalSeen = true
		}
		if meta.Incomplete {
			result.SuccessTerminal = true
			result.IncompleteTerminal = true
			result.TerminalSeen = true
		}
		if meta.Failed {
			result.StreamErrored = true
			result.FailedTerminal = true
			result.TerminalSeen = true
			if meta.StreamErr != nil {
				result.FailureError = meta.StreamErr
			}
		}
	}
	return result
}

func forwardChatResponsesStream(c *gin.Context, body io.Reader, requestBody []byte, converterState *any, fallbackReasoning bool) (chatResponsesStreamResult, error) {
	reader := bufio.NewReaderSize(body, constant.ScannerBufferInitial)
	result := chatResponsesStreamResult{}
	for {
		event, err := codex.ReadSSEEvent(reader, constant.ScannerBufferMax*2)
		if err != nil {
			if err == io.EOF {
				// 如果转换器已累积状态但未生成终态事件，合成 [DONE] 触发 response.completed
				// （存在转换错误时转换器只会产出 failed 终态，绝不合成 completed，P0-1）
				if !result.SuccessTerminal && !result.FailedTerminal && converterState != nil && *converterState != nil {
					synthEvents, synthErr := codex.ConvertOpenAIChatToResponsesWithContext(
						requestBody, nil, []byte("data: [DONE]"), converterState, fallbackReasoning)
					eventResult := inspectConvertedResponsesEvents(c, synthEvents)
					if eventResult.SuccessTerminal {
						result.SuccessTerminal = true
					}
					if eventResult.IncompleteTerminal {
						result.IncompleteTerminal = true
					}
					if eventResult.TerminalSeen {
						result.TerminalSeen = true
					}
					if eventResult.FailedTerminal {
						result.StreamErrored = true
						result.FailedTerminal = true
						if result.FailureError == nil {
							result.FailureError = eventResult.FailureError
						}
					}
					if synthErr != nil {
						// EOF 合成阶段暴露的转换错误（畸形工具 close / 已记录 ConversionError）：
						// 停止并上抛，错误不得 swallow；failed 终态已随 events 写出。
						result.StreamErrored = true
						result.FailedTerminal = true
						if result.FailureError == nil {
							result.FailureError = &model.Error{Message: synthErr.Error(), Type: "upstream_error", Code: "invalid_upstream_response"}
						}
						logger.Log.Errorf("[responses-converted] chat→responses EOF synth conversion failed: %v", synthErr)
						c.Writer.Flush()
						return result, synthErr
					}
					c.Writer.Flush()
				}
				return result, nil
			}
			codex.RenderTerminalStreamReadErrorEvent(c, err)
			c.Writer.Flush()
			result.StreamErrored = true
			result.FailedTerminal = true
			if result.FailureError == nil {
				result.FailureError = &model.Error{Message: err.Error(), Type: "stream_read_error", Code: "bad_response"}
			}
			return result, err
		}
		// 无 data 载荷的事件帧不构成 Chat chunk（chat §7.1 每 chunk 必为 data + JSON object），
		// 不得送入转换器触发初始化或 invalid_stream_event
		if event.Data == "" {
			continue
		}

		rawLine := "data: " + event.Data
		convertedEvents, convErr := codex.ConvertOpenAIChatToResponsesWithContext(requestBody, nil, []byte(rawLine), converterState, fallbackReasoning)
		eventResult := inspectConvertedResponsesEvents(c, convertedEvents)
		if eventResult.SuccessTerminal {
			result.SuccessTerminal = true
		}
		if eventResult.IncompleteTerminal {
			result.IncompleteTerminal = true
		}
		if eventResult.TerminalSeen {
			result.TerminalSeen = true
		}
		if eventResult.FailedTerminal {
			result.StreamErrored = true
			result.FailedTerminal = true
			result.FailureError = eventResult.FailureError
		}
		// 转换错误边界（契约 §1.3 / T7）：转换器已随 events 产出唯一 response.failed 终态
		// （SSE header 已提交，禁止再附加 JSON error body）；这里兜底补齐状态位并停止读流，
		// error 上抛供 controller 回滚预扣费与日志，禁止 swallow。
		if convErr != nil {
			result.StreamErrored = true
			result.FailedTerminal = true
			if result.FailureError == nil {
				result.FailureError = &model.Error{Message: convErr.Error(), Type: "upstream_error", Code: "invalid_upstream_response"}
			}
			logger.Log.Errorf("[responses-converted] chat→responses stream conversion failed: %v", convErr)
			c.Writer.Flush()
			_, _ = io.Copy(io.Discard, body)
			return result, convErr
		}
		c.Writer.Flush()
		if result.FailedTerminal || result.SuccessTerminal {
			// 消费剩余 body 以确保连接可复用
			_, _ = io.Copy(io.Discard, body)
			return result, nil
		}
	}
}

func formatSSEEvent(event codex.SSEEvent) string {
	var b strings.Builder
	if event.Event != "" {
		b.WriteString("event: ")
		b.WriteString(event.Event)
		b.WriteByte('\n')
	}
	if event.Data != "" {
		for i, line := range strings.Split(event.Data, "\n") {
			if i > 0 {
				b.WriteByte('\n')
			}
			b.WriteString("data: ")
			b.WriteString(line)
		}
	}
	return b.String()
}

// responsesPreConsumeEstimates 返回参与预扣估算的 (promptTokens, maxOutputTokens)。
//
// 这是估算器的调用方闸门：仅当请求根为良构对象时读取字段；畸形 / null / 非对象根一律返回
// (0, 0)，复刻旧实现 `json.Unmarshal` 失败或 null 根导致 map 为 nil 时两个估算器均返回 0 的语义。
// 两个估算器本身只做纯字段读取与算术，不重复全量校验（缺陷 2）。
func responsesPreConsumeEstimates(root gjson.Result, rootKind responsesRootKind) (promptTokens, maxOutputTokens int) {
	if rootKind != responsesRootObject {
		return 0, 0
	}
	return estimateResponsesPromptTokens(root), estimateResponsesMaxOutputTokens(root)
}

// maxOutputTokensCap 单请求输出上限，用于 clamp 预扣估算输入，防止极大值导致额度计算溢出
const maxOutputTokensCap = 1_000_000

// estimateResponsesMaxOutputTokens returns zero for absent, nonnumeric, or nonpositive values and clamps positive values to maxOutputTokensCap.
//
// 前置条件：调用方必须已确保请求为良构 JSON（且根为对象）。本函数只做纯字段读取与算术，
// 不调用 json.Valid / gjson.Valid 或任何等价的全量校验器（缺陷 2：良构性结论由调用方 gate 一次）。
//
// 1e300 级极大值直接 int() 转换会溢出为负数，使预扣额度变负；故转换前 clamp 到 maxOutputTokensCap。
// 必须用 Type 判定数字（.Float() 对非数字返回 0），否则字符串等类型会被误当作合法值。
// 非对象根的 Type 防御：gjson 对非对象根按路径取值返回不存在，故直接返回 0。
func estimateResponsesMaxOutputTokens(request gjson.Result) int {
	if !request.IsObject() {
		return 0
	}
	v := request.Get("max_output_tokens")
	if v.Type != gjson.Number {
		return 0
	}
	f := v.Float()
	if f <= 0 {
		return 0
	}
	if f > maxOutputTokensCap {
		return maxOutputTokensCap
	}
	// 与旧实现 int(float64) 一致：正数截断
	return int(f)
}

// estimateResponsesPromptTokens estimates instructions, input, and tools from one shared lazy root while preserving the minimum of 10.
//
// 前置条件：调用方必须已确保请求为良构 JSON（且根为对象）。本函数只做纯字段读取与算术，
// 不调用 json.Valid / gjson.Valid 或任何等价的全量校验器（缺陷 2：良构性结论由调用方 gate 一次）。
// 非对象根的 Type 防御：gjson 对非对象根按路径取值返回不存在，故直接走 10 的兜底。
func estimateResponsesPromptTokens(request gjson.Result) int {
	if !request.IsObject() {
		return 0
	}
	promptTokens := 0

	// 估算 instructions 的 token 数：仅非空字符串计入（.String() 对非字符串返回字面表示，故先判 Type）
	if instructions := request.Get("instructions"); instructions.Type == gjson.String && instructions.Str != "" {
		promptTokens += openai.CountTokenInput(instructions.Str, "")
	}

	// 估算 input 的 token 数：字符串按实际 token，数组按每项 100 估算，其余类型（含对象/null）不计入
	if input := request.Get("input"); input.Exists() {
		switch {
		case input.Type == gjson.String:
			promptTokens += openai.CountTokenInput(input.Str, "")
		case input.IsArray():
			// 简单估算：每个消息大约 100 个 token（用 `#` 取长度，避免 Array() 构造切片）
			promptTokens += int(input.Get("#").Int()) * 100
		}
	}

	// 估算 tools 的 token 数：仅数组计入，每项约 200 个 token
	if tools := request.Get("tools"); tools.IsArray() {
		promptTokens += int(tools.Get("#").Int()) * 200
	}

	// 确保至少有一些 token 数
	if promptTokens < 10 {
		promptTokens = 10
	}

	return promptTokens
}

// extractModelCaseInsensitive 从良构 JSON 对象中按大小写不敏感语义提取顶层 model 字段，
// 与 middleware 的共享缓存结论保持一致（Unicode 简单折叠，等价 encoding/json 的 struct 字段匹配）。
//
// 语义（必须与 middleware.buildRequestBodyMetadata 完全对齐）：
//   - 大小写变体之间按文档序 last-wins（`{"model":"a","Model":"b"}` → "b"）；
//   - 键名字节完全相同的重复键 first-wins（jsonparser/gjson 的确定性行为，与 encoding/json 的 last-wins 不同）；
//   - 缺失或 null → ("", true)（合法零值）；null 变体不覆盖已有字符串变体值
//     （`{"model":"a","Model":null}` → "a"，与 encoding/json 对 string 字段的 null 无操作语义一致）；
//   - 存在但类型不符 → ("", false)。
//
// 重复键差异由 TestResponsesModelExtractionDuplicateKeys 锁定；缓存来源（middleware
// buildRequestBodyMetadata）的同一差异由 TestGetRequestModelDuplicateModelFirstWins 锁定。
//
// 返回的 bool 与缓存的 ModelValid 同义。
func extractModelCaseInsensitive(body []byte) (model string, valid bool) {
	valid = true
	var winningKey string
	gjson.ParseBytes(body).ForEach(func(key, value gjson.Result) bool {
		if !strings.EqualFold(key.Str, "model") {
			return true
		}
		// 字节完全相同的重复键：first-wins，跳过
		if winningKey != "" && key.Str == winningKey {
			return true
		}
		winningKey = key.Str
		switch value.Type {
		case gjson.String:
			// 库间差异声明（契约 AC-9）：非法 UTF-8 字节在 encoding/json 中会被净化为 U+FFFD，
			// 而 gjson / jsonparser 保留原始字节（value.Str 即原始字节的 string 视图）。本路径
			// 保持原始字节透传、不主动净化；该差异属已声明的解析库差异，不做代码修正。
			model = value.Str
		case gjson.Null:
			// 显式 null 对 string 字段是无操作（与 encoding/json 及 middleware 缓存语义一致）：
			// 不覆盖前一个大小写变体已写入的值，仅保持 ModelValid=true。
		default:
			model = ""
			valid = false
		}
		return true
	})
	return model, valid
}

// responsesSemanticWellFormed 把纯语法良构性升级为复刻旧实现 `json.Unmarshal(body, &map[string]any)`
// 的语义良构性：语法良构（json.Valid / 缓存的 WellFormed 均为 true）且根为对象时，若文档中存在
// 超出 float64 表示范围的数值字面量（如 1e400），旧实现会整体反序列化失败，故此处判为「不良构」。
//
// 非对象根不做数值检测：旧实现本就因根类型失败，两路已按 Malformed / NonObject 一致处理。
// 该函数对同一惰性根与同一 wellFormed 输入恒定产出相同结论，因此缓存命中（cachedWellFormed）
// 与缓存缺失（json.Valid）两路不会分叉（缺陷 4）。
func responsesSemanticWellFormed(root gjson.Result, wellFormed bool) bool {
	if !wellFormed || !root.IsObject() {
		return wellFormed
	}
	return !hasUnrepresentableNumber(root)
}

// responsesRootKind 描述请求体根节点的良构性与类型，用于复刻旧实现
// `json.Unmarshal(body, &map[string]interface{})` 的成败语义（缺陷 1 / 缺陷 3）。
type responsesRootKind int

const (
	// responsesRootMalformed：非良构 JSON，或超深嵌套使 json.Valid 失败（旧实现反序列化失败）。
	responsesRootMalformed responsesRootKind = iota
	// responsesRootObject：良构 JSON 对象（旧实现反序列化成功，可按键提取）。
	responsesRootObject
	// responsesRootNull：良构 JSON `null`（旧实现反序列化成功，但 map 保持 nil，不派生字段）。
	responsesRootNull
	// responsesRootNonObject：良构 JSON 数组/字符串/数字/布尔（旧实现反序列化失败）。
	responsesRootNonObject
)

// responsesRootKindOf 依据调用方已确定的良构性结论与惰性根类型，对请求体根分类。
//
// wellFormed 必须来自与 middleware 相同的判定源（缓存 WellFormed，或 json.Valid），
// 以保证缓存命中与回退两路对同一份 bytes 得到相同分类（缺陷 3）。
func responsesRootKindOf(root gjson.Result, wellFormed bool) responsesRootKind {
	if !wellFormed {
		return responsesRootMalformed
	}
	if root.IsObject() {
		return responsesRootObject
	}
	if root.Type == gjson.Null {
		return responsesRootNull
	}
	return responsesRootNonObject
}

// isValidResponsesRoot 判定完整 body 是否为词法良构（gjson.ValidBytes）的 JSON 对象。
// gjson.Parse 对畸形 JSON 仍可能给出 IsObject()==true（如 `{invalid`），且会跳过前导非空白
// 字节（如 `\x00{...}` → Raw 仅含 `{...}`），故必须同时校验 IsObject 与**完整 body**的词法
// 良构性：只看惰性根的 Raw 会把前导垃圾一并放行，被 sjson 原样保留后转发上游（优化项 3）。
//
// 该函数专供 model 改写守卫使用（sjson / rebuildModelField 直接操作原始字节，可安全处理
// 超深嵌套等 encoding/json 无法反序列化的输入）；字段派生与估算的良构性判定不使用此函数，
// 而是统一走 json.Valid / 共享缓存 + 超范围数值语义闸门，以与 middleware 对齐（缺陷 3）。
//
// gjson.ValidBytes 无嵌套深度上限，故超深嵌套 body（json.Valid=false、gjson.Valid=true）
// 仍被视为可改写，锁定任务 3.4 登记的 sanctioned 差异。
//
// 与 encoding/json 的已知差异（AC-9）：重复键 gjson 取第一个、encoding/json 取最后一个
// （如 `{"max_output_tokens":10,"max_output_tokens":20}` → gjson 得 10、旧实现得 20）。本项目可接受。
// 该差异在 model 改写触发输入上的取值由 TestRelayResponsesDirectModelRewriteUsesSJSON 的
// 重复键用例锁定。
func isValidResponsesRoot(request gjson.Result, body []byte) bool {
	return request.IsObject() && gjson.ValidBytes(body)
}

// hasUnrepresentableNumber 递归检测 JSON 中是否存在超出 float64 表示范围的数字字面量。
// 等价于模块 B `relay/adaptor/openai/main.go` 的同名实现（缺陷 4 的参考先例）：
// gjson 对超出 float64 范围的数字取值为 ±Inf，与旧实现 `json.Unmarshal(body, &map[string]any)`
// 报错（`cannot unmarshal number 1e400 into Go value of type float64`）的判定等价；
// 下溢到 0 的字面量（如 1e-400）取值为有限数，与 encoding/json 不报错的行为一致。
//
// 仅对对象根调用即可（调用方已保证）：非对象根的旧实现本就因根类型失败，无需数值检测。
func hasUnrepresentableNumber(result gjson.Result) bool {
	switch result.Type {
	case gjson.Number:
		return math.IsInf(result.Num, 0)
	case gjson.JSON:
		found := false
		result.ForEach(func(_, value gjson.Result) bool {
			if hasUnrepresentableNumber(value) {
				found = true
				return false
			}
			return true
		})
		return found
	default:
		return false
	}
}

// countModelKeys 统计顶层 model 键的出现次数（大小写不敏感，与共享缓存的键匹配语义一致）。
// 用于识别 sjson.SetBytes 无法正确处理的重复键场景：它只替换首个匹配键，而上游按文档序
// last-wins 取值，会取到未被映射的后续键。
func countModelKeys(body []byte) int {
	count := 0
	gjson.ParseBytes(body).ForEach(func(key, _ gjson.Result) bool {
		if strings.EqualFold(key.Str, "model") {
			count++
		}
		return true
	})
	return count
}

// rebuildModelField 重建请求体顶层，剔除全部 model 键变体（大小写不敏感、含转义等价键）
// 后追加一个规范的 "model" 键。用于 sjson.SetBytes 无法正确处理的重复键场景：它只替换首个
// 匹配键，而上游按文档序 last-wins 会取到未被映射的后续键。
//
// 非 model 字段沿用原始字节表示，不做 JSON 值归一化，因此 1e400、超深嵌套等 encoding/json
// 无法反序列化的值也能原样保留（这是与旧 map 归一化路径的关键差异：后者对这类输入会失败）。
// 骨架为空对象时 sjson.SetBytes 会在空对象上追加 model 键，无需特殊处理。
func rebuildModelField(body []byte, actualModel string) ([]byte, error) {
	var skeleton bytes.Buffer
	skeleton.WriteByte('{')
	first := true
	gjson.ParseBytes(body).ForEach(func(key, value gjson.Result) bool {
		if strings.EqualFold(key.Str, "model") {
			return true
		}
		if !first {
			skeleton.WriteByte(',')
		}
		first = false
		skeleton.WriteString(key.Raw)
		skeleton.WriteByte(':')
		skeleton.WriteString(value.Raw)
		return true
	})
	skeleton.WriteByte('}')
	return sjson.SetBytes(skeleton.Bytes(), "model", actualModel)
}

func rollbackResponsesPreConsumedQuota(ctx context.Context, userId int) {
	if preConsumed, ok := ctx.Value(CtxKeyPreConsumedQuota).(int64); ok && preConsumed > 0 {
		if err := dbmodel.IncreaseUserQuota(userId, preConsumed); err != nil {
			logger.Log.Errorf("error rolling back pre-consumed quota: %v", err)
			return
		}
		dbmodel.PostConsumeResetUserQuotaCache(ctx, userId, preConsumed)
	}
}

func postConsumeQuotaForResponses(ctx context.Context, usage *model.Usage, meta *metaPkg.Meta, ratio float64, modelRatio float64, reqBody string, respBody string, reqHeader string) {
	if usage == nil {
		logger.Log.Errorf("usage is nil, which is unexpected")
		return
	}

	var quota int64
	completionRatio := billingratio.GetCompletionRatio(meta.ActualModelName, meta.ChannelType)
	promptTokens := usage.PromptTokens
	completionTokens := usage.CompletionTokens
	// 从 usage 中提取缓存命中的token数
	cachedTokens := 0
	if usage.PromptTokensDetails != nil {
		cachedTokens = usage.PromptTokensDetails.CachedTokens
	}
	quota = int64(math.Ceil((float64(promptTokens) + float64(completionTokens)*completionRatio) * ratio))

	if ratio != 0 && quota <= 0 {
		quota = 1
	}

	totalTokens := promptTokens + completionTokens

	// Check pre-consumed quota early so we can rollback if totalTokens == 0
	preConsumedQuota := int64(0)
	if v := ctx.Value(CtxKeyPreConsumedQuota); v != nil {
		preConsumedQuota = v.(int64)
	}

	if totalTokens == 0 {
		if preConsumedQuota > 0 {
			logger.Log.Warnf("totalTokens is 0 for user %d, model %s, rolling back pre-consumed quota", meta.UserId, meta.ActualModelName)
			if err := dbmodel.IncreaseUserQuota(meta.UserId, preConsumedQuota); err != nil {
				logger.Log.Errorf("error rolling back pre-consumed quota: " + err.Error())
			}
			dbmodel.PostConsumeResetUserQuotaCache(ctx, meta.UserId, preConsumedQuota)
		}
		return
	}

	var err error
	if preConsumedQuota > 0 {
		diff := quota - preConsumedQuota
		if diff > 0 {
			err = dbmodel.DecreaseUserQuota(meta.UserId, diff)
		} else if diff < 0 {
			err = dbmodel.IncreaseUserQuota(meta.UserId, -diff)
		}
		// diff == 0: exactly right, no adjustment needed
	} else {
		err = dbmodel.DecreaseUserQuota(meta.UserId, quota)
	}
	if err != nil {
		logger.Log.Errorf("error decrease user quota: " + err.Error())
	}
	// DB quota has already been updated above; refresh Redis cache from DB.
	dbmodel.PostConsumeResetUserQuotaCache(ctx, meta.UserId, quota)

	groupRatio := dbmodel.GetGroupModelRatio(meta.Group)
	logContent := fmt.Sprintf("Responses API - 倍率：%.2f × %.2f × 分组%.2f", modelRatio, completionRatio, groupRatio)

	logRecord := &dbmodel.Log{
		UserId:            meta.UserId,
		ChannelId:         meta.ChannelId,
		PromptTokens:      promptTokens,
		CompletionTokens:  completionTokens,
		CachedTokens:      cachedTokens,
		ModelName:         meta.ActualModelName,
		TokenName:         meta.TokenName,
		Quota:             int(quota),
		Content:           logContent,
		IsStream:          meta.IsStream,
		ElapsedTime:       helper.CalcElapsedTime(meta.StartTime),
		FirstTokenTime:    getFirstTokenTime(ctx),
		SystemPromptReset: false,
		ChannelName:       meta.ChannelName,
		RequestBody:       reqBody,
		ResponseBody:      respBody,
		RequestHeader:     reqHeader,
	}
	dbmodel.RecordConsumeLog(ctx, logRecord)

	if logRecord.Id > 0 {
		active.BroadcastComplete(&active.LogRecordData{
			Id:               logRecord.Id,
			UserId:           logRecord.UserId,
			CreatedAt:        logRecord.CreatedAt,
			Content:          logRecord.Content,
			Username:         logRecord.Username,
			TokenName:        logRecord.TokenName,
			ModelName:        logRecord.ModelName,
			Quota:            logRecord.Quota,
			PromptTokens:     logRecord.PromptTokens,
			CompletionTokens: logRecord.CompletionTokens,
			CachedTokens:     logRecord.CachedTokens,
			ChannelId:        logRecord.ChannelId,
			RequestId:        logRecord.RequestId,
			ElapsedTime:      logRecord.ElapsedTime,
			FirstTokenTime:   logRecord.FirstTokenTime,
			IsStream:         logRecord.IsStream,
			ChannelName:      logRecord.ChannelName,
			HasRequestBody:   logRecord.RequestBody != "",
			HasResponseBody:  logRecord.ResponseBody != "",
			HasRequestHeader: logRecord.RequestHeader != "",
		})
	}

	dbmodel.UpdateUserUsedQuotaAndRequestCount(meta.UserId, quota)
	dbmodel.UpdateChannelUsedQuota(meta.ChannelId, quota)
}

func isErrorResp(resp *http.Response) bool {
	return resp.StatusCode != http.StatusOK
}

func relayErrorHandler(resp *http.Response) *model.ErrorWithStatusCode {
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		logger.Log.Errorf("[%s] %+v", "read response body failed", err)
		return openai.ErrorWrapper(err, "read response body failed", http.StatusInternalServerError)
	}
	err = resp.Body.Close()
	if err != nil {
		logger.Log.Errorf("[%s] %+v", "close response body failed", err)
		return openai.ErrorWrapper(err, "close response body failed", http.StatusInternalServerError)
	}
	resp.Body = io.NopCloser(bytes.NewBuffer(respBody))

	var openaiErr model.Error
	err = json.Unmarshal(respBody, &openaiErr)
	if err != nil {
		logger.Log.Errorf("[%s] raw response: %s, err: %+v", "unmarshal response body failed", string(respBody), err)
		openaiErr = model.Error{
			Message: string(respBody),
			Type:    "server_error",
			Code:    "response_parse_error",
		}
	}
	if openaiErr.Message == "" {
		openaiErr.Message = string(respBody)
	}
	return &model.ErrorWithStatusCode{
		Error:      openaiErr,
		StatusCode: resp.StatusCode,
	}
}
