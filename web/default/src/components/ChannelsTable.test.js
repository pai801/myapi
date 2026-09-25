import React from 'react';
import { createRoot } from 'react-dom/client';
import { act } from 'react-dom/test-utils';

// 与项目其余组件测试一致：直接用 react-dom/client + test-utils 的 act（未安装 @testing-library）。
/* eslint-disable testing-library/no-unnecessary-act */

global.IS_REACT_ACT_ENVIRONMENT = true;

// 复制渠道动作的契约测试：点击复制必须请求完整渠道端点、只把白名单 v1 信封写入剪贴板，
// 且任一失败路径（copy=false / success:false / 请求被拒）都只弹失败提示、绝不弹成功提示。
// i18n 被 mock 成 t:(k)=>k，故按钮文本即 i18n 键本身。

jest.mock('react-i18next', () => ({
  useTranslation: () => ({ t: (k) => k, i18n: { language: 'zh' } }),
}));

jest.mock('react-router-dom', () => ({
  Link: ({ children, ...rest }) => <a {...rest}>{children}</a>,
}));

jest.mock('../helpers', () => ({
  API: { get: jest.fn() },
  copy: jest.fn(),
  loadChannelModels: jest.fn(),
  setPromptShown: jest.fn(),
  shouldShowPrompt: jest.fn(() => false),
  showError: jest.fn(),
  showInfo: jest.fn(),
  showSuccess: jest.fn(),
  timestamp2string: jest.fn(),
}));

jest.mock('../helpers/channelDescriptor', () => ({
  buildChannelOptions: jest.fn(),
  loadChannelDescriptors: jest.fn(),
}));

jest.mock('../helpers/render', () => ({
  renderGroup: jest.fn(),
  renderNumber: jest.fn((n) => n),
}));

// eslint-disable-next-line import/first
import { API, copy, loadChannelModels, showError, showSuccess } from '../helpers';
// eslint-disable-next-line import/first
import {
  buildChannelOptions,
  loadChannelDescriptors,
} from '../helpers/channelDescriptor';
// eslint-disable-next-line import/first
import ChannelsTable from './ChannelsTable';

const COPY_BUTTON_KEY = 'channel.buttons.copy';
const COPY_SUCCESS_KEY = 'channel.messages.copy_success';
const COPY_FAILED_KEY = 'channel.messages.copy_failed';

// 列表行（keyless，与既有列表端点一致）。
const listRow = {
  id: 7,
  type: 1,
  name: 'row-name',
  group: 'default',
  models: 'gpt-3.5-turbo',
  status: 1,
  response_time: 0,
  balance: 0,
  priority: 1,
};

// 完整渠道响应（含 key 与非复制字段；这些额外字段绝不允许进入剪贴板）。
const fullChannel = {
  id: 7,
  type: 1,
  name: 'row-name',
  group: 'default,pro',
  models: 'gpt-3.5-turbo,gpt-4',
  key: 'sk-secret-value',
  base_url: null,
  other: null,
  model_mapping: null,
  system_prompt: null,
  priority: null,
  config: '',
  status: 1,
  quota: 12345,
  created_time: 111,
};

