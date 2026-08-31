package xunfei

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"

	"github.com/pai801/myapi/common"
	"github.com/pai801/myapi/common/helper"
	"github.com/pai801/myapi/common/logger"
	"github.com/pai801/myapi/common/random"
	"github.com/pai801/myapi/relay/adaptor/openai"
	"github.com/pai801/myapi/relay/constant"
	"github.com/pai801/myapi/relay/meta"
	"github.com/pai801/myapi/relay/model"
)

// https://console.xfyun.cn/services/cbm
// https://www.xfyun.cn/doc/spark/Web.html

func requestOpenAI2Xunfei(request model.GeneralOpenAIRequest, xunfeiAppId string, domain string) *ChatRequest {
	messages := make([]Message, 0, len(request.Messages))
	for _, message := range request.Messages {
		messages = append(messages, Message{
			Role:    message.Role,
			Content: message.StringContent(),
		})
	}
	xunfeiRequest := ChatRequest{}
	xunfeiRequest.Header.AppId = xunfeiAppId
	xunfeiRequest.Parameter.Chat.Domain = domain
	xunfeiRequest.Parameter.Chat.Temperature = request.Temperature
	xunfeiRequest.Parameter.Chat.TopK = request.N
	xunfeiRequest.Parameter.Chat.MaxTokens = request.MaxTokens
	xunfeiRequest.Payload.Message.Text = messages

	if strings.HasPrefix(domain, "generalv3") || domain == "4.0Ultra" {
		functions := make([]model.Function, len(request.Tools))
		for i, tool := range request.Tools {
			functions[i] = tool.Function
		}
		xunfeiRequest.Payload.Functions = &Functions{
			Text: functions,
		}
	}

	return &xunfeiRequest
}

func getToolCalls(response *ChatResponse) []model.Tool {
	var toolCalls []model.Tool
	if len(response.Payload.Choices.Text) == 0 {
		return toolCalls
	}
	item := response.Payload.Choices.Text[0]
	if item.FunctionCall == nil {
		return toolCalls
	}
	toolCall := model.Tool{
		Id:       fmt.Sprintf("call_%s", random.GetUUID()),
		Type:     "function",
		Function: *item.FunctionCall,
	}
	toolCalls = append(toolCalls, toolCall)
	return toolCalls
}

func responseXunfei2OpenAI(response *ChatResponse) *openai.TextResponse {
	if len(response.Payload.Choices.Text) == 0 {
		response.Payload.Choices.Text = []ChatResponseTextItem{
			{
				Content: "",
			},
		}
	}
	choice := openai.TextResponseChoice{
		Index: 0,
		Message: model.Message{
			Role:      "assistant",
			Content:   response.Payload.Choices.Text[0].Content,
			ToolCalls: getToolCalls(response),
		},
		FinishReason: constant.StopFinishReason,
	}
	fullTextResponse := openai.TextResponse{
		Id:      fmt.Sprintf("chatcmpl-%s", random.GetUUID()),
		Object:  "chat.completion",
		Created: helper.GetTimestamp(),
		Choices: []openai.TextResponseChoice{choice},
		Usage:   response.Payload.Usage.Text,
	}
	return &fullTextResponse
}

func streamResponseXunfei2OpenAI(xunfeiResponse *ChatResponse) *openai.ChatCompletionsStreamResponse {
	if len(xunfeiResponse.Payload.Choices.Text) == 0 {
		xunfeiResponse.Payload.Choices.Text = []ChatResponseTextItem{
			{
				Content: "",
			},
		}
	}
	var choice openai.ChatCompletionsStreamResponseChoice
	choice.Delta.Content = xunfeiResponse.Payload.Choices.Text[0].Content
	choice.Delta.ToolCalls = getToolCalls(xunfeiResponse)
	if xunfeiResponse.Payload.Choices.Status == 2 {
		choice.FinishReason = &constant.StopFinishReason
	}
	response := openai.ChatCompletionsStreamResponse{
		Id:      fmt.Sprintf("chatcmpl-%s", random.GetUUID()),
		Object:  "chat.completion.chunk",
		Created: helper.GetTimestamp(),
		Model:   "SparkDesk",
		Choices: []openai.ChatCompletionsStreamResponseChoice{choice},
	}
	return &response
}

