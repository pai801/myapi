package replicate

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/pkg/errors"
	"github.com/pai801/myapi/common"
	"github.com/pai801/myapi/common/logger"
	"github.com/pai801/myapi/common/render"
	"github.com/pai801/myapi/relay/adaptor/openai"
	"github.com/pai801/myapi/relay/constant"
	"github.com/pai801/myapi/relay/meta"
	"github.com/pai801/myapi/relay/model"
)

func ChatHandler(c *gin.Context, resp *http.Response) (
	srvErr *model.ErrorWithStatusCode, usage *model.Usage) {
	if resp.StatusCode != http.StatusCreated {
		payload, _ := io.ReadAll(resp.Body)
		logger.Log.Errorf("[%s] %+v", "bad_status_code", errors.Errorf("bad_status_code [%d]%s", resp.StatusCode, string(payload)))
		return openai.ErrorWrapper(
				errors.Errorf("bad_status_code [%d]%s", resp.StatusCode, string(payload)),
				"bad_status_code", http.StatusInternalServerError),
			nil
	}

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		logger.Log.Errorf("[%s] %+v", "read_response_body_failed", err)
		return openai.ErrorWrapper(err, "read_response_body_failed", http.StatusInternalServerError), nil
	}

	respData := new(ChatResponse)
	if err = json.Unmarshal(respBody, respData); err != nil {
		logger.Log.Errorf("[%s] %+v", "unmarshal_response_body_failed", err)
		return openai.ErrorWrapper(err, "unmarshal_response_body_failed", http.StatusInternalServerError), nil
	}

	pollDeadline := time.Now().Add(taskPollTimeout)
	for {
		err = func() error {
			if time.Now().After(pollDeadline) {
				return errors.Errorf("replicate task polling timeout after %s", taskPollTimeout)
			}
			// get task（单次请求绑定超时，防止挂死的 GET 无限占用 goroutine）
			reqCtx, cancel := pollRequestContext(c.Request.Context(), pollDeadline)
			defer cancel()
			taskReq, err := http.NewRequestWithContext(reqCtx,
				http.MethodGet, respData.URLs.Get, nil)
			if err != nil {
				return errors.Wrap(err, "new request")
			}

			taskReq.Header.Set("Authorization", "Bearer "+meta.GetByContext(c).APIKey)
			taskResp, err := http.DefaultClient.Do(taskReq)
			if err != nil {
				return errors.Wrap(err, "get task")
			}
			defer taskResp.Body.Close()

			if taskResp.StatusCode != http.StatusOK {
				payload, _ := io.ReadAll(taskResp.Body)
				return errors.Errorf("bad status code [%d]%s",
					taskResp.StatusCode, string(payload))
			}

			taskBody, err := io.ReadAll(taskResp.Body)
			if err != nil {
				return errors.Wrap(err, "read task response")
			}

			taskData := new(ChatResponse)
			if err = json.Unmarshal(taskBody, taskData); err != nil {
				return errors.Wrap(err, "decode task response")
			}

			switch taskData.Status {
			case "succeeded":
			case "failed", "canceled":
				return errors.Errorf("task failed, [%s]%s", taskData.Status, taskData.Error)
			default:
				time.Sleep(time.Second * 3)
				return errNextLoop
			}

			if taskData.URLs.Stream == "" {
				return errors.New("stream url is empty")
			}

			// request stream url
			responseText, err := chatStreamHandler(c, pollDeadline, taskData.URLs.Stream)
			if err != nil {
				return errors.Wrap(err, "chat stream handler")
			}

			ctxMeta := meta.GetByContext(c)
			usage = openai.ResponseText2Usage(responseText,
				ctxMeta.ActualModelName, ctxMeta.PromptTokens)
			return nil
		}()
		if err != nil {
			if errors.Is(err, errNextLoop) {
				continue
			}

			logger.Log.Errorf("[%s] %+v", "chat_task_failed", err)
			return openai.ErrorWrapper(err, "chat_task_failed", http.StatusInternalServerError), nil
		}

		break
	}

	return nil, usage
}

const (
	eventPrefix = "event: "
	dataPrefix  = "data: "
	done        = "[DONE]"
)

func chatStreamHandler(c *gin.Context, pollDeadline time.Time, streamUrl string) (responseText string, err error) {
	// request stream endpoint
	// 流式响应合法时长可能超过单请求 30s 上限，仅绑定轮询总 deadline，防挂死连接无限占用 goroutine
	streamCtx, cancel := context.WithTimeout(c.Request.Context(), time.Until(pollDeadline))
	defer cancel()
	streamReq, err := http.NewRequestWithContext(streamCtx, http.MethodGet, streamUrl, nil)
	if err != nil {
		return "", errors.Wrap(err, "new request to stream")
	}

	streamReq.Header.Set("Authorization", "Bearer "+meta.GetByContext(c).APIKey)
	streamReq.Header.Set("Accept", "text/event-stream")
	streamReq.Header.Set("Cache-Control", "no-store")

	resp, err := http.DefaultClient.Do(streamReq)
	if err != nil {
		return "", errors.Wrap(err, "do request to stream")
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		payload, _ := io.ReadAll(resp.Body)
		return "", errors.Errorf("bad status code [%d]%s", resp.StatusCode, string(payload))
	}

	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, constant.ScannerBufferInitial), constant.ScannerBufferMax)
	scanner.Split(bufio.ScanLines)

	common.SetEventStreamHeaders(c)
	doneRendered := false
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}

		// Handle comments starting with ':'
		if strings.HasPrefix(line, ":") {
			continue
		}

		// Parse SSE fields
		if strings.HasPrefix(line, eventPrefix) {
			event := strings.TrimSpace(line[len(eventPrefix):])
			var data string
			// Read the following lines to get data and id
			for scanner.Scan() {
				nextLine := scanner.Text()
				if nextLine == "" {
					break
				}
				if strings.HasPrefix(nextLine, dataPrefix) {
					data = nextLine[len(dataPrefix):]
				} else if strings.HasPrefix(nextLine, "id:") {
					// id = strings.TrimSpace(nextLine[len("id:"):])
				}
			}

			if event == "output" {
				render.StringData(c, data)
				responseText += data
			} else if event == "done" {
				render.Done(c)
				doneRendered = true
				break
			}
		}
	}

	if err := scanner.Err(); err != nil {
		return "", errors.Wrap(err, "scan stream")
	}

	if !doneRendered {
		render.Done(c)
	}

	return responseText, nil
}
