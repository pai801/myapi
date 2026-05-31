package codex

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/songquanpeng/one-api/common"
	"github.com/songquanpeng/one-api/common/ctxkey"
	"github.com/songquanpeng/one-api/common/logger"
	"github.com/songquanpeng/one-api/common/render"
	"github.com/songquanpeng/one-api/relay/meta"
	"github.com/songquanpeng/one-api/relay/model"
)

const (
	dataPrefix       = "data: "
	done             = "[DONE]"
	dataPrefixLength = len(dataPrefix)
)

var ModelList = []string{
	"gpt-4o",
	"gpt-4o-mini",
	"gpt-4-turbo",
	"gpt-4",
	"gpt-3.5-turbo",
	"o1",
	"o1-mini",
}

func DoResponsesResponse(c *gin.Context, resp *http.Response, meta *meta.Meta) (*model.Usage, *model.ErrorWithStatusCode) {
	var textResponse model.ResponsesResponse
	ctx := c.Request.Context()
	responseBody, err := io.ReadAll(resp.Body)
	if err != nil {
		logger.Errorf(ctx, "[%s] %+v", "read_response_body_failed", err)
		return nil, ErrorWrapper(err, "read_response_body_failed", http.StatusInternalServerError)
	}
	err = resp.Body.Close()
	if err != nil {
		logger.Errorf(ctx, "[%s] %+v", "close_response_body_failed", err)
		return nil, ErrorWrapper(err, "close_response_body_failed", http.StatusInternalServerError)
	}

	err = json.Unmarshal(responseBody, &textResponse)
	if err != nil {
		logger.Errorf(ctx, "[%s] %+v", "invalid_json_response", err)
		return nil, ErrorWrapper(err, "invalid_json_response", http.StatusInternalServerError)
	}

	resp.Body = io.NopCloser(bytes.NewBuffer(responseBody))

	for k, v := range resp.Header {
		for _, vv := range v {
			c.Writer.Header().Add(k, vv)
		}
	}
	c.Writer.WriteHeader(resp.StatusCode)
	_, err = io.Copy(c.Writer, resp.Body)
	if err != nil {
		logger.Errorf(ctx, "[%s] %+v", "copy_response_body_failed", err)
		return nil, ErrorWrapper(err, "copy_response_body_failed", http.StatusInternalServerError)
	}
	err = resp.Body.Close()
	if err != nil {
		logger.Errorf(ctx, "[%s] %+v", "close_response_body_failed", err)
		return nil, ErrorWrapper(err, "close_response_body_failed", http.StatusInternalServerError)
	}

	usage := &model.Usage{
		PromptTokens:     textResponse.Usage.InputTokens,
		CompletionTokens: textResponse.Usage.OutputTokens,
		TotalTokens:      textResponse.Usage.TotalTokens,
	}

	c.Set(ctxkey.ResponseBody, string(responseBody))
	return usage, nil
}

func StreamResponsesHandler(c *gin.Context, resp *http.Response) (*model.ErrorWithStatusCode, string, *model.Usage) {
	responseText := ""
	scanner := bufio.NewScanner(resp.Body)
	scanner.Split(bufio.ScanLines)
	var usage *model.Usage

	common.SetEventStreamHeaders(c)

	doneRendered := false
	for scanner.Scan() {
		data := scanner.Text()
		if len(data) < dataPrefixLength {
			continue
		}
		if data[:dataPrefixLength] != dataPrefix {
			continue
		}
		if strings.HasPrefix(data[dataPrefixLength:], done) {
			render.StringData(c, data)
			doneRendered = true
			continue
		}

		var streamResponse model.ResponsesStreamEvent
		err := json.Unmarshal([]byte(data[dataPrefixLength:]), &streamResponse)
		if err != nil {
			logger.SysError("error unmarshalling stream response: " + err.Error())
			render.StringData(c, data)
			continue
		}
		render.StringData(c, data)

		if streamResponse.Delta != nil {
			switch d := streamResponse.Delta.(type) {
			case string:
				responseText += d
			case map[string]interface{}:
				if content, ok := d["content"].(string); ok {
					responseText += content
				}
			}
		}

		if streamResponse.Usage != nil {
			usage = &model.Usage{
				PromptTokens:     streamResponse.Usage.InputTokens,
				CompletionTokens: streamResponse.Usage.OutputTokens,
				TotalTokens:      streamResponse.Usage.TotalTokens,
			}
		}

		if streamResponse.Response != nil && streamResponse.Response.Usage.TotalTokens > 0 {
			usage = &model.Usage{
				PromptTokens:     streamResponse.Response.Usage.InputTokens,
				CompletionTokens: streamResponse.Response.Usage.OutputTokens,
				TotalTokens:      streamResponse.Response.Usage.TotalTokens,
			}
		}
	}

	if err := scanner.Err(); err != nil {
		logger.SysError("error reading stream: " + err.Error())
	}

	if !doneRendered {
		render.Done(c)
	}

	err := resp.Body.Close()
	if err != nil {
		logger.Errorf(c.Request.Context(), "[%s] %+v", "close_response_body_failed", err)
		return ErrorWrapper(err, "close_response_body_failed", http.StatusInternalServerError), "", nil
	}

	return nil, responseText, usage
}

func ErrorWrapper(err error, code string, statusCode int) *model.ErrorWithStatusCode {
	return &model.ErrorWithStatusCode{
		Error: model.Error{
			Message: err.Error(),
			Type:    "one_api_error",
			Param:   "",
			Code:    code,
		},
		StatusCode: statusCode,
	}
}
