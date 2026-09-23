package middleware

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/buger/jsonparser"
	"github.com/gin-gonic/gin"
	"github.com/pai801/myapi/common"
	"github.com/pai801/myapi/common/ctxkey"
	"github.com/pai801/myapi/common/helper"
	"github.com/pai801/myapi/common/logger"
	"github.com/pai801/myapi/model"
)

func abortWithMessage(c *gin.Context, statusCode int, message string) {
	c.JSON(statusCode, gin.H{
		"error": gin.H{
			"message": helper.MessageWithRequestId(message, c.GetString(helper.RequestIdKey)),
			"type":    "myapi_error",
		},
	})
	c.Abort()
	logger.Log.Errorf(message)
}

// errRequestBodyNotWellFormed / errRequestBodyModelType 是 JSON 分支的失败原因。
// 外层统一沿用改造前的 "common.UnmarshalBodyReusable failed" 前缀：该字符串会经
// middleware/auth.go 的 abortWithMessage 直接回给客户端，改动它属于契约未声明的可观测变更。
var (
	errRequestBodyNotWellFormed = errors.New("request body is not well-formed JSON")
	errRequestBodyModelType     = errors.New("request model has an unexpected JSON type")
)

// getRequestModel returns the request model while preserving route defaults, non-JSON ShouldBind
// behavior, and downstream body readability; malformed or model-type-invalid JSON returns an error.
//
// JSON 分支不再 json.Unmarshal 到 ModelRequest 全量反序列化，而是 GetRequestBody + json.Valid
// 预检 + jsonparser 按需扫描，并把 well-formedness / model / stream 结论以具体值写入共享缓存
// （ctxkey.KeyRequestBodyMetadata），供 detectStreamFromBody 与 responses 入口复用，避免同一
// body 被 TokenAuth / relay 入口 / responses 路径重复扫描。
//
// json.Valid 只保证 well-formedness，**拦不住类型不匹配**（如 {"model":123} 合法但类型错），
// 故 model / stream 必须经自带类型校验的类型化读取（设计规则 R2）：由 buildRequestBodyMetadata
// 在扫描时按 jsonparser.ValueType 显式分支，类型不符置对应 Valid=false，绝不从原始字节派生值。
func getRequestModel(c *gin.Context) (modelName string, err error) {
	if strings.HasPrefix(c.Request.Header.Get("Content-Type"), "application/json") {
		modelName, err = extractRequestModelFromJSON(c)
		if err != nil {
			return "", err
		}
	} else {
		// 非 JSON（form / multipart）保持既有 ShouldBind 路径不变：复用 UnmarshalBodyReusable，
		// 它同样会缓存 body 并恢复 c.Request.Body，且 jsonparser 不适用于 form 数据。
		var modelRequest ModelRequest
		if err := common.UnmarshalBodyReusable(c, &modelRequest); err != nil {
			return "", fmt.Errorf("common.UnmarshalBodyReusable failed: %w", err)
		}
		modelName = modelRequest.Model
	}

	// 路由默认值：与改造前逐条保持一致，不改变 moderation / embeddings / image / audio 行为。
	if strings.HasPrefix(c.Request.URL.Path, "/v1/moderations") {
		if modelName == "" {
			modelName = "text-moderation-stable"
		}
	}
	if strings.HasSuffix(c.Request.URL.Path, "embeddings") {
		if modelName == "" {
			modelName = c.Param("model")
		}
	}
	if strings.HasPrefix(c.Request.URL.Path, "/v1/images/generations") {
		if modelName == "" {
			modelName = "dall-e-2"
		}
	}
	if strings.HasPrefix(c.Request.URL.Path, "/v1/audio/transcriptions") || strings.HasPrefix(c.Request.URL.Path, "/v1/audio/translations") {
		if modelName == "" {
			modelName = "whisper-1"
		}
	}
	return modelName, nil
}

