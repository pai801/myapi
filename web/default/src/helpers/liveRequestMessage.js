// 从请求体（原始 JSON 字符串）中提取最后一条 role=user 的消息文本，
// 供 Live Requests 面板「消息」列展示。
//
// 支持两种协议形态，按请求体结构分发（刻意不依赖后端 relay_mode 枚举）：
//   - Chat Completions：顶层 `messages` 为数组；
//   - Responses（/v1/responses）：顶层 `input`（字符串简写或 item 数组）。
// chat 判据严格为「`messages` 是数组」：`messages` 与 `input` 共存且 `messages`
// 为数组时按 chat 处理；`messages` 存在但非数组时不算合法 chat 请求，
// 降级按 responses（`input`）再试一次。
//
// request_body 过大时后端会替换成 "[body too large: N bytes]" 等非 JSON 占位串，
// 解析失败直接返回空串 —— 与现网行为一致。

// ===== Chat Completions 分支 =====

// content 有两种形态：
//   1) 字符串：{"role":"user","content":"继续工作"}
//   2) 分段数组：{"role":"user","content":[{"type":"text","text":"..."}, ...]}
//     取数组内最后一个 type=text 分段的 text
// 注意：chat 口径只认 type=text，responses 分段类型（如 input_text）在此不识别 ——
// 两套口径刻意隔离，不混用。
function extractChatContentText(content) {
  if (typeof content === 'string') return content.trim();
  if (Array.isArray(content)) {
    for (let i = content.length - 1; i >= 0; i--) {
      const part = content[i];
      if (part && part.type === 'text' && typeof part.text === 'string') {
        return part.text.trim();
      }
    }
  }
  return '';
}

// 倒序找最后一条 user 消息；若它取不到文本（如纯图片分段）则继续往前找
export function extractChatUserMessage(messages) {
  if (!Array.isArray(messages)) return '';
  for (let i = messages.length - 1; i >= 0; i--) {
    const msg = messages[i];
    if (!msg || msg.role !== 'user') continue;
    const text = extractChatContentText(msg.content);
    if (text) return text;
  }
  return '';
}

// ===== Responses 分支 =====

// 文本分段判定，口径对齐后端 relay/adaptor/codex/responses_to_chat.go 的
// convertContentArray：type 非字符串或空串时默认按 input_text 处理，
// 文本分段类型为 input_text / output_text / text。
// input_image / input_file / input_audio / refusal 等不产出文本。
function extractResponsesPartText(part) {
  if (!part || typeof part !== 'object') return '';
  const blockType = typeof part.type === 'string' ? part.type : '';
  const isTextPart =
    blockType === '' ||
    blockType === 'input_text' ||
    blockType === 'output_text' ||
    blockType === 'text';
  if (isTextPart && typeof part.text === 'string') return part.text.trim();
  return '';
}

// content 两种形态：字符串简写直接 trim；数组倒序取第一个文本分段
function extractResponsesContentText(content) {
  if (typeof content === 'string') return content.trim();
  if (Array.isArray(content)) {
    for (let i = content.length - 1; i >= 0; i--) {
      const text = extractResponsesPartText(content[i]);
      if (text) return text;
    }
  }
  return '';
}

// input 两种形态（docs/responses-protocol.md §2）：
//   1) EasyInputMessage 简写：字符串 → 直接 trim
//   2) item 数组 → 倒序取最后一条 user 消息 item。
// user item 认定：role==='user' 且 type 缺失或为 'message'（兼容无 type 的简写；
// function_call / function_call_output / reasoning / item_reference 等
// 不带 role 的 item 天然跳过）。取不到文本则继续往前找更早的 user 消息。
export function extractResponsesUserMessage(input) {
  if (typeof input === 'string') return input.trim();
  if (!Array.isArray(input)) return '';
  for (let i = input.length - 1; i >= 0; i--) {
    const item = input[i];
    if (!item || item.role !== 'user') continue;
    if (item.type !== undefined && item.type !== 'message') continue;
    const text = extractResponsesContentText(item.content);
    if (text) return text;
  }
  return '';
}

// ===== 入口：形状分发 =====

export function extractUserMessage(requestBody) {
  if (!requestBody) return '';
  let parsed;
  try {
    parsed = JSON.parse(requestBody);
  } catch (e) {
    return '';
  }
  if (!parsed || typeof parsed !== 'object') return '';
  if (Array.isArray(parsed.messages)) return extractChatUserMessage(parsed.messages);
  if (parsed.input !== undefined) return extractResponsesUserMessage(parsed.input);
  return '';
}
