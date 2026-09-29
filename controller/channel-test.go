package controller

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/pai801/myapi/common/config"
	"github.com/pai801/myapi/common/ctxkey"
	"github.com/pai801/myapi/common/helper"
	"github.com/pai801/myapi/common/logger"
	"github.com/pai801/myapi/middleware"
	"github.com/pai801/myapi/model"
	"github.com/pai801/myapi/monitor"
	"github.com/pai801/myapi/relay"
	"github.com/pai801/myapi/relay/adaptor"
	"github.com/pai801/myapi/relay/adaptor/openai"
	"github.com/pai801/myapi/relay/channeltype"
	"github.com/pai801/myapi/relay/controller"
	"github.com/pai801/myapi/relay/meta"
	relaymodel "github.com/pai801/myapi/relay/model"
	"github.com/pai801/myapi/relay/relaymode"
)

func buildTestRequest(model string) *relaymodel.GeneralOpenAIRequest {
	if model == "" {
		model = "gpt-3.5-turbo"
	}
	testRequest := &relaymodel.GeneralOpenAIRequest{
		Model: model,
	}
	testMessage := relaymodel.Message{
		Role:    "user",
		Content: config.TestPrompt,
	}
	testRequest.Messages = append(testRequest.Messages, testMessage)
	return testRequest
}

func parseTestResponse(resp string) (*openai.TextResponse, string, error) {
	var response openai.TextResponse
	err := json.Unmarshal([]byte(resp), &response)
	if err != nil {
		return nil, "", err
	}
	if len(response.Choices) == 0 {
		return nil, "", errors.New("response has no choices")
	}
	stringContent, ok := response.Choices[0].Content.(string)
	if !ok {
		return nil, "", errors.New("response content is not string")
	}
	return &response, stringContent, nil
}

func formatFailureResponseBody(statusCode int, body []byte) []byte {
	if statusCode == 0 && len(body) == 0 {
		return nil
	}
	result, _ := json.Marshal(map[string]interface{}{
		"status_code": statusCode,
		"body":        string(body),
	})
	return result
}

// limitLoggedBody 按 config.MaxLoggedBodySize 对委托路径的诊断大字段限长，
// 口径与消费日志一致（见 controller/relay.go 与 relay/controller/text.go）：
// 超限写 "[body too large: N bytes]" 占位，空体保持空。
func limitLoggedBody(body string) string {
	if body == "" {
		return ""
	}
	if len(body) <= config.MaxLoggedBodySize {
		return body
	}
	return fmt.Sprintf("[body too large: %d bytes]", len(body))
}

// testerFor 对已注册适配器做可选的 adaptor.Tester 能力发现：接口存在性即能力发现。
//
// 抽为独立函数而非在 testChannel 内联断言，原因有二：其一，testChannel 内的局部变量
// adaptor 会遮蔽包名 adaptor，无法直接书写 adaptor.Tester；其二，此处是「测试能力发现」
// 的唯一真源，便于审计与复用。adp == nil 时安全返回 false（调用方此前已返回既有错误）。
func testerFor(adp adaptor.Adaptor) (adaptor.Tester, bool) {
	if adp == nil {
		return nil, false
	}
	tester, ok := adp.(adaptor.Tester)
	return tester, ok
}

