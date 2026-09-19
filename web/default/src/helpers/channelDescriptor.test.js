import {
  descriptorOAuth,
  pickI18n,
  resolvePanelType,
} from './channelDescriptor';

// 描述符驱动的面板选择与 OAuth 元数据解析的纯函数单测（PRD §5.12 层2）。
// 不依赖网络 / 渲染：仅验证「清单 → 渲染器类型」与「OAuth 元数据可用性」的判定。

const oauthDescriptor = {
  channel_type: 54,
  panel_type: 'oauth-device-code',
  panel: {
    oauth: {
      start_path: '/channel/ext/login/start',
      poll_path: '/channel/ext/login/poll',
      params: { realm: 'cn' },
    },
  },
};

describe('descriptorOAuth', () => {
  it('returns oauth meta when both start and poll paths are present', () => {
    expect(descriptorOAuth(oauthDescriptor)).toEqual(
      oauthDescriptor.panel.oauth
    );
  });

  it('returns null when panel/oauth is missing', () => {
    expect(descriptorOAuth({ channel_type: 1, panel_type: 'manual' })).toBeNull();
    expect(descriptorOAuth({ panel: {} })).toBeNull();
    expect(descriptorOAuth(undefined)).toBeNull();
  });

  it('returns null when either path is missing', () => {
    expect(
      descriptorOAuth({ panel: { oauth: { start_path: '/x' } } })
    ).toBeNull();
    expect(
      descriptorOAuth({ panel: { oauth: { poll_path: '/y' } } })
    ).toBeNull();
  });
});

describe('resolvePanelType', () => {
  it('returns manual for manual descriptors', () => {
    expect(resolvePanelType({ panel_type: 'manual' })).toBe('manual');
  });

  it('returns the oauth type when oauth metadata is complete', () => {
    expect(resolvePanelType(oauthDescriptor)).toBe('oauth-device-code');
    expect(
      resolvePanelType({
        panel_type: 'oauth-authorization-code',
        panel: { oauth: { start_path: '/a', poll_path: '/b' } },
      })
    ).toBe('oauth-authorization-code');
  });

  it('falls back to manual for oauth types without usable metadata', () => {
    expect(resolvePanelType({ panel_type: 'oauth-device-code' })).toBe('manual');
    expect(
      resolvePanelType({ panel_type: 'oauth-device-code', panel: { oauth: {} } })
    ).toBe('manual');
  });

  it('falls back to manual for unknown / missing types', () => {
    expect(resolvePanelType({ panel_type: 'something-else' })).toBe('manual');
    expect(resolvePanelType(undefined)).toBe('manual');
    expect(resolvePanelType(null)).toBe('manual');
  });
});

describe('pickI18n', () => {
  it('picks exact language, then base language, then fallback', () => {
    const map = { zh: '中文', en: 'English' };
    expect(pickI18n(map, 'fallback', 'zh')).toBe('中文');
    expect(pickI18n(map, 'fallback', 'en-US')).toBe('English');
    expect(pickI18n(map, 'fallback', 'fr')).toBe('fallback');
    expect(pickI18n(undefined, 'fallback', 'zh')).toBe('fallback');
  });
});
