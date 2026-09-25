import React from 'react';
import { createRoot } from 'react-dom/client';
import { act } from 'react-dom/test-utils';

// 与项目其余组件测试一致：直接用 react-dom/client + test-utils 的 act（未安装 @testing-library）。
/* eslint-disable testing-library/no-unnecessary-act */

global.IS_REACT_ACT_ENVIRONMENT = true;

// 一键粘贴（仅新建页）的契约测试：校验整份 v1 信封后才填表，任一失败都保持表单不变且不发请求，
// 剪贴板读取被拒只弹失败提示。CRA resetMocks 会逐用例清空实现，故所有 mockImplementation 放 beforeEach。

jest.mock('react-i18next', () => ({
  useTranslation: () => ({ t: (k) => k, i18n: { language: 'zh' } }),
}));

// navigate 用可变 mock 承载：断言新建保存成功后才跳转回渠道列表。
let mockNavigate;

// 新建模式：useParams 返回 {} → isEdit 为 false。
jest.mock('react-router-dom', () => ({
  useParams: () => ({}),
  useNavigate: () => mockNavigate,
}));

jest.mock('../../helpers', () => ({
  API: { get: jest.fn(), post: jest.fn(), put: jest.fn() },
  copy: jest.fn(),
  getChannelModels: jest.fn(),
  showError: jest.fn(),
  showInfo: jest.fn(),
  showSuccess: jest.fn(),
  showWarning: jest.fn(),
  verifyJSON: jest.fn(),
}));

jest.mock('../../helpers/render', () => ({
  renderChannelTip: jest.fn(),
}));

jest.mock('../../components/ChannelPanelRenderer', () => ({
  __esModule: true,
  default: () => null,
}));

let mockDescriptor = null;
// 模拟模块级 descriptor 缓存就绪状态：仅当 loadChannelDescriptors 解析后才可被 getChannelDescriptor 命中。
let descriptorsReady = false;

jest.mock('../../helpers/channelDescriptor', () => ({
  loadChannelDescriptors: jest.fn(),
  findDescriptor: jest.fn(),
  getChannelDescriptor: jest.fn(),
  buildChannelOptions: jest.fn(),
  descriptorKeyPrompt: jest.fn(),
}));

// eslint-disable-next-line import/first
import { API, getChannelModels, showError, showSuccess, verifyJSON } from '../../helpers';
// eslint-disable-next-line import/first
import {
  buildChannelOptions,
  descriptorKeyPrompt,
  findDescriptor,
  getChannelDescriptor,
  loadChannelDescriptors,
} from '../../helpers/channelDescriptor';
// eslint-disable-next-line import/first
import EditChannel from './EditChannel';

const PASTE_BUTTON_KEY = 'channel.edit.buttons.paste';
const PASTE_SUCCESS_KEY = 'channel.edit.messages.paste_success';
const PASTE_INVALID_KEY = 'channel.edit.messages.paste_invalid';
const PASTE_FAILED_KEY = 'channel.edit.messages.paste_failed';
const SUBMIT_KEY = 'channel.edit.buttons.submit';

function validEnvelope() {
  return {
    myapi_channel: 1,
    channel: {
      type: 1,
      name: 'pasted',
      group: 'default,vip',
      models: 'gpt-3.5-turbo,gpt-4',
      key: 'sk-pasted',
      base_url: 'https://api.example.com',
      other: 'other-val',
      model_mapping: '{"a":"b"}',
      system_prompt: 'sys',
      priority: 5,
      config: '{"custom_key":{"x":1},"region":"us"}',
    },
  };
}

