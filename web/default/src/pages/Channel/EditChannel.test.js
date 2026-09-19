import React from 'react';
import { createRoot } from 'react-dom/client';
import { act } from 'react-dom/test-utils';

// 本测试直接用 react-dom/client + test-utils 的 act（项目未安装 @testing-library），
// 因此 testing-library 的 act 规则在此属误报。
/* eslint-disable testing-library/no-unnecessary-act */

// React 18 需要显式声明处于 act 环境，否则每次 act() 都会打印未包裹警告。
global.IS_REACT_ACT_ENVIRONMENT = true;

// EditChannel 的「自定义出站请求头」契约测试（跨仓契约：前端不硬编码持久化键名）。
//
// 契约：渠道 config JSON 里 custom headers 的持久化键名由后端下发的 descriptor 声明
// （capabilities.custom_headers_key），前端据此渲染编辑器并在提交时写入该键名，
// 绝不硬编码 config 的持久化键名。本测试守护这条契约：descriptor 一变，行为必须变。
//
// 说明：i18n 被 mock 成 t:(k)=>k，故页面文本即 i18n 键本身，便于用键名断言。
// CRA 的 jest 配置开启了 resetMocks，工厂里的实现会被逐用例清空，
// 因此所有 mockImplementation 一律在 beforeEach 里重新装配。

jest.mock('react-i18next', () => ({
  useTranslation: () => ({ t: (k) => k, i18n: { language: 'zh' } }),
}));