func buildXunfeiAuthUrl(hostUrl string, apiKey, apiSecret string) string {
	HmacWithShaToBase64 := func(algorithm, data, key string) string {
		mac := hmac.New(sha256.New, []byte(key))
		mac.Write([]byte(data))
		encodeData := mac.Sum(nil)
		return base64.StdEncoding.EncodeToString(encodeData)
	}
	ul, err := url.Parse(hostUrl)
	if err != nil {
		fmt.Println(err)
	}
	date := time.Now().UTC().Format(time.RFC1123)
	signString := []string{"host: " + ul.Host, "date: " + date, "GET " + ul.Path + " HTTP/1.1"}
	sign := strings.Join(signString, "\n")
	sha := HmacWithShaToBase64("hmac-sha256", sign, apiSecret)
	authUrl := fmt.Sprintf("hmac username=\"%s\", algorithm=\"%s\", headers=\"%s\", signature=\"%s\"", apiKey,
		"hmac-sha256", "host date request-line", sha)
	authorization := base64.StdEncoding.EncodeToString([]byte(authUrl))
	v := url.Values{}
	v.Add("host", ul.Host)
	v.Add("date", date)
	v.Add("authorization", authorization)
	callUrl := hostUrl + "?" + v.Encode()
	return callUrl
}

// 形参序与渠道密钥填写顺序(APPID|APIKey|APISecret 官方惯例)对齐，修复原先 username/签名密钥取参对调导致的 401
func StreamHandler(c *gin.Context, meta *meta.Meta, textRequest model.GeneralOpenAIRequest, appId string, apiKey string, apiSecret string) (*model.ErrorWithStatusCode, *model.Usage) {
	domain, authUrl := getXunfeiAuthUrl(meta.Config.APIVersion, apiKey, apiSecret)
	dataChan, stopChan, err := xunfeiMakeRequest(textRequest, domain, authUrl, appId, c.Request.Context())
	if err != nil {
		logger.Log.Errorf("[%s] %+v", "xunfei_request_failed", err)
		return openai.ErrorWrapper(err, "xunfei_request_failed", http.StatusInternalServerError), nil
	}
	common.SetEventStreamHeaders(c)
	var usage model.Usage
	c.Stream(func(w io.Writer) bool {
		select {
		case xunfeiResponse := <-dataChan:
			usage.PromptTokens += xunfeiResponse.Payload.Usage.Text.PromptTokens
			usage.CompletionTokens += xunfeiResponse.Payload.Usage.Text.CompletionTokens
			usage.TotalTokens += xunfeiResponse.Payload.Usage.Text.TotalTokens
			response := streamResponseXunfei2OpenAI(&xunfeiResponse)
			jsonResponse, err := json.Marshal(response)
			if err != nil {
				logger.Log.Errorf("error marshalling stream response: " + err.Error())
				return true
			}
			c.Render(-1, common.CustomEvent{Data: "data: " + string(jsonResponse)})
			return true
		case <-stopChan:
			c.Render(-1, common.CustomEvent{Data: "data: [DONE]"})
			return false
		}
	})
	return nil, &usage
}

