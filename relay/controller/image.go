package controller

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/pai801/myapi/common"
	"github.com/pai801/myapi/common/ctxkey"
	"github.com/pai801/myapi/common/logger"
	"github.com/pai801/myapi/model"
	"github.com/pai801/myapi/relay"
	"github.com/pai801/myapi/relay/adaptor/openai"
	billingratio "github.com/pai801/myapi/relay/billing/ratio"
	"github.com/pai801/myapi/relay/channeltype"
	"github.com/pai801/myapi/relay/meta"
	relaymodel "github.com/pai801/myapi/relay/model"
)

func getImageRequest(c *gin.Context, _ int) (*relaymodel.ImageRequest, error) {
	imageRequest := &relaymodel.ImageRequest{}
	err := common.UnmarshalBodyReusable(c, imageRequest)
	if err != nil {
		return nil, err
	}
	if imageRequest.N == 0 {
		imageRequest.N = 1
	}
	if imageRequest.Size == "" {
		imageRequest.Size = "1024x1024"
	}
	if imageRequest.Model == "" {
		imageRequest.Model = "dall-e-2"
	}
	return imageRequest, nil
}

func isValidImageSize(model string, size string) bool {
	if model == "cogview-3" || billingratio.ImageSizeRatios[model] == nil {
		return true
	}
	_, ok := billingratio.ImageSizeRatios[model][size]
	return ok
}

func isValidImagePromptLength(model string, promptLength int) bool {
	maxPromptLength, ok := billingratio.ImagePromptLengthLimitations[model]
	return !ok || promptLength <= maxPromptLength
}

func isWithinRange(element string, value int) bool {
	amounts, ok := billingratio.ImageGenerationAmounts[element]
	return !ok || (value >= amounts[0] && value <= amounts[1])
}

func getImageSizeRatio(model string, size string) float64 {
	if ratio, ok := billingratio.ImageSizeRatios[model][size]; ok {
		return ratio
	}
	return 1
}

func validateImageRequest(imageRequest *relaymodel.ImageRequest, _ *meta.Meta) *relaymodel.ErrorWithStatusCode {
	// check prompt length
	if imageRequest.Prompt == "" {
		return openai.ErrorWrapper(errors.New("prompt is required"), "prompt_missing", http.StatusBadRequest)
	}

	// model validation
	if !isValidImageSize(imageRequest.Model, imageRequest.Size) {
		return openai.ErrorWrapper(errors.New("size not supported for this image model"), "size_not_supported", http.StatusBadRequest)
	}

	if !isValidImagePromptLength(imageRequest.Model, len(imageRequest.Prompt)) {
		return openai.ErrorWrapper(errors.New("prompt is too long"), "prompt_too_long", http.StatusBadRequest)
	}

	// Number of generated images validation
	if !isWithinRange(imageRequest.Model, imageRequest.N) {
		return openai.ErrorWrapper(errors.New("invalid value of n"), "n_not_within_range", http.StatusBadRequest)
	}
	return nil
}

func getImageCostRatio(imageRequest *relaymodel.ImageRequest) (float64, error) {
	if imageRequest == nil {
		return 0, errors.New("imageRequest is nil")
	}
	imageCostRatio := getImageSizeRatio(imageRequest.Model, imageRequest.Size)
	if imageRequest.Quality == "hd" && imageRequest.Model == "dall-e-3" {
		if imageRequest.Size == "1024x1024" {
			imageCostRatio *= 2
		} else {
			imageCostRatio *= 1.5
		}
	}
	return imageCostRatio, nil
}