// extractRequestModelFromJSON 从 JSON 请求体按需提取 model，并写入共享元数据缓存。
//
// 语义：
//   - 先 json.Valid 建立 well-formedness；畸形 JSON 立即返回错误，绝不从半截 JSON 取值。
//   - model 缺失或为 null → 零值 ""（与 encoding/json 一致），不报错。
//   - model 存在但类型非字符串 → 返回错误，不产出误导值（契约：model 类型非法必须失败）。
//
// 无论成功或失败都会恢复 c.Request.Body：GetRequestBody 读后不恢复，若不重建，下游直读
// body 的路由（proxy / audio / text / image）会拿到空 body。
func extractRequestModelFromJSON(c *gin.Context) (string, error) {
	body, err := common.GetRequestBody(c)
	if err != nil {
		return "", fmt.Errorf("common.UnmarshalBodyReusable failed: %w", err)
	}
	if c.Request != nil {
		c.Request.Body = io.NopCloser(bytes.NewBuffer(body))
	}

	metadata := buildRequestBodyMetadata(body)
	// 以具体非指针值写入共享缓存；缓存缺失/类型不符时消费方自行回退自解析，缓存不是正确性依赖。
	c.Set(ctxkey.KeyRequestBodyMetadata, metadata)

	if !metadata.WellFormed {
		return "", fmt.Errorf("common.UnmarshalBodyReusable failed: %w", errRequestBodyNotWellFormed)
	}
	if !metadata.ModelValid {
		return "", fmt.Errorf("common.UnmarshalBodyReusable failed: %w", errRequestBodyModelType)
	}
	return metadata.Model, nil
}

// buildRequestBodyMetadata 从原始 body 推导共享元数据结论（不变式见 ctxkey.RequestBodyMetadata）：
//   - 畸形 JSON → 零值（WellFormed=false 时其余字段必须为零值）。
//   - 根为数组/字符串/数字/布尔（不可接受的根）→ WellFormed=true 但两个 Valid 均为 false，
//     视为提取失败（见下方根类型判定），不得与「键缺失」混为一类。
//   - 根为 null → 等价于空对象（encoding/json 把 null 反序列化进 struct 视为无操作），
//     model / stream 均按键缺失处理为合法零值，两个 Valid 保持 true。
//   - 对象根下 model / stream 缺失或为 null → 合法零值，对应 Valid 保持 true，不改变控制流。
//   - 对象根下 model / stream 存在但类型不符 → 对应 Valid 为 false，永不产出派生值。
//
// 键匹配语义刻意**不对称**，各自对齐其既有消费方的行为，不得统一：
//   - `model` 大小写不敏感：改造前把 body 反序列化进 struct（json.Unmarshal(body, &ModelRequest)），
//     encoding/json 对字段名先精确匹配、再按 Unicode 简单折叠（foldName）匹配，故 `Model`/`MODEL`/
//     `mOdel` 等任意大小写变体都会命中 `json:"model"`。本函数用 bytes.EqualFold（与 encoding/json
//     的 foldName 同为 Unicode 简单折叠）复现该语义，避免静默丢失此类请求的 model。
//   - `stream` 大小写敏感（只认精确键名 `stream`）：改造前的消费方均用精确键查找——detectStreamFromBody
//     用 bodyMap["stream"]，relayResponsesDirect / relayResponsesConverted 用 req["stream"]，故
//     `Stream`/`STREAM` 等变体一律视为键缺失。这里必须精确匹配：任务 2.3 要求 detectStreamFromBody
//     优先读共享缓存、缺失时回退 jsonparser.GetBoolean(body, "stream")（精确匹配），并验收两条路径
//     结果一致。若此处按大小写不敏感匹配，`{"Stream":true}` 会在缓存命中时得 true、回退时得 false，
//     两路径不一致，破坏 2.3 的验收标准。
//
// 值选取与 encoding/json 对齐：encoding/json 按键在文档中的出现顺序逐键赋值，故 model 的大小写
// 变体之间是 last-wins（`{"model":"a","Model":"b"}` → "b"）。
//
// 与 encoding/json 的已知差异：字节完全相同的重复键取第一个（first-wins），encoding/json 取最后
// 一个（last-wins）。该差异仅限「键名字节完全一致」的重复；model 的不同大小写变体之间仍按 last-wins。
// 由 TestGetRequestModelDuplicateModelFirstWins（model）与 TestGetRequestModelStreamKeyExactMatch
// （stream first-wins）锁定。
func buildRequestBodyMetadata(body []byte) ctxkey.RequestBodyMetadata {
	if !json.Valid(body) {
		return ctxkey.RequestBodyMetadata{}
	}

	// 根类型判定：JSON 文档的根必须是对象，或为 null（null 等价于「对象缺失」，见下）。
	// 改造前把 body 反序列化进 struct（json.Unmarshal(body, &ModelRequest)）：
	//   - 数组 / 字符串 / 数字 / 布尔根 → 得到 "cannot unmarshal ... into Go value of type
	//     ModelRequest" 错误，即提取失败；
	//   - null 根 → 反序列化成功（encoding/json 视 null 为无操作，不触碰目标 struct），
	//     等价于键全部缺失，属合法零值路径。
	// json.Valid 只保证 well-formedness，对上述各类根均返回 true，故必须在字段提取前显式区分
	// 「不可接受的根」（提取失败）与「对象 / null 根但键缺失」（合法零值）。
	rootType := jsonRootType(body)
	if rootType == jsonparser.Null {
		// null 根：encoding/json 把 null 反序列化进 struct 视为无操作，等价于空对象，
		// 故 model / stream 均按「键缺失」处理为合法零值（getRequestModel 返回 ("", nil)）。
		return ctxkey.RequestBodyMetadata{WellFormed: true, ModelValid: true, StreamValid: true}
	}
	if rootType != jsonparser.Object {
		// 数组 / 字符串 / 数字 / 布尔根，model / stream 均无法按对象键提取：WellFormed=true、
		// 两个 Valid=false，令 extractRequestModelFromJSON 走 ModelValid=false 的 error 路径
		// （恢复改造前 400 行为）。
		return ctxkey.RequestBodyMetadata{WellFormed: true}
	}

	metadata := ctxkey.RequestBodyMetadata{WellFormed: true, ModelValid: true, StreamValid: true}

	// 单次遍历顶层键：model 做大小写不敏感匹配（等价 encoding/json 的字段匹配），
	// stream 做精确键名匹配（等价既有消费方的 map 精确查找，见上方不对称性说明）。
	// 类型校验沿用设计规则 R2：存在但类型不符 → 对应 Valid 置 false，永不产出派生值；
	// null 与缺失 → 合法零值，对应 Valid 保持 true。
	// modelKey 记录当前胜出键的原始字节，仅用于识别「键名字节完全相同的重复键」：命中重复键时
	// 跳过（first-wins），其余大小写变体按文档序覆盖（last-wins，与 encoding/json 一致）。
	// stream 因是精确匹配，所有命中键在解码后字节相同（含转义拼写），故 streamSeen 即代表
	// first-wins：仅首个 `stream` 键生效。
	//
	// 必须遍历到文档末尾（无法在命中首个匹配键后提前返回）：model 是 last-wins，提前返回会把
	// `{"model":"a","Model":"b"}` 误判为 "a"。这是与旧 GetString/GetBoolean 精确查找相比的主要
	// 开销来源；json.Valid 预检（O(n) 全量）本就主导耗时，故整体仍显著优于改造前的 encoding/json
	// 结构体解码。
	var modelKey []byte
	var streamSeen bool
	_ = jsonparser.ObjectEach(body, func(key, value []byte, dataType jsonparser.ValueType, _ int) error {
		switch {
		case bytes.EqualFold(key, []byte("model")):
			if !bytes.Equal(key, modelKey) {
				modelKey = key
				switch dataType {
				case jsonparser.String:
					if s, err := jsonparser.ParseString(value); err == nil {
						metadata.Model = s
					} else {
						metadata.ModelValid = false
					}
				case jsonparser.Null:
					// 显式 null → 零值，ModelValid 保持 true（与 encoding/json 同语义）。
				default:
					metadata.ModelValid = false
				}
			}
		case bytes.Equal(key, []byte("stream")):
			if !streamSeen {
				streamSeen = true
				switch dataType {
				case jsonparser.Boolean:
					metadata.Stream = value[0] == 't'
				case jsonparser.Null:
					// 显式 null → 零值，StreamValid 保持 true。
				default:
					metadata.StreamValid = false
				}
			}
		}
		return nil
	})

	return metadata
}

