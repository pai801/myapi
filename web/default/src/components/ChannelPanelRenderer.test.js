import React from 'react';
import { createRoot } from 'react-dom/client';
import { act } from 'react-dom/test-utils';

// 本测试直接用 react-dom/client + test-utils 的 act（项目未安装 @testing-library），
// 因此 testing-library 的 act 规则在此属误报。
/* eslint-disable testing-library/no-unnecessary-act */

// React 18 需要显式声明处于 act 环境，否则每次 act() 都会打印未包裹警告。
global.IS_REACT_ACT_ENVIRONMENT = true;

// 通用面板渲染器的组件级验证（PRD §5.12 层2 / AC8 前半段）：
// 给定扩展渠道的 descriptor，面板应渲染出「开始登录」按钮；点击后应把 start 请求发到
// **descriptor 下发的路径**（而非硬编码），并带上 descriptor 声明的参数。
//
// 说明：不引入 @testing-library（项目未安装），直接用 react-dom/client + test-utils 的 act。

jest.mock('react-i18next', () => ({
  useTranslation: () => ({ t: (k) => k, i18n: { language: 'zh' } }),
}));

jest.mock('../helpers', () => ({
  API: { get: jest.fn(), post: jest.fn() },
  showError: jest.fn(),
  showSuccess: jest.fn(),
  showWarning: jest.fn(),
}));

// eslint-disable-next-line import/first
import { API } from '../helpers';
// eslint-disable-next-line import/first
import ChannelPanelRenderer from './ChannelPanelRenderer';

const extDescriptor = {
  channel_type: 54,
  name: '扩展渠道',
  name_i18n: { zh: '扩展渠道', en: 'Ext Channel' },
  panel_type: 'oauth-device-code',
  panel: {
    oauth: {
      start_path: '/channel/ext/login/start',
      poll_path: '/channel/ext/login/poll',
      params: { realm: 'cn' },
    },
  },
};

describe('ChannelPanelRenderer (oauth)', () => {
  let container;
  let root;

  beforeEach(() => {
    container = document.createElement('div');
    document.body.appendChild(container);
    root = createRoot(container);
    // jsdom 未实现 window.open，替换为返回占位窗口的桩，避免噪声与拦截。
    window.open = jest.fn(() => ({ closed: false, location: {}, close() {} }));
  });

  afterEach(() => {
    act(() => root.unmount());
    container.remove();
    jest.clearAllMocks();
  });

  it('renders the start button for an oauth descriptor', () => {
    act(() => {
      root.render(
        <ChannelPanelRenderer
          descriptor={extDescriptor}
          onCredential={() => {}}
          disabled={false}
        />
      );
    });
    expect(container.textContent).toContain('channel.edit.oauth.button_start');
    expect(container.textContent).toContain('扩展渠道');
  });

  it('falls back to the manual panel when oauth metadata is missing', () => {
    act(() => {
      root.render(
        <ChannelPanelRenderer
          descriptor={{ ...extDescriptor, panel: {} }}
          disabled={false}
        />
      );
    });
    expect(container.textContent).not.toContain(
      'channel.edit.oauth.button_start'
    );
  });

  it('posts the start request to the descriptor-declared path with its params', async () => {
    API.post.mockResolvedValue({
      data: { sessionId: 'sess-1', authUrl: 'https://example.com/auth' },
    });

    act(() => {
      root.render(
        <ChannelPanelRenderer
          descriptor={extDescriptor}
          onCredential={() => {}}
          disabled={false}
        />
      );
    });

    const button = Array.from(container.querySelectorAll('button')).find((b) =>
      b.textContent.includes('channel.edit.oauth.button_start')
    );
    expect(button).toBeTruthy();

    await act(async () => {
      button.dispatchEvent(new MouseEvent('click', { bubbles: true }));
    });

    expect(API.post).toHaveBeenCalledWith('/api/channel/ext/login/start', {
      realm: 'cn',
    });
  });
});
