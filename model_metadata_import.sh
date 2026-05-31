#!/bin/bash
# One API 模型元数据导入脚本
# 使用方法: bash model_metadata.sh 或复制单个 curl 命令执行

BASE_URL="http://localhost:3000"
TOKEN="YOUR_ADMIN_TOKEN"  # 替换为你的管理员 token

echo "开始导入模型元数据..."

# ==================== GPT 系列 ====================

# GPT-4
curl -X POST "${BASE_URL}/api/model-metadata/" \
  -H "Authorization: Bearer ${TOKEN}" \
  -H "Content-Type: application/json" \
  -d '{
    "name": "gpt4",
    "display_name": "GPT-4",
    "visibility": "list",
    "supported_in_api": true,
    "priority": 100,
    "default_reasoning_level": "medium",
    "supported_reasoning_levels": ["low", "medium", "high"],
    "context_window": 8192,
    "truncation_policy": "auto",
    "input_modalities": ["text"],
    "output_modalities": ["text"],
    "supported_endpoint_types": ["openai"],
    "apply_patch_tool_type": "",
    "web_search_tool_type": "",
    "max_output_tokens": 8192
  }'

# GPT-4 Turbo
curl -X POST "${BASE_URL}/api/model-metadata/" \
  -H "Authorization: Bearer ${TOKEN}" \
  -H "Content-Type: application/json" \
  -d '{
    "name": "gpt4turbo",
    "display_name": "GPT-4 Turbo",
    "visibility": "list",
    "supported_in_api": true,
    "priority": 95,
    "default_reasoning_level": "medium",
    "supported_reasoning_levels": ["low", "medium", "high"],
    "context_window": 128000,
    "truncation_policy": "auto",
    "input_modalities": ["text", "image"],
    "output_modalities": ["text"],
    "supported_endpoint_types": ["openai", "openai-response"],
    "apply_patch_tool_type": "function",
    "web_search_tool_type": "function",
    "max_output_tokens": 4096
  }'

# GPT-4o
curl -X POST "${BASE_URL}/api/model-metadata/" \
  -H "Authorization: Bearer ${TOKEN}" \
  -H "Content-Type: application/json" \
  -d '{
    "name": "gpt4o",
    "display_name": "GPT-4o",
    "visibility": "list",
    "supported_in_api": true,
    "priority": 90,
    "default_reasoning_level": "medium",
    "supported_reasoning_levels": ["low", "medium", "high"],
    "context_window": 128000,
    "truncation_policy": "auto",
    "input_modalities": ["text", "image"],
    "output_modalities": ["text"],
    "supported_endpoint_types": ["openai", "openai-response"],
    "apply_patch_tool_type": "function",
    "web_search_tool_type": "function",
    "max_output_tokens": 16384
  }'

# GPT-4o Mini
curl -X POST "${BASE_URL}/api/model-metadata/" \
  -H "Authorization: Bearer ${TOKEN}" \
  -H "Content-Type: application/json" \
  -d '{
    "name": "gpt4omin",
    "display_name": "GPT-4o Mini",
    "visibility": "list",
    "supported_in_api": true,
    "priority": 80,
    "default_reasoning_level": "medium",
    "supported_reasoning_levels": ["low", "medium", "high"],
    "context_window": 128000,
    "truncation_policy": "auto",
    "input_modalities": ["text", "image"],
    "output_modalities": ["text"],
    "supported_endpoint_types": ["openai", "openai-response"],
    "apply_patch_tool_type": "",
    "web_search_tool_type": "",
    "max_output_tokens": 16384
  }'