describe('EditChannel paste action (create mode)', () => {
  let container;
  let root;
  let readText;

  beforeEach(() => {
    mockDescriptor = null;
    mockNavigate = jest.fn();
    descriptorsReady = false;
    readText = jest.fn();
    Object.defineProperty(navigator, 'clipboard', {
      value: { readText },
      configurable: true,
      writable: true,
    });

    loadChannelDescriptors.mockImplementation(() => {
      descriptorsReady = true;
      return Promise.resolve(mockDescriptor ? [mockDescriptor] : []);
    });
    findDescriptor.mockImplementation(() => mockDescriptor);
    getChannelDescriptor.mockImplementation(() =>
      descriptorsReady ? mockDescriptor : undefined
    );
    buildChannelOptions.mockImplementation(() => []);
    descriptorKeyPrompt.mockImplementation(() => '');
    getChannelModels.mockImplementation(() => []);
    verifyJSON.mockImplementation(() => true);

    API.get.mockImplementation((url) => {
      if (url === '/api/channel/models') {
        return Promise.resolve({ data: { data: [] } });
      }
      if (url === '/api/group/') {
        return Promise.resolve({ data: { data: [] } });
      }
      return Promise.resolve({ data: { success: true, message: '', data: [] } });
    });
    API.post.mockResolvedValue({ data: { success: true, message: '' } });
    API.put.mockResolvedValue({ data: { success: true, message: '' } });

    container = document.createElement('div');
    document.body.appendChild(container);
    root = createRoot(container);
  });

  afterEach(() => {
    act(() => root.unmount());
    container.remove();
  });

  async function renderPage() {
    await act(async () => {
      root.render(<EditChannel />);
    });
    await act(async () => {
      await new Promise((resolve) => setTimeout(resolve, 0));
    });
  }

  function findPasteButton() {
    return Array.from(container.querySelectorAll('button')).find((b) =>
      b.textContent.includes(PASTE_BUTTON_KEY)
    );
  }

  function findSubmitButton() {
    return Array.from(container.querySelectorAll('button')).find((b) =>
      b.textContent.includes(SUBMIT_KEY)
    );
  }

  async function clickPaste() {
    const button = findPasteButton();
    expect(button).toBeTruthy();
    await act(async () => {
      button.dispatchEvent(new MouseEvent('click', { bubbles: true }));
      await new Promise((resolve) => setTimeout(resolve, 0));
    });
  }

  async function clickSubmit() {
    const button = findSubmitButton();
    expect(button).toBeTruthy();
    await act(async () => {
      button.dispatchEvent(new MouseEvent('click', { bubbles: true }));
      await new Promise((resolve) => setTimeout(resolve, 0));
    });
  }

  function inputValue(name) {
    const el = container.querySelector(`input[name="${name}"]`);
    return el ? el.value : undefined;
  }

  function textareaValue(name) {
    const el = container.querySelector(`textarea[name="${name}"]`);
    return el ? el.value : undefined;
  }

  it('renders the paste button only in create mode', async () => {
    await renderPage();
    expect(findPasteButton()).toBeTruthy();
  });

  it('fills the form on a valid envelope and does NOT auto-submit', async () => {
    readText.mockResolvedValue(JSON.stringify(validEnvelope()));
    await renderPage();

    await clickPaste();

    expect(readText).toHaveBeenCalledTimes(1);
    expect(showSuccess).toHaveBeenCalledWith(PASTE_SUCCESS_KEY);
    expect(showError).not.toHaveBeenCalled();

    // 粘贴本身绝不触发提交。
    expect(API.post).not.toHaveBeenCalled();
    expect(API.put).not.toHaveBeenCalled();

    // 直接字段被填充。
    expect(inputValue('name')).toBe('pasted');
    expect(inputValue('key')).toBe('sk-pasted');
    expect(inputValue('base_url')).toBe('https://api.example.com');
    expect(textareaValue('model_mapping')).toBe('{"a":"b"}');
    expect(textareaValue('system_prompt')).toBe('sys');
    // config 扩展键进入自定义配置编辑器，系统键（region）被排除。
    expect(textareaValue('custom_config')).toContain('custom_key');
    expect(textareaValue('custom_config')).not.toContain('region');

    // 通过手动提交核对 group/models 的逗号串 -> 数组映射（仅此一次 POST，非粘贴触发）。
    API.post.mockClear();
    await act(async () => {
      findSubmitButton().dispatchEvent(new MouseEvent('click', { bubbles: true }));
      await new Promise((resolve) => setTimeout(resolve, 0));
    });
    expect(API.post).toHaveBeenCalledTimes(1);
    const payload = API.post.mock.calls[0][1];
    expect(payload.group).toBe('default,vip');
    expect(payload.models).toBe('gpt-3.5-turbo,gpt-4');
    expect(payload.priority).toBe(5);
    const parsedConfig = JSON.parse(payload.config);
    expect(parsedConfig.custom_key).toEqual({ x: 1 });
    expect(parsedConfig.region).toBe('us');
  });

  it('rejects a wrong/absent marker and leaves the form untouched', async () => {
    const cases = [
      JSON.stringify({ ...validEnvelope(), myapi_channel: 2 }),
      JSON.stringify({ channel: validEnvelope().channel }),
    ];
    for (const text of cases) {
      readText.mockResolvedValue(text);
      await renderPage();

      showError.mockClear();
      await clickPaste();

      expect(showError).toHaveBeenCalledWith(PASTE_INVALID_KEY);
      expect(showSuccess).not.toHaveBeenCalled();
      expect(inputValue('name')).toBe('');
      expect(inputValue('key')).toBe('');
      expect(API.post).not.toHaveBeenCalled();
    }
  });

  it('rejects malformed JSON and non-object channel, leaving the form untouched', async () => {
    const cases = [
      '{not valid json',
      JSON.stringify({ myapi_channel: 1, channel: [1, 2, 3] }),
      JSON.stringify({ myapi_channel: 1, channel: null }),
      JSON.stringify({ myapi_channel: 1, channel: 'x' }),
    ];
    for (const text of cases) {
      readText.mockResolvedValue(text);
      await renderPage();

      showError.mockClear();
      await clickPaste();

      expect(showError).toHaveBeenCalledWith(PASTE_INVALID_KEY);
      expect(showSuccess).not.toHaveBeenCalled();
      expect(inputValue('name')).toBe('');
      expect(API.post).not.toHaveBeenCalled();
    }
  });

  it('rejects wrong field types and non-object config', async () => {
    const base = validEnvelope();
    const cases = [
      { ...base, channel: { ...base.channel, name: 123 } },
      { ...base, channel: { ...base.channel, group: ['default'] } },
      { ...base, channel: { ...base.channel, models: 1 } },
      { ...base, channel: { ...base.channel, key: null } },
      { ...base, channel: { ...base.channel, priority: '5' } },
      { ...base, channel: { ...base.channel, config: '[1,2]' } },
      { ...base, channel: { ...base.channel, config: 'not json' } },
      { ...base, channel: { ...base.channel, model_mapping: '{bad' } },
    ];
    for (const envelope of cases) {
      readText.mockResolvedValue(JSON.stringify(envelope));
      await renderPage();

      showError.mockClear();
      await clickPaste();

      expect(showError).toHaveBeenCalledWith(PASTE_INVALID_KEY);
      expect(showSuccess).not.toHaveBeenCalled();
      expect(inputValue('name')).toBe('');
      expect(API.post).not.toHaveBeenCalled();
    }
  });

  it('shows the failed toast when the clipboard read is rejected and never posts', async () => {
    readText.mockRejectedValue(new Error('denied'));
    await renderPage();

    await clickPaste();

    expect(showError).toHaveBeenCalledWith(PASTE_FAILED_KEY);
    expect(showSuccess).not.toHaveBeenCalled();
    expect(inputValue('name')).toBe('');
    expect(API.post).not.toHaveBeenCalled();
  });

  it('uses the module-level descriptor so a declared header key is never treated as an extension key', async () => {
    const KEY = 'x_custom_headers';
    // descriptor 通过模块级 getChannelDescriptor 提供（模拟 state 尚未就绪的时序）。
    mockDescriptor = {
      channel_type: 1,
      name: 'x',
      panel_type: 'manual',
      capabilities: { supports_custom_headers: true, custom_headers_key: KEY },
    };
    const envelope = validEnvelope();
    envelope.channel.config = JSON.stringify({
      [KEY]: { Authorization: 'Bearer secret' },
      ext_flag: true,
    });
    readText.mockResolvedValue(JSON.stringify(envelope));

    await renderPage();
    await clickPaste();

    expect(showSuccess).toHaveBeenCalledWith(PASTE_SUCCESS_KEY);
    // 声明的请求头键必须从扩展编辑器排除，扩展键仍保留。
    expect(textareaValue('custom_config')).toContain('ext_flag');
    expect(textareaValue('custom_config')).not.toContain(KEY);
  });

  it('navigates to /channel after a successful create save', async () => {
    // 通过粘贴填入合法数据以满足新建校验（name/key/至少一个模型）。
    readText.mockResolvedValue(JSON.stringify(validEnvelope()));
    API.post.mockResolvedValue({ data: { success: true, message: '' } });
    await renderPage();

    await clickPaste();
    expect(mockNavigate).not.toHaveBeenCalled();

    await clickSubmit();

    expect(API.post).toHaveBeenCalledTimes(1);
    expect(mockNavigate).toHaveBeenCalledTimes(1);
    expect(mockNavigate).toHaveBeenCalledWith('/channel');
  });

  it('does NOT navigate when the create save returns success:false', async () => {
    readText.mockResolvedValue(JSON.stringify(validEnvelope()));
    API.post.mockResolvedValue({
      data: { success: false, message: 'create-failed' },
    });
    await renderPage();

    await clickPaste();
    await clickSubmit();

    expect(API.post).toHaveBeenCalledTimes(1);
    expect(showError).toHaveBeenCalledWith('create-failed');
    expect(mockNavigate).not.toHaveBeenCalled();
  });

  it('awaits the descriptor list so a cold cache still owns the declared header key', async () => {
    const KEY = 'x_custom_headers';
    mockDescriptor = {
      channel_type: 1,
      name: 'x',
      panel_type: 'manual',
      capabilities: { supports_custom_headers: true, custom_headers_key: KEY },
    };
    const envelope = validEnvelope();
    envelope.channel.config = JSON.stringify({
      [KEY]: { Authorization: 'Bearer secret' },
      ext_flag: true,
    });
    readText.mockResolvedValue(JSON.stringify(envelope));

    // 冷缓存：挂载时 loadChannelDescriptors 被推迟，descriptor 缓存尚未就绪。
    let resolveDescriptors;
    const deferred = new Promise((resolve) => {
      resolveDescriptors = () => {
        descriptorsReady = true;
        resolve([mockDescriptor]);
      };
    });
    loadChannelDescriptors.mockImplementation(() => deferred);
    getChannelDescriptor.mockImplementation(() =>
      descriptorsReady ? mockDescriptor : undefined
    );

    await renderPage();

    // 先点击粘贴：处理器必须 await 尚未解析的 descriptor 加载。
    const pastePromise = (async () => {
      const button = findPasteButton();
      expect(button).toBeTruthy();
      await act(async () => {
        button.dispatchEvent(new MouseEvent('click', { bubbles: true }));
        await new Promise((r) => setTimeout(r, 0));
      });
    })();
    await pastePromise;
    // 此时 promise 仍未解析 → 表单尚未被填充（证明粘贴在等 descriptor）。
    expect(inputValue('name')).toBe('');

    // 解析 descriptor 后，粘贴继续：声明的请求头键必须被识别为专属键，不得进入扩展编辑器。
    await act(async () => {
      resolveDescriptors();
      await new Promise((r) => setTimeout(r, 0));
    });

    expect(showSuccess).toHaveBeenCalledWith(PASTE_SUCCESS_KEY);
    expect(inputValue('name')).toBe('pasted');
    expect(textareaValue('custom_config')).toContain('ext_flag');
    expect(textareaValue('custom_config')).not.toContain(KEY);
  });

  it('shows the invalid toast and mutates nothing when getChannelModels throws', async () => {
    readText.mockResolvedValue(JSON.stringify(validEnvelope()));
    // 模拟 localStorage.channel_models === "null" 时 getChannelModels 抛错。
    getChannelModels.mockImplementation(() => {
      throw new Error('cannot read models');
    });

    await renderPage();

    showError.mockClear();
    await clickPaste();

    // 派生阶段抛错：提示失败、表单保持原样、无部分写入、无未处理 rejection。
    expect(showError).toHaveBeenCalledWith(PASTE_INVALID_KEY);
    expect(showSuccess).not.toHaveBeenCalled();
    expect(inputValue('name')).toBe('');
    expect(inputValue('key')).toBe('');
    expect(textareaValue('custom_config')).toBe('');
    expect(API.post).not.toHaveBeenCalled();
  });
});