// 形参序与渠道密钥填写顺序(APPID|APIKey|APISecret 官方惯例)对齐，修复原先 username/签名密钥取参对调导致的 401
func Handler(c *gin.Context, meta *meta.Meta, textRequest model.GeneralOpenAIRequest, appId string, apiKey string, apiSecret string) (*model.ErrorWithStatusCode, *model.Usage) {
	domain, authUrl := getXunfeiAuthUrl(meta.Config.APIVersion, apiKey, apiSecret)
	dataChan, stopChan, err := xunfeiMakeRequest(textRequest, domain, authUrl, appId, c.Request.Context())
	if err != nil {
		logger.Log.Errorf("[%s] %+v", "xunfei_request_failed", err)
		return openai.ErrorWrapper(err, "xunfei_request_failed", http.StatusInternalServerError), nil
	}
	var usage model.Usage
	var content string
	var xunfeiResponse ChatResponse
	stop := false
	for !stop {
		select {
		case xunfeiResponse = <-dataChan:
			if len(xunfeiResponse.Payload.Choices.Text) == 0 {
				continue
			}
			content += xunfeiResponse.Payload.Choices.Text[0].Content
			usage.PromptTokens += xunfeiResponse.Payload.Usage.Text.PromptTokens
			usage.CompletionTokens += xunfeiResponse.Payload.Usage.Text.CompletionTokens
			usage.TotalTokens += xunfeiResponse.Payload.Usage.Text.TotalTokens
		case stop = <-stopChan:
		}
	}
	if len(xunfeiResponse.Payload.Choices.Text) == 0 {
		logger.Log.Errorf("[%s] %+v", "xunfei_empty_response_detected", errors.New("xunfei empty response detected"))
		return openai.ErrorWrapper(errors.New("xunfei empty response detected"), "xunfei_empty_response_detected", http.StatusInternalServerError), nil
	}
	xunfeiResponse.Payload.Choices.Text[0].Content = content

	response := responseXunfei2OpenAI(&xunfeiResponse)
	jsonResponse, err := json.Marshal(response)
	if err != nil {
		logger.Log.Errorf("[%s] %+v", "marshal_response_body_failed", err)
		return openai.ErrorWrapper(err, "marshal_response_body_failed", http.StatusInternalServerError), nil
	}
	c.Writer.Header().Set("Content-Type", "application/json")
	_, _ = c.Writer.Write(jsonResponse)
	return nil, &usage
}

func xunfeiMakeRequest(textRequest model.GeneralOpenAIRequest, domain, authUrl, appId string, ctx context.Context) (chan ChatResponse, chan bool, error) {
	d := websocket.Dialer{
		HandshakeTimeout: 5 * time.Second,
	}
	conn, resp, err := d.Dial(authUrl, nil)
	if err != nil || resp.StatusCode != 101 {
		// gorilla/websocket 握手失败时 resp 非 nil，必须关闭 body 释放底层连接，否则每次失败（如 401）泄漏一条 TLS 连接
		if resp != nil {
			_ = resp.Body.Close()
		}
		return nil, nil, err
	}
	// 监控请求 ctx：客户端断开时主动关闭底层连接，解除阻塞中的读/写，防止 goroutine 与连接泄漏。
	// 必须在首个 conn 读写之前启动，否则上游迟迟不发首帧时 ctx 取消无法解除阻塞
	monitorDone := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			_ = conn.Close()
		case <-monitorDone:
		}
	}()

	data := requestOpenAI2Xunfei(textRequest, appId, domain)
	err = conn.WriteJSON(data)
	if err != nil {
		close(monitorDone)
		_ = conn.Close()
		return nil, nil, err
	}
	_, msg, err := conn.ReadMessage()
	if err != nil {
		close(monitorDone)
		_ = conn.Close()
		return nil, nil, err
	}

	// dataChan 无缓冲：正常路径逐帧握手，保证末帧先于 stop 被消费，避免缓冲引入的末帧丢失竞争；
	// 客户端断开（消费方不再读）时由 ctx 取消解除生产者阻塞，泄漏防护不回退
	dataChan := make(chan ChatResponse)
	stopChan := make(chan bool, 1)
	go func() {
		defer func() {
			close(monitorDone)
			// ctx 取消路径 monitor 可能已关闭 conn，net.ErrClosed 属预期，降噪不打
			if closeErr := conn.Close(); closeErr != nil && !errors.Is(closeErr, net.ErrClosed) {
				logger.Log.Errorf("error closing websocket connection: " + closeErr.Error())
			}
			// stopChan 带缓冲：消费方已退出时投递不再永久阻塞
			stopChan <- true
		}()
		for {
			if msg == nil {
				_, msg, err = conn.ReadMessage()
				if err != nil {
					logger.Log.Errorf("error reading stream response: " + err.Error())
					break
				}
			}
			var response ChatResponse
			err = json.Unmarshal(msg, &response)
			if err != nil {
				logger.Log.Errorf("error unmarshalling stream response: " + err.Error())
				break
			}
			msg = nil
			select {
			case dataChan <- response:
			case <-ctx.Done():
				// 客户端断开（消费方不再读），退出由 defer 关闭连接并投递 stop
				return
			}
			if response.Payload.Choices.Status == 2 {
				break
			}
		}
	}()

	return dataChan, stopChan, nil
}