# GPT-5
curl -X POST "${BASE_URL}/api/model-metadata/" \
  -H "Authorization: Bearer ${TOKEN}" \
  -H "Content-Type: application/json" \
  -d '{
    "name": "gpt5",
    "display_name": "GPT-5",
    "visibility": "list",
    "supported_in_api": true,
    "priority": 0,
    "default_reasoning_level": "medium",
    "supported_reasoning_levels": ["low", "medium", "high", "xhigh"],
    "context_window": 200000,
    "truncation_policy": "auto",
    "input_modalities": ["text", "image"],
    "output_modalities": ["text"],
    "supported_endpoint_types": ["openai", "openai-response"],
    "apply_patch_tool_type": "function",
    "web_search_tool_type": "function",
    "max_output_tokens": 32768
  }'

# GPT-5.5
curl -X POST "${BASE_URL}/api/model-metadata/" \
  -H "Authorization: Bearer ${TOKEN}" \
  -H "Content-Type: application/json" \
  -d '{
    "name": "gpt55",
    "display_name": "GPT-5.5",
    "visibility": "list",
    "supported_in_api": true,
    "priority": 0,
    "default_reasoning_level": "medium",
    "supported_reasoning_levels": ["low", "medium", "high", "xhigh"],
    "context_window": 200000,
    "truncation_policy": "auto",
    "input_modalities": ["text", "image"],
    "output_modalities": ["text"],
    "supported_endpoint_types": ["openai", "openai-response"],
    "apply_patch_tool_type": "function",
    "web_search_tool_type": "function",
    "max_output_tokens": 32768
  }'

echo ""
echo "GPT 系列导入完成！"

# ==================== Claude 系列 ====================

# Claude 3.5 Sonnet
curl -X POST "${BASE_URL}/api/model-metadata/" \
  -H "Authorization: Bearer ${TOKEN}" \
  -H "Content-Type: application/json" \
  -d '{
    "name": "claude35sonnet",
    "display_name": "Claude 3.5 Sonnet",
    "visibility": "list",
    "supported_in_api": true,
    "priority": 50,
    "default_reasoning_level": "medium",
    "supported_reasoning_levels": ["low", "medium", "high"],
    "context_window": 200000,
    "truncation_policy": "auto",
    "input_modalities": ["text", "image"],
    "output_modalities": ["text"],
    "supported_endpoint_types": ["anthropic"],
    "apply_patch_tool_type": "",
    "web_search_tool_type": "",
    "max_output_tokens": 8192
  }'

# Claude 3 Opus
curl -X POST "${BASE_URL}/api/model-metadata/" \
  -H "Authorization: Bearer ${TOKEN}" \
  -H "Content-Type: application/json" \
  -d '{
    "name": "claude3opus",
    "display_name": "Claude 3 Opus",
    "visibility": "list",
    "supported_in_api": true,
    "priority": 40,
    "default_reasoning_level": "medium",
    "supported_reasoning_levels": ["low", "medium", "high"],
    "context_window": 200000,
    "truncation_policy": "auto",
    "input_modalities": ["text", "image"],
    "output_modalities": ["text"],
    "supported_endpoint_types": ["anthropic"],
    "apply_patch_tool_type": "",
    "web_search_tool_type": "",
    "max_output_tokens": 4096
  }'

# Claude 3.5 Haiku
curl -X POST "${BASE_URL}/api/model-metadata/" \
  -H "Authorization: Bearer ${TOKEN}" \
  -H "Content-Type: application/json" \
  -d '{
    "name": "claude35haiku",
    "display_name": "Claude 3.5 Haiku",
    "visibility": "list",
    "supported_in_api": true,
    "priority": 60,
    "default_reasoning_level": "medium",
    "supported_reasoning_levels": ["low", "medium", "high"],
    "context_window": 200000,
    "truncation_policy": "auto",
    "input_modalities": ["text", "image"],
    "output_modalities": ["text"],
    "supported_endpoint_types": ["anthropic"],
    "apply_patch_tool_type": "",
    "web_search_tool_type": "",
    "max_output_tokens": 4096
  }'

echo ""
echo "Claude 系列导入完成！"

# ==================== DeepSeek 系列 ====================