// 走「加载已有渠道」路径：useParams 返回 id，页面进入 isEdit 分支。
jest.mock('react-router-dom', () => ({
  useParams: () => ({ id: '1' }),
  useNavigate: () => jest.fn(),
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

// 面板渲染器与本契约无关，桩掉以减少无关依赖（OAuth 面板等）。
jest.mock('../../components/ChannelPanelRenderer', () => ({
  __esModule: true,
  default: () => null,
}));

// 每个用例注入的 descriptor / 渠道响应数据（须以 mock 前缀命名，供 jest-hoist 工厂闭包引用）。
let mockDescriptor = null;
let mockChannelData = null;

jest.mock('../../helpers/channelDescriptor', () => ({
  loadChannelDescriptors: jest.fn(),
  findDescriptor: jest.fn(),
  getChannelDescriptor: jest.fn(),
  buildChannelOptions: jest.fn(),
  descriptorKeyPrompt: jest.fn(),
}));

// eslint-disable-next-line import/first
import {
  API,
  getChannelModels,
  verifyJSON,
} from '../../helpers';
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

const KEY = 'x_key';
const EDITOR_TITLE_KEY = 'channel.edit.custom_headers.title';
const SUBMIT_KEY = 'channel.edit.buttons.submit';
// 一个不该被写入的硬编码键名（中性占位：真实键名由清单下发，本常量只用于反证硬编码路径）。
const HARDCODED_KEY = 'hardcoded_custom_headers';

// 扩展渠道 descriptor；capabilities 由各用例注入，精确驱动被守护的双条件门控。
function descriptorWith(capabilities) {
  return {
    channel_type: 54,
    name: '扩展渠道',
    name_i18n: { zh: '扩展渠道' },
    panel_type: 'manual',
    capabilities,
  };
}

// 后端返回的已有渠道数据；models 非空以通过提交前的模型校验。
function channelResponse(config) {
  return {
    id: 1,
    type: 54,
    name: 'x',
    key: '',
    base_url: '',
    models: 'gpt-3.5-turbo',
    group: 'default',
    model_mapping: '',
    config,
  };
}

describe('EditChannel custom-headers contract', () => {
  let container;
  let root;

  beforeEach(() => {
    // 所有实现必须在此重新装配（CRA resetMocks 会清空上一用例的实现）。
    loadChannelDescriptors.mockImplementation(() =>
      Promise.resolve(mockDescriptor ? [mockDescriptor] : [])
    );
    findDescriptor.mockImplementation(() => mockDescriptor);
    getChannelDescriptor.mockImplementation(() => mockDescriptor);
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
      return Promise.resolve({
        data: { success: true, message: '', data: mockChannelData },
      });
    });
    API.put.mockResolvedValue({ data: { success: true, message: '' } });
    API.post.mockResolvedValue({ data: { success: true, message: '' } });

    container = document.createElement('div');
    document.body.appendChild(container);
    root = createRoot(container);
  });

  afterEach(() => {
    act(() => root.unmount());
    container.remove();
    mockDescriptor = null;
    mockChannelData = null;
  });

  // 渲染页面并冲刷异步加载（descriptors + loadChannel + fetchModels + fetchGroups）。
  async function renderPage() {
    await act(async () => {
      root.render(<EditChannel />);
    });
    await act(async () => {
      await new Promise((resolve) => setTimeout(resolve, 0));
    });
  }

  function findSubmitButton() {
    return Array.from(container.querySelectorAll('button')).find((b) =>
      b.textContent.includes(SUBMIT_KEY)
    );
  }

  it('renders the editor when descriptor declares capability AND a key name', async () => {
    mockDescriptor = descriptorWith({
      supports_custom_headers: true,
      custom_headers_key: KEY,
    });
    mockChannelData = channelResponse('{}');

    await renderPage();

    expect(container.textContent).toContain(EDITOR_TITLE_KEY);
  });

  it('does NOT render when capability is true but the key name is empty', async () => {
    mockDescriptor = descriptorWith({
      supports_custom_headers: true,
      custom_headers_key: '',
    });
    mockChannelData = channelResponse('{}');

    await renderPage();

    // 正向前置：页面必须已成功渲染，否则「编辑器不存在」是假绿（整体 return null 也会通过否定断言）。
    expect(container.textContent).toContain(SUBMIT_KEY);
    expect(container.textContent).not.toContain(EDITOR_TITLE_KEY);
  });

  it('does NOT render when capability is false even if a key name is provided', async () => {
    mockDescriptor = descriptorWith({
      supports_custom_headers: false,
      custom_headers_key: KEY,
    });
    mockChannelData = channelResponse('{}');

    await renderPage();

    // 正向前置：页面必须已成功渲染，否则「编辑器不存在」是假绿（整体 return null 也会通过否定断言）。
    expect(container.textContent).toContain(SUBMIT_KEY);
    expect(container.textContent).not.toContain(EDITOR_TITLE_KEY);
  });

  it('submits payload keyed by the descriptor-provided name, never the hardcoded one', async () => {
    mockDescriptor = descriptorWith({
      supports_custom_headers: true,
      custom_headers_key: KEY,
    });
    mockChannelData = channelResponse(
      JSON.stringify({ [KEY]: { Authorization: 'Bearer secret' } })
    );

    await renderPage();

    // 已有渠道已从 config 回填出一行请求头，保存时应把它写回下发的键名。
    const submitButton = findSubmitButton();
    expect(submitButton).toBeTruthy();

    await act(async () => {
      submitButton.dispatchEvent(new MouseEvent('click', { bubbles: true }));
      await new Promise((resolve) => setTimeout(resolve, 0));
    });

    expect(API.put).toHaveBeenCalledTimes(1);
    const payload = API.put.mock.calls[0][1];
    const parsedConfig = JSON.parse(payload.config);

    expect(parsedConfig).toHaveProperty(KEY);
    expect(parsedConfig[KEY]).toEqual({ Authorization: 'Bearer secret' });
    // 键集合必须恰好等于清单下发的键名：写入任何其它键（拼错名、硬编码回退名等）都会破坏该集合。
    // 仅断言「含 KEY」不足以证明，因为回填路径会把渠道原有键原样保留，导致断言与写入键名无关。
    expect(Object.keys(parsedConfig).sort()).toEqual([KEY]);
    expect(parsedConfig).not.toHaveProperty(HARDCODED_KEY);
  });

  it('drops the config key when the header set collapses to empty', async () => {
    mockDescriptor = descriptorWith({
      supports_custom_headers: true,
      custom_headers_key: KEY,
    });
    // 渠道预置 config 为空对象：加载后回填默认空行，提交时没有任何非空请求头可写入。
    mockChannelData = channelResponse('{}');

    await renderPage();

    const submitButton = findSubmitButton();
    expect(submitButton).toBeTruthy();

    await act(async () => {
      submitButton.dispatchEvent(new MouseEvent('click', { bubbles: true }));
      await new Promise((resolve) => setTimeout(resolve, 0));
    });

    expect(API.put).toHaveBeenCalledTimes(1);
    const payload = API.put.mock.calls[0][1];
    const parsedConfig = JSON.parse(payload.config);

    // 空对象必须被删除，不得残留 { <key>: {} }。
    expect(parsedConfig).not.toHaveProperty(KEY);
    expect(Object.keys(parsedConfig)).toEqual([]);
  });
});
