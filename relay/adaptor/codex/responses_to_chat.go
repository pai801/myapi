package codex

import (
	"encoding/json"
	"strings"
)

func ConvertResponsesToChatRequest(modelName string, inputRawJSON []byte, stream bool) []byte {
	var req map[string]interface{}
	if err := json.Unmarshal(inputRawJSON, &req); err != nil {
		return inputRawJSON
	}

	chatReq := map[string]interface{}{
		"model":    modelName,
		"messages": []interface{}{},
		"stream":   stream,
	}

	if stream {
		chatReq["stream_options"] = map[string]interface{}{
			"include_usage": true,
		}
	}

	if v, ok := req["max_output_tokens"].(float64); ok {
		chatReq["max_tokens"] = int(v)
	}
	if v, ok := req["temperature"].(float64); ok {
		chatReq["temperature"] = v
	}
	if v, ok := req["top_p"].(float64); ok {
		chatReq["top_p"] = v
	}
	if v, ok := req["user"].(string); ok {
		chatReq["user"] = v
	}

	if instructions, ok := req["instructions"].(string); ok && instructions != "" {
		messages := chatReq["messages"].([]interface{})
		messages = append(messages, map[string]interface{}{
			"role":    "system",
			"content": instructions,
		})
		chatReq["messages"] = messages
	}

	if input, ok := req["input"]; ok {
		messages := chatReq["messages"].([]interface{})
		inputMessages := convertInputToMessages(input)
		chatReq["messages"] = append(messages, inputMessages...)
	}

	if tools, ok := req["tools"].([]interface{}); ok {
		chatReq["tools"] = convertToolsToOpenAI(tools)
	}

	if v, ok := req["tool_choice"]; ok {
		if _, hasTools := req["tools"]; hasTools {
			chatReq["tool_choice"] = v
		}
	}

	if v, ok := req["parallel_tool_calls"].(bool); ok {
		if _, hasTools := req["tools"]; hasTools {
			chatReq["parallel_tool_calls"] = v
		}
	}

	if reasoning, ok := req["reasoning"].(map[string]interface{}); ok {
		if effort, ok := reasoning["effort"].(string); ok {
			switch effort {
			case "none":
				chatReq["reasoning_effort"] = "none"
			case "auto":
				chatReq["reasoning_effort"] = "auto"
			case "minimal":
				chatReq["reasoning_effort"] = "low"
			case "low":
				chatReq["reasoning_effort"] = "low"
			case "medium":
				chatReq["reasoning_effort"] = "medium"
			case "high":
				chatReq["reasoning_effort"] = "high"
			case "xhigh":
				chatReq["reasoning_effort"] = "xhigh"
			default:
				chatReq["reasoning_effort"] = "auto"
			}
		}
	}

	result, _ := json.Marshal(chatReq)
	return result
}

func convertInputToMessages(input interface{}) []interface{} {
	var messages []interface{}

	switch v := input.(type) {
	case string:
		messages = append(messages, map[string]interface{}{
			"role":    "user",
			"content": v,
		})
	case []interface{}:
		for _, item := range v {
			if itemMap, ok := item.(map[string]interface{}); ok {
				messages = append(messages, convertInputItem(itemMap))
			}
		}
	}

	return messages
}

func convertInputItem(item map[string]interface{}) map[string]interface{} {
	itemType, _ := item["type"].(string)
	if itemType == "" {
		if _, hasRole := item["role"]; hasRole {
			itemType = "message"
		}
	}

	switch itemType {
	case "message":
		return convertMessageItem(item)
	case "function_call":
		return convertFunctionCallItem(item)
	case "function_call_output":
		return convertFunctionCallOutputItem(item)
	default:
		return convertMessageItem(item)
	}
}

func convertMessageItem(item map[string]interface{}) map[string]interface{} {
	role, _ := item["role"].(string)
	if role == "" {
		role = "user"
	}

	if role == "developer" {
		role = "system"
	}

	message := map[string]interface{}{
		"role":    role,
		"content": "",
	}

	if content, ok := item["content"]; ok {
		switch v := content.(type) {
		case string:
			message["content"] = v
		case []interface{}:
			message["content"] = convertContentArray(v)
		}
	}

	return message
}