describe('ChannelsTable copy action', () => {
  let container;
  let root;

  beforeEach(() => {
    buildChannelOptions.mockImplementation(() => []);
    loadChannelDescriptors.mockImplementation(() => Promise.resolve([]));
    loadChannelModels.mockImplementation(() => Promise.resolve([]));
    API.get.mockImplementation((url) => {
      if (url === '/api/channel/?p=0') {
        return Promise.resolve({
          data: { success: true, message: '', data: [{ ...listRow }] },
        });
      }
      if (url === '/api/channel/copy/7') {
        return Promise.resolve({
          data: { success: true, message: '', data: { ...fullChannel } },
        });
      }
      return Promise.resolve({ data: { success: true, message: '', data: [] } });
    });
    copy.mockResolvedValue(true);

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
      root.render(<ChannelsTable />);
    });
    await act(async () => {
      await new Promise((resolve) => setTimeout(resolve, 0));
    });
  }

  function findCopyButton() {
    return Array.from(container.querySelectorAll('button')).find((b) =>
      b.textContent.includes(COPY_BUTTON_KEY)
    );
  }

  async function clickCopy() {
    const button = findCopyButton();
    expect(button).toBeTruthy();
    await act(async () => {
      button.dispatchEvent(new MouseEvent('click', { bubbles: true }));
      await new Promise((resolve) => setTimeout(resolve, 0));
    });
  }

  it('requests the full-channel endpoint and writes only the v1 whitelist envelope', async () => {
    await renderPage();

    await clickCopy();

    expect(API.get).toHaveBeenCalledWith('/api/channel/copy/7');
    expect(copy).toHaveBeenCalledTimes(1);

    const envelope = JSON.parse(copy.mock.calls[0][0]);
    // 顶层恰为格式标记 + channel 两项。
    expect(Object.keys(envelope).sort()).toEqual(['channel', 'myapi_channel']);
    expect(envelope.myapi_channel).toBe(1);

    // channel 内字段集合必须恰好等于白名单（不得夹带 id/status/quota 等）。
    expect(Object.keys(envelope.channel).sort()).toEqual(
      [
        'base_url',
        'config',
        'group',
        'key',
        'model_mapping',
        'models',
        'name',
        'other',
        'priority',
        'system_prompt',
        'type',
      ].sort()
    );

    // group/models 是逗号字符串，绝不是 groups 或数组。
    expect(envelope.channel.group).toBe('default,pro');
    expect(envelope.channel).not.toHaveProperty('groups');
    expect(typeof envelope.channel.models).toBe('string');
    expect(envelope.channel.models).toBe('gpt-3.5-turbo,gpt-4');

    // Go 可空指针归一：null -> ''，priority null -> 数字 1，config 空串 -> '{}'。
    expect(envelope.channel.base_url).toBe('');
    expect(envelope.channel.other).toBe('');
    expect(envelope.channel.model_mapping).toBe('');
    expect(envelope.channel.system_prompt).toBe('');
    expect(envelope.channel.priority).toBe(1);
    expect(typeof envelope.channel.priority).toBe('number');
    expect(envelope.channel.config).toBe('{}');
    expect(typeof envelope.channel.config).toBe('string');

    // 非白名单字段绝不泄漏。
    expect(envelope.channel).not.toHaveProperty('id');
    expect(envelope.channel).not.toHaveProperty('status');
    expect(envelope.channel).not.toHaveProperty('quota');

    expect(showSuccess).toHaveBeenCalledWith(COPY_SUCCESS_KEY);
    expect(showError).not.toHaveBeenCalled();
  });

  it('shows the failure toast (and no success) when copy() resolves false', async () => {
    copy.mockResolvedValue(false);
    await renderPage();

    await clickCopy();

    expect(showError).toHaveBeenCalledWith(COPY_FAILED_KEY);
    expect(showSuccess).not.toHaveBeenCalled();
  });

  it('shows the failure toast (and no success) when the API returns success:false', async () => {
    API.get.mockImplementation((url) => {
      if (url === '/api/channel/?p=0') {
        return Promise.resolve({
          data: { success: true, message: '', data: [{ ...listRow }] },
        });
      }
      return Promise.resolve({
        data: { success: false, message: 'boom', data: null },
      });
    });
    await renderPage();

    await clickCopy();

    expect(showError).toHaveBeenCalledWith('boom');
    expect(copy).not.toHaveBeenCalled();
    expect(showSuccess).not.toHaveBeenCalled();
  });

  it('shows the failure toast (and no success) when the request is rejected', async () => {
    API.get.mockImplementation((url) => {
      if (url === '/api/channel/?p=0') {
        return Promise.resolve({
          data: { success: true, message: '', data: [{ ...listRow }] },
        });
      }
      return Promise.reject(new Error('network'));
    });
    await renderPage();

    await clickCopy();

    expect(showError).toHaveBeenCalledWith(COPY_FAILED_KEY);
    expect(copy).not.toHaveBeenCalled();
    expect(showSuccess).not.toHaveBeenCalled();
  });
});