// jsonRootType 返回 JSON 文档根节点的 jsonparser 类型。调用前必须已通过 json.Valid；
// 此时 jsonparser.Get 对根取值不会失败，dataType 即为根的实际类型。
//
// 语义对齐改造前的 json.Unmarshal(body, &struct)：
//   - Object 根 → 正常按键提取；
//   - Null 根 → 反序列化进 struct 视为无操作，等价于对象缺失（合法零值）；
//   - Array / String / Number / Boolean 根 → 反序列化失败，必须返回 error。
func jsonRootType(body []byte) jsonparser.ValueType {
	_, dataType, _, err := jsonparser.Get(body)
	if err != nil {
		return jsonparser.Unknown
	}
	return dataType
}

func isModelInList(modelName string, models string) bool {
	if modelName == "" {
		return false
	}
	if modelName == "auto" {
		return true
	}
	if models == "" {
		return true
	}
	simplified := model.SimplifyModelName(modelName)
	modelList := strings.Split(models, ",")
	for _, alias := range modelList {
		simplifiedAlias := model.SimplifyModelName(alias)
		// 必须精确匹配：若允许名以请求名为前缀（HasPrefix(alias, request)），
		// 令牌仅允许 o3-mini 时可越权请求 o3 等更贵模型；反向前缀同理
		if simplified == simplifiedAlias {
			return true
		}
	}
	return false
}