func convertContentArray(content []interface{}) interface{} {
	var textParts []string
	var hasMedia bool
	chatContent := []interface{}{}

	for _, block := range content {
		if blockMap, ok := block.(map[string]interface{}); ok {
			blockType, _ := blockMap["type"].(string)
			if blockType == "" {
				blockType = "input_text"
			}

			switch blockType {
			case "input_text", "output_text", "text":
				if text, ok := blockMap["text"].(string); ok && text != "" {
					textParts = append(textParts, text)
					chatContent = append(chatContent, map[string]interface{}{
						"type": "text",
						"text": text,
					})
				}
			case "input_image", "image_url":
				if imgBlock := convertImageBlock(blockMap); imgBlock != nil {
					chatContent = append(chatContent, imgBlock)
					hasMedia = true
				}
			}
		}
	}

	if hasMedia {
		return chatContent
	}
	if len(textParts) > 0 {
		return strings.Join(textParts, "\n")
	}
	return ""
}

func convertImageBlock(block map[string]interface{}) interface{} {
	// 1. Try image_url format (existing code)
	if imageURL, ok := block["image_url"]; ok {
		switch v := imageURL.(type) {
		case string:
			if v != "" {
				return map[string]interface{}{
					"type": "image_url",
					"image_url": map[string]interface{}{
						"url": v,
					},
				}
			}
		case map[string]interface{}:
			if url, ok := v["url"].(string); ok && url != "" {
				return map[string]interface{}{
					"type":      "image_url",
					"image_url": v,
				}
			}
		}
	}

	// 2. Try source format (base64 or url source)
	if source, ok := block["source"].(map[string]interface{}); ok {
		sourceType, _ := source["type"].(string)
		switch sourceType {
		case "base64":
			mediaType, _ := source["media_type"].(string)
			data, _ := source["data"].(string)
			if mediaType == "" || data == "" {
				return nil
			}
			return map[string]interface{}{
				"type": "image_url",
				"image_url": map[string]interface{}{
					"url": "data:" + mediaType + ";base64," + data,
				},
			}
		case "url":
			url, _ := source["url"].(string)
			if url == "" {
				return nil
			}
			return map[string]interface{}{
				"type": "image_url",
				"image_url": map[string]interface{}{
					"url": url,
				},
			}
		}
	}

	return nil
}

func convertFunctionCallItem(item map[string]interface{}) map[string]interface{} {
	callID, _ := item["call_id"].(string)
	name, _ := item["name"].(string)
	args, _ := item["arguments"].(string)
	if args == "" {
		args = "{}"
	}

	return map[string]interface{}{
		"role": "assistant",
		"tool_calls": []interface{}{
			map[string]interface{}{
				"id":   callID,
				"type": "function",
				"function": map[string]interface{}{
					"name":      name,
					"arguments": args,
				},
			},
		},
	}
}

func convertFunctionCallOutputItem(item map[string]interface{}) map[string]interface{} {
	callID, _ := item["call_id"].(string)
	output, _ := item["output"].(string)

	return map[string]interface{}{
		"role":         "tool",
		"tool_call_id": callID,
		"content":      output,
	}
}

func convertToolsToOpenAI(tools []interface{}) []interface{} {
	var result []interface{}

	for _, tool := range tools {
		if toolMap, ok := tool.(map[string]interface{}); ok {
			toolType, _ := toolMap["type"].(string)
			if toolType != "" && toolType != "function" {
				continue
			}

			name := getStringValue(toolMap, "name")
			if name == "" {
				if fn, ok := toolMap["function"].(map[string]interface{}); ok {
					name = getStringValue(fn, "name")
				}
			}
			if name == "" {
				continue
			}

			description := getStringValue(toolMap, "description")
			if description == "" {
				if fn, ok := toolMap["function"].(map[string]interface{}); ok {
					description = getStringValue(fn, "description")
				}
			}

			params := getObjectValue(toolMap, "parameters")
			if params == nil {
				if fn, ok := toolMap["function"].(map[string]interface{}); ok {
					params = getObjectValue(fn, "parameters")
				}
			}
			params = normalizeParameters(params)

			result = append(result, map[string]interface{}{
				"type": "function",
				"function": map[string]interface{}{
					"name":        name,
					"description": description,
					"parameters":  params,
				},
			})
		}
	}

	return result
}

