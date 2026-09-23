import {
  extractChatUserMessage,
  extractResponsesUserMessage,
  extractUserMessage,
} from './liveRequestMessage';

// Live Requests「消息」列取值的纯函数单测：
// chat（顶层 messages）与 responses（顶层 input）两种协议形态的取值口径，
// 以及按形状分发、异常输入降级为空串的行为。

const body = (obj) => JSON.stringify(obj);

describe('extractUserMessage — chat 协议回归', () => {
  it('取最后一条 user 消息的字符串 content', () => {
    expect(
      extractUserMessage(
        body({
          model: 'gpt-4o',
          messages: [
            { role: 'system', content: 'sys' },
            { role: 'user', content: '  继续工作  ' },
            { role: 'assistant', content: 'ok' },
          ],
        })
      )
    ).toBe('继续工作');
  });

  it('content 为分段数组时取最后一个 type=text 分段', () => {
    expect(
      extractUserMessage(
        body({
          messages: [
            {
              role: 'user',
              content: [
                { type: 'text', text: '第一段' },
                { type: 'image_url', image_url: { url: 'https://x/img.png' } },
                { type: 'text', text: '最后一段' },
              ],
            },
          ],
        })
      )
    ).toBe('最后一段');
  });

  it('最后一条 user 只有图片分段时回退到更早的 user 消息', () => {
    expect(
      extractUserMessage(
        body({
          messages: [
            { role: 'user', content: '有文本的前一条' },
            { role: 'assistant', content: 'ack' },
            { role: 'user', content: [{ type: 'image_url', image_url: { url: 'x' } }] },
          ],
        })
      )
    ).toBe('有文本的前一条');
  });

  it('messages 存在但非数组时返回空串', () => {
    expect(extractUserMessage(body({ messages: 'not an array' }))).toBe('');
    expect(extractUserMessage(body({ messages: { role: 'user' } }))).toBe('');
  });

  it('非 JSON / body too large 占位串 / 空值一律返回空串', () => {
    expect(extractUserMessage('this is not json {{{')).toBe('');
    expect(extractUserMessage('[body too large: 100 bytes]')).toBe('');
    expect(extractUserMessage('')).toBe('');
    expect(extractUserMessage(null)).toBe('');
    expect(extractUserMessage(undefined)).toBe('');
  });

  it('chat 分支口径隔离：不识别 responses 的 input_text 分段', () => {
    expect(
      extractUserMessage(
        body({
          messages: [
            { role: 'user', content: [{ type: 'input_text', text: 'responses 分段类型' }] },
          ],
        })
      )
    ).toBe('');
  });

  it('messages 含 null 元素时跳过并取到有效 user 消息', () => {
    expect(
      extractUserMessage(
        body({
          messages: [null, { role: 'user', content: 'x' }],
        })
      )
    ).toBe('x');
  });

  it('顶层能 parse 成原始值（如数字）时返回空串', () => {
    expect(extractUserMessage('42')).toBe('');
  });
});