# DeepSeek V3
curl -X POST "${BASE_URL}/api/model-metadata/" \
  -H "Authorization: Bearer ${TOKEN}" \
  -H "Content-Type: application/json" \
  -d '{
    "name": "deepseekv3",
    "display_name": "DeepSeek V3",
    "visibility": "list",
    "supported_in_api": true,
    "priority": 60,
    "default_reasoning_level": "medium",
    "supported_reasoning_levels": ["low", "medium", "high"],
    "context_window": 128000,
    "truncation_policy": "auto",
    "input_modalities": ["text"],
    "output_modalities": ["text"],
    "supported_endpoint_types": ["openai", "openai-response"],
    "apply_patch_tool_type": "",
    "web_search_tool_type": "",
    "max_output_tokens": 8192
  }'

# DeepSeek V4 Pro
curl -X POST "${BASE_URL}/api/model-metadata/" \
  -H "Authorization: Bearer ${TOKEN}" \
  -H "Content-Type: application/json" \
  -d '{
    "name": "deepseekv4pro",
    "display_name": "DeepSeek V4 Pro",
    "visibility": "list",
    "supported_in_api": true,
    "priority": 20,
    "default_reasoning_level": "medium",
    "supported_reasoning_levels": ["low", "medium", "high"],
    "context_window": 128000,
    "truncation_policy": "auto",
    "input_modalities": ["text"],
    "output_modalities": ["text"],
    "supported_endpoint_types": ["openai", "openai-response"],
    "apply_patch_tool_type": "",
    "web_search_tool_type": "",
    "max_output_tokens": 8192
  }'

# DeepSeek V4 Flash
curl -X POST "${BASE_URL}/api/model-metadata/" \
  -H "Authorization: Bearer ${TOKEN}" \
  -H "Content-Type: application/json" \
  -d '{
    "name": "deepseekv4flash",
    "display_name": "DeepSeek V4 Flash",
    "visibility": "list",
    "supported_in_api": true,
    "priority": 30,
    "default_reasoning_level": "medium",
    "supported_reasoning_levels": ["low", "medium", "high"],
    "context_window": 128000,
    "truncation_policy": "auto",
    "input_modalities": ["text"],
    "output_modalities": ["text"],
    "supported_endpoint_types": ["openai", "openai-response"],
    "apply_patch_tool_type": "",
    "web_search_tool_type": "",
    "max_output_tokens": 8192
  }'

echo ""
echo "DeepSeek 系列导入完成！"

# ==================== Kimi 系列 ====================

# Kimi K2.6
curl -X POST "${BASE_URL}/api/model-metadata/" \
  -H "Authorization: Bearer ${TOKEN}" \
  -H "Content-Type: application/json" \
  -d '{
    "name": "kimik26",
    "display_name": "Kimi K2.6",
    "visibility": "list",
    "supported_in_api": true,
    "priority": 35,
    "default_reasoning_level": "medium",
    "supported_reasoning_levels": ["low", "medium", "high"],
    "context_window": 200000,
    "truncation_policy": "auto",
    "input_modalities": ["text"],
    "output_modalities": ["text"],
    "supported_endpoint_types": ["openai"],
    "apply_patch_tool_type": "",
    "web_search_tool_type": "",
    "max_output_tokens": 8192
  }'

echo ""
echo "Kimi 系列导入完成！"

# ==================== Qwen 系列 ====================

# Qwen 2.5 7B
curl -X POST "${BASE_URL}/api/model-metadata/" \
  -H "Authorization: Bearer ${TOKEN}" \
  -H "Content-Type: application/json" \
  -d '{
    "name": "qwen257binstruct",
    "display_name": "Qwen 2.5 7B",
    "visibility": "list",
    "supported_in_api": true,
    "priority": 70,
    "default_reasoning_level": "medium",
    "supported_reasoning_levels": ["low", "medium", "high"],
    "context_window": 32768,
    "truncation_policy": "auto",
    "input_modalities": ["text"],
    "output_modalities": ["text"],
    "supported_endpoint_types": ["openai"],
    "apply_patch_tool_type": "",
    "web_search_tool_type": "",
    "max_output_tokens": 8192
  }'