func ConvertChatResponseToResponses(chatResponseBody []byte, model string) []byte {
	var chatResp map[string]interface{}
	if err := json.Unmarshal(chatResponseBody, &chatResp); err != nil {
		return chatResponseBody
	}

	responsesResp := map[string]interface{}{
		"output": []interface{}{},
		"status": "completed",
	}

	if id, ok := chatResp["id"].(string); ok {
		responsesResp["id"] = id
	}
	if created, ok := chatResp["created"].(float64); ok {
		responsesResp["created"] = int64(created)
	}
	if model != "" {
		responsesResp["model"] = model
	} else if m, ok := chatResp["model"].(string); ok {
		responsesResp["model"] = m
	}

	if choices, ok := chatResp["choices"].([]interface{}); ok {
		for _, choice := range choices {
			if choiceMap, ok := choice.(map[string]interface{}); ok {
				if message, ok := choiceMap["message"].(map[string]interface{}); ok {
					output := convertChatMessageToOutput(message)
					if outputs, ok := responsesResp["output"].([]interface{}); ok {
						responsesResp["output"] = append(outputs, output...)
					}
				}
			}
		}
	}

	if usage, ok := chatResp["usage"].(map[string]interface{}); ok {
		responsesResp["usage"] = parseUsage(usage)
	}

	result, _ := json.Marshal(responsesResp)
	return result
}

func convertChatMessageToOutput(message map[string]interface{}) []interface{} {
	var output []interface{}

	// 处理 reasoning_content
	if reasoning, ok := message["reasoning_content"].(string); ok && reasoning != "" {
		output = append(output, map[string]interface{}{
			"type":    "reasoning",
			"summary": []interface{}{map[string]interface{}{"type": "summary_text", "text": reasoning}},
		})
	}

	// 处理 content，同时提取 <think> 标签
	if content, ok := message["content"]; ok && content != nil {
		// 先尝试提取 thinking 内容
		thinking, remainingContent := extractThinkingFromContent(content)
		if thinking != "" {
			output = append(output, map[string]interface{}{
				"type":    "reasoning",
				"summary": []interface{}{map[string]interface{}{"type": "summary_text", "text": thinking}},
			})
		}

		// 处理剩余的文本内容
		if remainingContent != nil {
			switch v := remainingContent.(type) {
			case string:
				if v != "" {
					output = append(output, map[string]interface{}{
						"type": "message",
						"role": "assistant",
						"content": []interface{}{
							map[string]interface{}{
								"type": "output_text",
								"text": v,
							},
						},
					})
				}
			case []interface{}:
				// 如果是数组，检查是否有实际内容
				hasContent := false
				for _, block := range v {
					if blockMap, ok := block.(map[string]interface{}); ok {
						blockType, _ := blockMap["type"].(string)
						if (blockType == "text" || blockType == "output_text") &&
							getStringValue(blockMap, "text") != "" {
							hasContent = true
							break
						}
					}
				}
				if hasContent {
					output = append(output, map[string]interface{}{
						"type":    "message",
						"role":    "assistant",
						"content": v,
					})
				}
			}
		}
	}

	// 处理 tool_calls
	if toolCalls, ok := message["tool_calls"].([]interface{}); ok {
		for _, tc := range toolCalls {
			if tcMap, ok := tc.(map[string]interface{}); ok {
				fnVal := getObjectValue(tcMap, "function")
				fnMap, ok := fnVal.(map[string]interface{})
				if !ok {
					continue
				}
				output = append(output, map[string]interface{}{
					"type":      "function_call",
					"call_id":   getStringValue(tcMap, "id"),
					"name":      getStringValue(fnMap, "name"),
					"arguments": getStringValue(fnMap, "arguments"),
				})
			}
		}
	}

	return output
}