describe('extractUserMessage — responses 协议', () => {
  it('input 为字符串简写（EasyInputMessage）时直接返回 trim 结果', () => {
    expect(extractUserMessage(body({ model: 'gpt-5', input: '  你好  ' }))).toBe('你好');
  });

  it('input 数组取末条 user message item 的 input_text', () => {
    // 线上真实 codex 请求体骨架（instructions 是 system prompt，不展示）
    expect(
      extractUserMessage(
        body({
          model: 'gpt-5.6-sol',
          input: [
            {
              type: 'message',
              role: 'user',
              content: [
                { type: 'input_text', text: '你是本次变更的架构契约生成者。请加载并严格执行 simple-contracts skill...' },
              ],
            },
          ],
          instructions: '## Prompt Defense Baseline ...',
          tools: [{ type: 'function', name: 'glob', parameters: {} }],
          store: false,
          reasoning: { effort: 'high' },
          stream: true,
        })
      )
    ).toBe('你是本次变更的架构契约生成者。请加载并严格执行 simple-contracts skill...');
  });

  it('content 数组倒序取文本分段：图片在前、文本在后', () => {
    expect(
      extractUserMessage(
        body({
          input: [
            {
              type: 'message',
              role: 'user',
              content: [
                { type: 'input_text', text: '前文' },
                { type: 'input_image', image_url: 'https://x/img.png' },
                { type: 'input_text', text: '后文' },
              ],
            },
          ],
        })
      )
    ).toBe('后文');
  });

  it('混排 function_call_output / reasoning / assistant output_text 时只取 user 文本', () => {
    expect(
      extractUserMessage(
        body({
          input: [
            { type: 'message', role: 'user', content: [{ type: 'input_text', text: '用户的真实问题' }] },
            { type: 'message', role: 'assistant', content: [{ type: 'output_text', text: '助手回答' }] },
            { type: 'reasoning', summary: [{ type: 'summary_text', text: '思考' }] },
            { type: 'function_call', call_id: 'fc_1', name: 'shell', arguments: '{}' },
            { type: 'function_call_output', call_id: 'fc_1', output: 'stdout 内容' },
          ],
        })
      )
    ).toBe('用户的真实问题');
  });

  it('兼容无 type 的 {"role":"user","content":"..."} 简写 item', () => {
    expect(
      extractUserMessage(
        body({
          input: [
            { role: 'user', content: '无 type 简写' },
          ],
        })
      )
    ).toBe('无 type 简写');
  });

  it('input 数组无 user 消息时返回空串', () => {
    expect(
      extractUserMessage(
        body({
          input: [
            { type: 'message', role: 'assistant', content: [{ type: 'output_text', text: 'hi' }] },
            { type: 'function_call_output', call_id: 'fc_1', output: 'x' },
          ],
        })
      )
    ).toBe('');
  });

  it('type 缺失但带字符串 text 的分段按文本处理（对齐 convertContentArray 口径）', () => {
    expect(
      extractUserMessage(
        body({
          input: [
            { type: 'message', role: 'user', content: [{ text: '缺 type 的文本分段' }] },
          ],
        })
      )
    ).toBe('缺 type 的文本分段');
  });

  it('最后一条 user 只有 input_image 分段时回退到更早的 user 消息', () => {
    expect(
      extractUserMessage(
        body({
          input: [
            { type: 'message', role: 'user', content: [{ type: 'input_text', text: '更早的问题' }] },
            { type: 'message', role: 'assistant', content: [{ type: 'output_text', text: 'ok' }] },
            { type: 'message', role: 'user', content: [{ type: 'input_image', image_url: 'x' }] },
          ],
        })
      )
    ).toBe('更早的问题');
  });

  it('input 含 null 元素时跳过并取到有效 user 消息', () => {
    expect(
      extractUserMessage(
        body({
          input: [
            null,
            { type: 'message', role: 'user', content: [{ type: 'input_text', text: 'x' }] },
          ],
        })
      )
    ).toBe('x');
  });

  it('非字符串 type 的分段按 input_text 处理（type:null / type:0）', () => {
    expect(
      extractUserMessage(
        body({
          input: [
            { type: 'message', role: 'user', content: [{ type: null, text: 'NULL' }] },
          ],
        })
      )
    ).toBe('NULL');
    expect(
      extractUserMessage(
        body({
          input: [
            { type: 'message', role: 'user', content: [{ type: 0, text: 'N' }] },
          ],
        })
      )
    ).toBe('N');
  });

  it('空文本/纯空白分段被跳过、继续往前找更早文本', () => {
    expect(
      extractUserMessage(
        body({
          input: [
            {
              type: 'message',
              role: 'user',
              content: [
                { type: 'input_text', text: 'earlier' },
                { type: 'input_text', text: '   ' },
              ],
            },
          ],
        })
      )
    ).toBe('earlier');
  });

  it('content 为空数组时返回空串', () => {
    expect(
      extractUserMessage(
        body({
          input: [{ type: 'message', role: 'user', content: [] }],
        })
      )
    ).toBe('');
  });
});

describe('extractUserMessage — 形状分发', () => {
  it('messages 与 input 同时存在时走 chat 分支', () => {
    expect(
      extractUserMessage(
        body({
          messages: [{ role: 'user', content: 'chat 分支' }],
          input: [{ type: 'message', role: 'user', content: [{ type: 'input_text', text: 'responses 分支' }] }],
        })
      )
    ).toBe('chat 分支');
  });

  it('两者都不存在时返回空串', () => {
    expect(extractUserMessage(body({ model: 'gpt-5', instructions: 'sys' }))).toBe('');
  });

  it('messages 存在但非数组、且有 input 时降级走 responses 分支', () => {
    expect(
      extractUserMessage(
        body({
          messages: { x: 1 },
          input: [{ type: 'message', role: 'user', content: [{ type: 'input_text', text: 'resp' }] }],
        })
      )
    ).toBe('resp');
  });
});

describe('分支函数（供复用）', () => {
  it('extractChatUserMessage 直接接收 messages 数组', () => {
    expect(extractChatUserMessage([{ role: 'user', content: '直接调用' }])).toBe('直接调用');
  });

  it('extractResponsesUserMessage 直接接收 input', () => {
    expect(extractResponsesUserMessage('字符串形态')).toBe('字符串形态');
    expect(extractResponsesUserMessage(42)).toBe('');
  });
});