# Qwen 2.5 72B
curl -X POST "${BASE_URL}/api/model-metadata/" \
  -H "Authorization: Bearer ${TOKEN}" \
  -H "Content-Type: application/json" \
  -d '{
    "name": "qwen2572binstruct",
    "display_name": "Qwen 2.5 72B",
    "visibility": "list",
    "supported_in_api": true,
    "priority": 45,
    "default_reasoning_level": "medium",
    "supported_reasoning_levels": ["low", "medium", "high"],
    "context_window": 128000,
    "truncation_policy": "auto",
    "input_modalities": ["text"],
    "output_modalities": ["text"],
    "supported_endpoint_types": ["openai"],
    "apply_patch_tool_type": "",
    "web_search_tool_type": "",
    "max_output_tokens": 8192
  }'

# Qwen 3
curl -X POST "${BASE_URL}/api/model-metadata/" \
  -H "Authorization: Bearer ${TOKEN}" \
  -H "Content-Type: application/json" \
  -d '{
    "name": "qwen3",
    "display_name": "Qwen 3",
    "visibility": "list",
    "supported_in_api": true,
    "priority": 15,
    "default_reasoning_level": "medium",
    "supported_reasoning_levels": ["low", "medium", "high"],
    "context_window": 128000,
    "truncation_policy": "auto",
    "input_modalities": ["text"],
    "output_modalities": ["text"],
    "supported_endpoint_types": ["openai", "openai-response"],
    "apply_patch_tool_type": "",
    "web_search_tool_type": "",
    "max_output_tokens": 8192
  }'

echo ""
echo "Qwen 系列导入完成！"

# ==================== Llama 系列 ====================

# Llama 3.1 8B
curl -X POST "${BASE_URL}/api/model-metadata/" \
  -H "Authorization: Bearer ${TOKEN}" \
  -H "Content-Type: application/json" \
  -d '{
    "name": "llama31instruct8b",
    "display_name": "Llama 3.1 8B",
    "visibility": "list",
    "supported_in_api": true,
    "priority": 85,
    "default_reasoning_level": "medium",
    "supported_reasoning_levels": ["low", "medium", "high"],
    "context_window": 128000,
    "truncation_policy": "auto",
    "input_modalities": ["text"],
    "output_modalities": ["text"],
    "supported_endpoint_types": ["openai"],
    "apply_patch_tool_type": "",
    "web_search_tool_type": "",
    "max_output_tokens": 8192
  }'

# Llama 3.1 70B
curl -X POST "${BASE_URL}/api/model-metadata/" \
  -H "Authorization: Bearer ${TOKEN}" \
  -H "Content-Type: application/json" \
  -d '{
    "name": "llama31instruct70b",
    "display_name": "Llama 3.1 70B",
    "visibility": "list",
    "supported_in_api": true,
    "priority": 55,
    "default_reasoning_level": "medium",
    "supported_reasoning_levels": ["low", "medium", "high"],
    "context_window": 128000,
    "truncation_policy": "auto",
    "input_modalities": ["text"],
    "output_modalities": ["text"],
    "supported_endpoint_types": ["openai"],
    "apply_patch_tool_type": "",
    "web_search_tool_type": "",
    "max_output_tokens": 8192
  }'

echo ""
echo "Llama 系列导入完成！"

# ==================== 导入完成 ====================

echo ""
echo "========================================="
echo "所有模型元数据导入完成！"
echo "========================================="
echo ""
echo "查看已导入的数据："
echo "curl -H \"Authorization: Bearer ${TOKEN}\" ${BASE_URL}/api/model-metadata/"
echo ""
