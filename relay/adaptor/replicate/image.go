package replicate

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"image"
	"image/png"
	"io"
	"net/http"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/pkg/errors"
	"github.com/pai801/myapi/common/logger"
	"github.com/pai801/myapi/relay/adaptor/openai"
	"github.com/pai801/myapi/relay/meta"
	"github.com/pai801/myapi/relay/model"
	"golang.org/x/image/webp"
	"golang.org/x/sync/errgroup"
)

// ImagesEditsHandler just copy response body to client
//
// https://replicate.com/black-forest-labs/flux-fill-pro
// func ImagesEditsHandler(c *gin.Context, resp *http.Response) (*model.ErrorWithStatusCode, *model.Usage) {
// 	c.Writer.WriteHeader(resp.StatusCode)
// 	for k, v := range resp.Header {
// 		c.Writer.Header().Set(k, v[0])
// 	}

// 	if _, err := io.Copy(c.Writer, resp.Body); err != nil {
// 		return ErrorWrapper(err, "copy_response_body_failed", http.StatusInternalServerError), nil
// 	}
// 	defer resp.Body.Close()

// 	return nil, nil
// }

var errNextLoop = errors.New("next_loop")

// 上游任务可能长期停留在 running 状态，轮询总时限防止请求 goroutine 无限占用；
// 生图任务常规数十秒到数分钟，5 分钟可覆盖慢任务且限制最坏等待
const taskPollTimeout = 5 * time.Minute

// singlePollRequestTimeout 单次轮询/下载请求的超时上限，防止个别请求挂死导致 goroutine 无限占用
const singlePollRequestTimeout = 30 * time.Second

// pollRequestContext 为轮询单次请求派生带超时的 context，
// 超时取 min(30s, 距轮询总 deadline 剩余)：既防单请求挂死，也保证不越过 taskPollTimeout 总时限
func pollRequestContext(parent context.Context, pollDeadline time.Time) (context.Context, context.CancelFunc) {
	timeout := singlePollRequestTimeout
	if remaining := time.Until(pollDeadline); remaining < timeout {
		timeout = remaining
	}
	return context.WithTimeout(parent, timeout)
}

func ImageHandler(c *gin.Context, resp *http.Response) (*model.ErrorWithStatusCode, *model.Usage) {
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

	respData := new(ImageResponse)
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

			taskData := new(ImageResponse)
			if err = json.Unmarshal(taskBody, taskData); err != nil {
				return errors.Wrap(err, "decode task response")
			}

			switch taskData.Status {
			case "succeeded":
			case "failed", "canceled":
				return errors.Errorf("task failed: %s", taskData.Status)
			default:
				time.Sleep(time.Second * 3)
				return errNextLoop
			}

			output, err := taskData.GetOutput()
			if err != nil {
				return errors.Wrap(err, "get output")
			}
			if len(output) == 0 {
				return errors.New("response output is empty")
			}

			var mu sync.Mutex
			var pool errgroup.Group
			respBody := &openai.ImageResponse{
				Created: taskData.CompletedAt.Unix(),
				Data:    []openai.ImageData{},
			}

			for _, imgOut := range output {
				imgOut := imgOut
				pool.Go(func() error {
					// download image（单次下载绑定超时，防止挂死的下载无限占用 goroutine）
					dlCtx, cancel := pollRequestContext(c.Request.Context(), pollDeadline)
					defer cancel()
					downloadReq, err := http.NewRequestWithContext(dlCtx,
						http.MethodGet, imgOut, nil)
					if err != nil {
						return errors.Wrap(err, "new request")
					}

					imgResp, err := http.DefaultClient.Do(downloadReq)
					if err != nil {
						return errors.Wrap(err, "download image")
					}
					defer imgResp.Body.Close()

					if imgResp.StatusCode != http.StatusOK {
						payload, _ := io.ReadAll(imgResp.Body)
						return errors.Errorf("bad status code [%d]%s",
							imgResp.StatusCode, string(payload))
					}

					imgData, err := io.ReadAll(imgResp.Body)
					if err != nil {
						return errors.Wrap(err, "read image")
					}

					imgData, err = ConvertImageToPNG(imgData)
					if err != nil {
						return errors.Wrap(err, "convert image")
					}

					mu.Lock()
					respBody.Data = append(respBody.Data, openai.ImageData{
						B64Json: fmt.Sprintf("data:image/png;base64,%s",
							base64.StdEncoding.EncodeToString(imgData)),
					})
					mu.Unlock()

					return nil
				})
			}

			if err := pool.Wait(); err != nil {
				if len(respBody.Data) == 0 {
					return errors.WithStack(err)
				}

				logger.Log.Errorf("some images failed to download: %+v", err)
			}

			c.JSON(http.StatusOK, respBody)
			return nil
		}()
		if err != nil {
			if errors.Is(err, errNextLoop) {
				continue
			}

			logger.Log.Errorf("[%s] %+v", "image_task_failed", err)
			return openai.ErrorWrapper(err, "image_task_failed", http.StatusInternalServerError), nil
		}

		break
	}

	return nil, nil
}

// ConvertImageToPNG converts a WebP image to PNG format
func ConvertImageToPNG(webpData []byte) ([]byte, error) {
	// bypass if it's already a PNG image
	if bytes.HasPrefix(webpData, []byte("\x89PNG")) {
		return webpData, nil
	}

	// check if is jpeg, convert to png
	if bytes.HasPrefix(webpData, []byte("\xff\xd8\xff")) {
		img, _, err := image.Decode(bytes.NewReader(webpData))
		if err != nil {
			return nil, errors.Wrap(err, "decode jpeg")
		}

		var pngBuffer bytes.Buffer
		if err := png.Encode(&pngBuffer, img); err != nil {
			return nil, errors.Wrap(err, "encode png")
		}

		return pngBuffer.Bytes(), nil
	}

	// Decode the WebP image
	img, err := webp.Decode(bytes.NewReader(webpData))
	if err != nil {
		return nil, errors.Wrap(err, "decode webp")
	}

	// Encode the image as PNG
	var pngBuffer bytes.Buffer
	if err := png.Encode(&pngBuffer, img); err != nil {
		return nil, errors.Wrap(err, "encode png")
	}

	return pngBuffer.Bytes(), nil
}