// parseUsage 解析 usage 字段，支持多种格式（OpenAI、Claude、Gemini）
func parseUsage(usage map[string]interface{}) map[string]interface{} {
	result := make(map[string]interface{})

	var inputTokens, outputTokens, totalTokens int
	var cachedTokens int
	var hasCacheDetails bool

	// 检查 Claude 格式（优先级最高）
	if _, has := usage["input_tokens"]; has {
		inputTokens = getIntValue(usage, "input_tokens")
	} else if _, has := usage["prompt_tokens"]; has {
		// OpenAI 格式
		inputTokens = getIntValue(usage, "prompt_tokens")
	} else if _, has := usage["promptTokenCount"]; has {
		// Gemini 格式
		inputTokens = getIntValue(usage, "promptTokenCount")
	}

	if _, has := usage["output_tokens"]; has {
		outputTokens = getIntValue(usage, "output_tokens")
	} else if _, has := usage["completion_tokens"]; has {
		outputTokens = getIntValue(usage, "completion_tokens")
	} else if _, has := usage["candidatesTokenCount"]; has {
		outputTokens = getIntValue(usage, "candidatesTokenCount")
	}

	if _, has := usage["total_tokens"]; has {
		totalTokens = getIntValue(usage, "total_tokens")
	} else {
		totalTokens = inputTokens + outputTokens
	}

	// 基础字段
	result["input_tokens"] = inputTokens
	result["output_tokens"] = outputTokens
	result["total_tokens"] = totalTokens

	// 处理缓存相关字段
	if v, has := usage["prompt_tokens_details"]; has {
		if details, ok := v.(map[string]interface{}); ok {
			if _, has := details["cached_tokens"]; has {
				cachedTokens = getIntValue(details, "cached_tokens")
				hasCacheDetails = true
			}
		}
	}

	if v, has := usage["input_tokens_details"]; has {
		if details, ok := v.(map[string]interface{}); ok {
			if _, has := details["cached_tokens"]; has {
				cachedTokens = getIntValue(details, "cached_tokens")
				hasCacheDetails = true
			}
		}
	}

	if _, has := usage["cache_read_input_tokens"]; has {
		cachedTokens = getIntValue(usage, "cache_read_input_tokens")
		hasCacheDetails = true
		result["cache_read_input_tokens"] = cachedTokens
	}

	// Claude 缓存创建字段
	if val, has := usage["cache_creation_input_tokens"]; has {
		result["cache_creation_input_tokens"] = val
	}
	if val, has := usage["cache_creation_5m_input_tokens"]; has {
		result["cache_creation_5m_input_tokens"] = val
	}
	if val, has := usage["cache_creation_1h_input_tokens"]; has {
		result["cache_creation_1h_input_tokens"] = val
	}
	if val, has := usage["cache_ttl"]; has {
		result["cache_ttl"] = val
	}

	// Gemini 缓存字段
	if _, has := usage["cachedContentTokenCount"]; has {
		cachedTokens = getIntValue(usage, "cachedContentTokenCount")
		hasCacheDetails = true
		// Gemini 的 promptTokenCount 已经包含了 cachedContentTokenCount，需要扣除
		if _, has := usage["promptTokenCount"]; has {
			actualInput := inputTokens - cachedTokens
			if actualInput < 0 {
				actualInput = 0
			}
			result["input_tokens"] = actualInput
			result["cache_read_input_tokens"] = cachedTokens
			result["total_tokens"] = actualInput + outputTokens
		}
	}

	// 添加 input_tokens_details（兼容 OpenAI 格式）
	if hasCacheDetails && cachedTokens > 0 {
		result["input_tokens_details"] = map[string]interface{}{
			"cached_tokens": cachedTokens,
		}
	}

	// 处理 output_tokens_details
	if v, has := usage["completion_tokens_details"]; has {
		result["output_tokens_details"] = v
	} else if v, has := usage["output_tokens_details"]; has {
		result["output_tokens_details"] = v
	}

	return result
}