func testChannel(ctx context.Context, channel *model.Channel, request *relaymodel.GeneralOpenAIRequest) (responseMessage string, err error, openaiErr *relaymodel.Error) {
	startTime := time.Now()
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = &http.Request{
		Method: "POST",
		URL:    &url.URL{Path: "/v1/chat/completions"},
		Body:   nil,
		Header: make(http.Header),
	}
	c.Request.Header.Set("Authorization", "Bearer "+channel.Key)
	c.Request.Header.Set("Content-Type", "application/json")
	c.Set(ctxkey.Channel, channel.Type)
	c.Set(ctxkey.BaseURL, channel.GetBaseURL())
	cfg, _ := channel.LoadConfig()
	c.Set(ctxkey.Config, cfg)
	middleware.SetupContextForSelectedChannel(c, channel, "")
	meta := meta.GetByContext(c)
	apiType := channeltype.ToAPIType(channel.Type)
	adaptor := relay.GetAdaptor(apiType)
	if adaptor == nil {
		return "", fmt.Errorf("invalid api type: %d, adaptor is nil", apiType), nil
	}
	adaptor.Init(meta)
	modelName := request.Model
	modelMap := channel.GetModelMapping()
	if modelName == "" || !strings.Contains(channel.Models, modelName) {
		modelNames := channel.GetModels()
		if len(modelNames) > 0 {
			modelName = modelNames[0]
		}
	}
	if modelMap != nil && modelMap[modelName] != "" {
		modelName = modelMap[modelName]
	}
	meta.OriginModelName, meta.ActualModelName = request.Model, modelName
	request.Model = modelName
	// —— 可选能力分派：接口存在性即能力发现（见 relay/adaptor/tester.go）——
	// 仅当适配器实现可选的 adaptor.Tester 时，才把连通性测试委托给渠道自身的 TestChannel，
	// 不再走 ConvertRequest / DoRequest / DoResponse / parseTestResponse 的通用约定。
	// 未实现该接口的渠道保持下方通用流程逐字节不变；adp == nil 已在更早处返回既有错误。
	// 测试日志归属 controller：命中委托分支时此处补记唯一一条日志后返回，
	// 渠道实现 MUST NOT 自行落库；日志形状与通用路径一致（Content 沿用成功/失败文案）。
	if tester, ok := testerFor(adaptor); ok {
		result, testErr := tester.TestChannel(ctx, meta)
		summary := result.Summary
		logContent := fmt.Sprintf("渠道 %s 测试成功，响应：%s", channel.Name, summary)
		if testErr != nil {
			// 失败只回传渠道受控的 err（实现方负责脱敏，不得含密钥/凭证/上游原文）；
			// 已捕获的诊断材料（出站体 / 上游错误体节选）仍落库，仅入服务端 DB，不回传客户端。
			logContent = fmt.Sprintf("渠道 %s 测试失败，错误：%s", channel.Name, testErr.Error())
		}
		model.RecordTestLog(ctx, &model.Log{
			ChannelId:        channel.Id,
			ChannelName:      channel.Name,
			ModelName:        modelName,
			Content:          logContent,
			ElapsedTime:      helper.CalcElapsedTime(startTime),
			RequestBody:      limitLoggedBody(result.RequestBody),
			ResponseBody:     limitLoggedBody(result.ResponseBody),
			PromptTokens:     result.PromptTokens,
			CompletionTokens: result.CompletionTokens,
			CachedTokens:     result.CachedTokens,
		})
		if testErr != nil {
			return "", testErr, nil
		}
		return summary, nil, nil
	}
	convertedRequest, err := adaptor.ConvertRequest(c, relaymode.ChatCompletions, request)
	if err != nil {
		return "", err, nil
	}
	jsonData, err := json.Marshal(convertedRequest)
	if err != nil {
		return "", err, nil
	}
	var respBody []byte
	// usage 上提到 defer 之前：日志字面量位于 defer 闭包内，需在闭包词法作用域可见。
	// 这是纯作用域搬迁（赋值时机与 nil 语义不变），使通用路径可落库 token 用量。
	var usage *relaymodel.Usage
	var respErr *relaymodel.ErrorWithStatusCode
	defer func() {
		logContent := fmt.Sprintf("渠道 %s 测试成功，响应：%s", channel.Name, responseMessage)
		if err != nil || openaiErr != nil {
			errorMessage := ""
			if err != nil {
				errorMessage = err.Error()
			} else {
				errorMessage = openaiErr.Message
			}
			logContent = fmt.Sprintf("渠道 %s 测试失败，错误：%s", channel.Name, errorMessage)
		}
		cachedTokens := 0
		promptTokens := 0
		completionTokens := 0
		if usage != nil {
			promptTokens = usage.PromptTokens
			completionTokens = usage.CompletionTokens
			if usage.PromptTokensDetails != nil {
				cachedTokens = usage.PromptTokensDetails.CachedTokens
			}
		}
		go model.RecordTestLog(ctx, &model.Log{
			ChannelId:        channel.Id,
			ChannelName:      channel.Name,
			ModelName:        modelName,
			Content:          logContent,
			ElapsedTime:      helper.CalcElapsedTime(startTime),
			RequestBody:      string(jsonData),
			ResponseBody:     string(respBody),
			PromptTokens:     promptTokens,
			CompletionTokens: completionTokens,
			CachedTokens:     cachedTokens,
		})
	}()
	logger.Log.Infof(string(jsonData))
	requestBody := bytes.NewBuffer(jsonData)
	c.Request.Body = io.NopCloser(requestBody)
	resp, err := adaptor.DoRequest(c, meta, requestBody)
	if err != nil {
		respBody = formatFailureResponseBody(0, []byte(err.Error()))
		return "", err, nil
	}
	if resp != nil && resp.StatusCode != http.StatusOK {
		rawBody, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		resp.Body = io.NopCloser(bytes.NewReader(rawBody))
		respBody = formatFailureResponseBody(resp.StatusCode, rawBody)
		err := controller.RelayErrorHandler(resp)
		errorMessage := err.Error.Message
		if errorMessage != "" {
			errorMessage = ", error message: " + errorMessage
		}
		return "", fmt.Errorf("http status code: %d%s", resp.StatusCode, errorMessage), &err.Error
	}
	usage, respErr = adaptor.DoResponse(c, resp, meta)
	if respErr != nil {
		if resp != nil {
			rawBody, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			resp.Body = io.NopCloser(bytes.NewReader(rawBody))
			if len(rawBody) == 0 {
				rawBody = []byte(respErr.Error.Message)
			}
			respBody = formatFailureResponseBody(resp.StatusCode, rawBody)
		} else {
			respBody = formatFailureResponseBody(0, []byte(respErr.Error.Message))
		}
		return "", fmt.Errorf("%s", respErr.Error.Message), &respErr.Error
	}
	if usage == nil {
		statusCode := 0
		if resp != nil {
			statusCode = resp.StatusCode
		}
		respBody = formatFailureResponseBody(statusCode, nil)
		return "", errors.New("usage is nil"), nil
	}
	rawResponse := w.Body.String()
	_, responseMessage, err = parseTestResponse(rawResponse)
	if err != nil {
		logger.Log.Errorf("failed to parse error: %s, \nresponse: %s", err.Error(), rawResponse)
		respBody = formatFailureResponseBody(200, []byte(rawResponse))
		return "", err, nil
	}
	result := w.Result()
	// print result.Body
	respBody, err = io.ReadAll(result.Body)
	if err != nil {
		return "", err, nil
	}
	logger.Log.Infof("testing channel #%d, response: \n%s", channel.Id, string(respBody))
	return responseMessage, nil, nil
}