func RelayImageHelper(c *gin.Context, relayMode int) *relaymodel.ErrorWithStatusCode {
	ctx := c.Request.Context()
	meta := meta.GetByContext(c)
	imageRequest, err := getImageRequest(c, meta.Mode)
	if err != nil {
		logger.Log.Errorf("[%s] %+v", "invalid_image_request", err)
		return openai.ErrorWrapper(err, "invalid_image_request", http.StatusBadRequest)
	}

	// map model name
	var isModelMapped bool
	meta.OriginModelName = imageRequest.Model
	imageRequest.Model, isModelMapped = getMappedModelName(imageRequest.Model, meta.ModelMapping)
	meta.ActualModelName = imageRequest.Model

	// model validation
	bizErr := validateImageRequest(imageRequest, meta)
	if bizErr != nil {
		return bizErr
	}

	imageCostRatio, err := getImageCostRatio(imageRequest)
	if err != nil {
		logger.Log.Errorf("[%s] %+v", "get_image_cost_ratio_failed", err)
		return openai.ErrorWrapper(err, "get_image_cost_ratio_failed", http.StatusInternalServerError)
	}

	imageModel := imageRequest.Model
	// Convert the original image model
	imageRequest.Model, _ = getMappedModelName(imageRequest.Model, billingratio.ImageOriginModelName)
	c.Set("response_format", imageRequest.ResponseFormat)

	var requestBody io.Reader
	if isModelMapped || meta.ChannelType == channeltype.Azure { // make Azure channel request body
		jsonStr, err := json.Marshal(imageRequest)
		if err != nil {
			logger.Log.Errorf("[%s] %+v", "marshal_image_request_failed", err)
			return openai.ErrorWrapper(err, "marshal_image_request_failed", http.StatusInternalServerError)
		}
		requestBody = bytes.NewBuffer(jsonStr)
	} else {
		requestBody = c.Request.Body
	}

	adaptor := relay.GetAdaptor(meta.APIType)
	if adaptor == nil {
		logger.Log.Errorf("[%s] %+v", "invalid_api_type", fmt.Errorf("invalid api type: %d", meta.APIType))
		return openai.ErrorWrapper(fmt.Errorf("invalid api type: %d", meta.APIType), "invalid_api_type", http.StatusBadRequest)
	}
	adaptor.Init(meta)

	// these adaptors need to convert the request
	switch meta.ChannelType {
	case channeltype.Zhipu,
		channeltype.Ali,
		channeltype.Replicate,
		channeltype.Baidu:
		finalRequest, err := adaptor.ConvertImageRequest(imageRequest)
		if err != nil {
			logger.Log.Errorf("[%s] %+v", "convert_image_request_failed", err)
			return openai.ErrorWrapper(err, "convert_image_request_failed", http.StatusInternalServerError)
		}
		jsonStr, err := json.Marshal(finalRequest)
		if err != nil {
			logger.Log.Errorf("[%s] %+v", "marshal_image_request_failed", err)
			return openai.ErrorWrapper(err, "marshal_image_request_failed", http.StatusInternalServerError)
		}
		requestBody = bytes.NewBuffer(jsonStr)
	}

	modelRatio := billingratio.GetModelRatio(imageModel, meta.ChannelType)
	groupRatio := model.GetGroupModelRatio(meta.Group)
	ratio := modelRatio * groupRatio
	userQuota, err := model.CacheGetUserQuota(ctx, meta.UserId)
	if err != nil {
		// DB/Redis 抖动时必须 5xx 拒绝而非用 0 余额误报 403
		logger.Log.Errorf("[%s] %+v", "get_user_quota_failed", err)
		return openai.ErrorWrapper(err, "get_user_quota_failed", http.StatusInternalServerError)
	}

	var quota int64
	switch meta.ChannelType {
	case channeltype.Replicate:
		// replicate always return 1 image
		quota = int64(ratio * imageCostRatio * 1000)
	default:
		quota = int64(ratio*imageCostRatio*1000) * int64(imageRequest.N)
	}

	if userQuota < quota {
		logger.Log.Errorf("[%s] %+v", "insufficient_user_quota", errors.New("user quota is not enough"))
		return openai.ErrorWrapper(errors.New("user quota is not enough"), "insufficient_user_quota", http.StatusForbidden)
	}

	// Pre-consume to close race window between check and actual consumption
	if err := model.DecreaseUserQuota(meta.UserId, quota); err != nil {
		logger.Log.Errorf("pre-consume quota failed for user %d: %v", meta.UserId, err)
		return openai.ErrorWrapper(err, "pre_consume_quota_failed", http.StatusInternalServerError)
	}
	ctx = context.WithValue(ctx, CtxKeyPreConsumedQuota, quota)

	// do request
	resp, err := adaptor.DoRequest(c, meta, requestBody)
	if err != nil {
		rollbackImagePreConsumedQuota(ctx, meta.UserId)
		logger.Log.Errorf("[%s] %+v", "do_request_failed", err)
		return openai.ErrorWrapper(err, "do_request_failed", http.StatusInternalServerError)
	}

	// do response
	_, respErr := adaptor.DoResponse(c, resp, meta)
	if respErr != nil {
		rollbackImagePreConsumedQuota(ctx, meta.UserId)
		logger.Log.Errorf("respErr is not nil: %+v", respErr)
		return respErr
	}

	// ImageHandler 不校验上游状态码（错误体原样透传给客户端）。三路回滚/结算语义：
	// DoRequest 失败、DoResponse 出错、上游非 2xx（含 resp 为 nil）均回滚预扣且不结算，仅 2xx 成功路径结算。
	// 与旧版 defer 的豁免语义存在差异：上游已返回 2xx 但 DoResponse 出错时，旧版仍扣费、新版回滚——
	// 此时上游已产生成本，但用户未收到产物，故向用户退款
	if resp == nil || (resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated) {
		rollbackImagePreConsumedQuota(ctx, meta.UserId)
		return nil
	}

	// 结算：图片按 N/size 定价，预扣额即最终扣费，无 usage 多退少补
	model.PostConsumeResetUserQuotaCache(ctx, meta.UserId, quota)
	if quota != 0 {
		tokenName := c.GetString(ctxkey.TokenName)
		logContent := fmt.Sprintf("倍率：%.2f", modelRatio)
		model.RecordConsumeLog(ctx, &model.Log{
			UserId:           meta.UserId,
			ChannelId:        meta.ChannelId,
			PromptTokens:     0,
			CompletionTokens: 0,
			ModelName:        imageRequest.Model,
			TokenName:        tokenName,
			Quota:            int(quota),
			Content:          logContent,
		})
		model.UpdateUserUsedQuotaAndRequestCount(meta.UserId, quota)
		channelId := c.GetInt(ctxkey.ChannelId)
		model.UpdateChannelUsedQuota(channelId, quota)
	}

	return nil
}

// rollbackImagePreConsumedQuota 回滚图片请求的预扣额度并刷新额度缓存
func rollbackImagePreConsumedQuota(ctx context.Context, userId int) {
	if preConsumed, ok := ctx.Value(CtxKeyPreConsumedQuota).(int64); ok && preConsumed > 0 {
		if err := model.IncreaseUserQuota(userId, preConsumed); err != nil {
			logger.Log.Errorf("error rolling back pre-consumed image quota: " + err.Error())
		}
		model.PostConsumeResetUserQuotaCache(ctx, userId, preConsumed)
	}
}