func TestChannel(c *gin.Context) {
	ctx := c.Request.Context()
	id, err := strconv.Atoi(c.Param("id"))
	if err != nil {
		c.JSON(http.StatusOK, gin.H{
			"success": false,
			"message": err.Error(),
		})
		return
	}
	channel, err := model.GetChannelById(id, true)
	if err != nil {
		c.JSON(http.StatusOK, gin.H{
			"success": false,
			"message": err.Error(),
		})
		return
	}
	modelName := c.Query("model")
	testRequest := buildTestRequest(modelName)
	tik := time.Now()
	responseMessage, err, _ := testChannel(ctx, channel, testRequest)
	tok := time.Now()
	milliseconds := tok.Sub(tik).Milliseconds()
	if err != nil {
		milliseconds = 0
	}
	go channel.UpdateResponseTime(milliseconds)
	consumedTime := float64(milliseconds) / 1000.0
	if err != nil {
		c.JSON(http.StatusOK, gin.H{
			"success":   false,
			"message":   err.Error(),
			"time":      consumedTime,
			"modelName": modelName,
		})
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"success":   true,
		"message":   responseMessage,
		"time":      consumedTime,
		"modelName": modelName,
	})
	return
}

var testAllChannelsLock sync.Mutex
var testAllChannelsRunning bool = false

func testChannels(ctx context.Context, notify bool, scope string) error {
	if config.RootUserEmail == "" {
		config.RootUserEmail = model.GetRootUserEmail()
	}
	testAllChannelsLock.Lock()
	if testAllChannelsRunning {
		testAllChannelsLock.Unlock()
		return errors.New("测试已在运行中")
	}
	testAllChannelsRunning = true
	testAllChannelsLock.Unlock()
	channels, err := model.GetAllChannels(0, 0, scope)
	if err != nil {
		return err
	}
	var disableThreshold = int64(config.ChannelDisableThreshold * 1000)
	if disableThreshold == 0 {
		disableThreshold = 10000000 // a impossible value
	}
	go func() {
		for _, channel := range channels {
			isChannelEnabled := channel.Status == model.ChannelStatusEnabled
			tik := time.Now()
			testRequest := buildTestRequest("")
			_, err, openaiErr := testChannel(ctx, channel, testRequest)
			tok := time.Now()
			milliseconds := tok.Sub(tik).Milliseconds()
			if isChannelEnabled && milliseconds > disableThreshold {
				err = fmt.Errorf("响应时间 %.2fs 超过阈值 %.2fs", float64(milliseconds)/1000.0, float64(disableThreshold)/1000.0)
				if config.AutomaticDisableChannelEnabled {
					monitor.DisableChannel(channel.Id, channel.Name, err.Error())
				}
			}
			if isChannelEnabled && monitor.ShouldDisableChannel(openaiErr, -1) {
				monitor.DisableChannel(channel.Id, channel.Name, err.Error())
			}
			if !isChannelEnabled && monitor.ShouldEnableChannel(err, openaiErr) {
				monitor.EnableChannel(channel.Id, channel.Name)
			}
			channel.UpdateResponseTime(milliseconds)
			time.Sleep(config.RequestInterval)
		}
		testAllChannelsLock.Lock()
		testAllChannelsRunning = false
		testAllChannelsLock.Unlock()
	}()
	return nil
}

func TestChannels(c *gin.Context) {
	ctx := c.Request.Context()
	scope := c.Query("scope")
	if scope == "" {
		scope = "all"
	}
	err := testChannels(ctx, true, scope)
	if err != nil {
		c.JSON(http.StatusOK, gin.H{
			"success": false,
			"message": err.Error(),
		})
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"message": "",
	})
	return
}

func AutomaticallyTestChannels(frequency int) {
	ctx := context.Background()
	for {
		time.Sleep(time.Duration(frequency) * time.Minute)
		logger.Log.Infof("testing all channels")
		_ = testChannels(ctx, false, "all")
		logger.Log.Infof("channel test finished")
	}
}
