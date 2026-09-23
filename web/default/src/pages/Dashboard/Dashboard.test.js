import React from 'react';
import { createRoot } from 'react-dom/client';
import { act } from 'react-dom/test-utils';

// 本测试沿用仓库既有方式：react-dom/client + test-utils 的 act（项目未安装 @testing-library），
// 因此 testing-library 的 act 规则在此属误报。
/* eslint-disable testing-library/no-unnecessary-act */

// React 18 需要显式声明处于 act 环境，否则每次 act() 都会打印未包裹警告。
global.IS_REACT_ACT_ENVIRONMENT = true;

// recharts 的 ResponsiveContainer 依赖 ResizeObserver，jsdom 未实现；
// 用同步回调的桩给出非零尺寸，图表才会真正渲染。
class ResizeObserverStub {
  constructor(callback) {
    this.callback = callback;
  }
  observe(target) {
    this.callback([{ target, contentRect: { width: 600, height: 300 } }], this);
  }
  unobserve() {}
  disconnect() {}
}
global.ResizeObserver = ResizeObserverStub;

// i18n 默认以「键透传」模式 mock：t:(k)=>k，页面文本即 i18n 键本身，便于用键名断言。
// 需要验证「语言切换 → 文案随之变化」时，可经 mockI18n.changeLanguage 切到真实 zh/en 资源。
const mockI18n = {
  language: 'keys',
  changeLanguage: (lng) => {
    mockI18n.language = lng;
    return Promise.resolve();
  },
};

jest.mock('react-i18next', () => {
  const zh = require('../../locales/zh/translation.json');
  const en = require('../../locales/en/translation.json');
  const lookup = (resource, key) =>
    key
      .split('.')
      .reduce((acc, part) => (acc == null ? acc : acc[part]), resource);
  return {
    useTranslation: () => ({
      t: (key) => {
        // 'keys' 模式保持键透传，既有用例按 key 断言。
        if (mockI18n.language === 'keys') return key;
        const resource = mockI18n.language === 'en' ? en : zh;
        const value = lookup(resource, key);
        return typeof value === 'string' ? value : key;
      },
      i18n: mockI18n,
    }),
  };
});

// 角色由 isAdmin() 决定，传输由 helpers 暴露的 Axios 边界 API 承担：
// 同时 mock 二者，既精确驱动普通用户/管理员身份，又让 Dashboard/api.js 走真实的
// 请求封装（路径/查询参数/信封解包都得到验证），并隔离 toastify 等无关副作用。
jest.mock('../../helpers', () => ({
  API: { get: jest.fn() },
  isAdmin: jest.fn(() => false),
}));

// eslint-disable-next-line import/first
import { API, isAdmin } from '../../helpers';
// eslint-disable-next-line import/first
import Dashboard from './index';
// eslint-disable-next-line import/first
import { fetchDashboardAggregate, fetchDashboardSummary } from './api';
// eslint-disable-next-line import/first
import { SummaryCards } from './components/SummaryCards';
// eslint-disable-next-line import/first
import {
  DIMENSION_SERIES_VALUES,
  DimensionChart,
  DimensionTooltip,
  buildDimensionProportionData,
  buildDimensionTopData,
  buildDimensionTrendData,
} from './components/DimensionChart';
// eslint-disable-next-line import/first
import { TrendCharts, buildOverviewTrendData } from './components/TrendCharts';
// eslint-disable-next-line import/first
import {
  loadDashboardChartPreferences,
  saveDashboardChartPreference,
} from './preferences';
// eslint-disable-next-line import/first
import zhTranslation from '../../locales/zh/translation.json';
// eslint-disable-next-line import/first
import enTranslation from '../../locales/en/translation.json';

// 图表偏好持久化的唯一存储 key（契约）。
const PREFERENCES_STORAGE_KEY = 'dashboard_chart_preferences';

// 契约默认偏好：每个维度 { view: 'trend', topN: 10 }。
const DEFAULT_PREFERENCES = {
  model: { view: 'trend', topN: 10 },
  token: { view: 'trend', topN: 10 },
  channel: { view: 'trend', topN: 10 },
  user: { view: 'trend', topN: 10 },
};

// 契约中的扁平聚合行，由 aggregate 端点返回。
const aggregateRows = [
  {
    bucket: '2026-09-23 14:00',
    key: 'gpt-4o',
    requests: 12,
    quota: 345000,
    prompt_tokens: 1000,
    completion_tokens: 300,
    cached_tokens: 250,
    avg_ttft: 150,
    avg_elapsed: 900,
  },
  {
    bucket: '2026-09-23 15:00',
    key: 'deepseek-v4',
    requests: 3,
    quota: 120000,
    prompt_tokens: 400,
    completion_tokens: 100,
    cached_tokens: 0,
    avg_ttft: 80,
    avg_elapsed: 500,
  },
];

// summary 端点返回的 KPI 汇总对象。
const summaryData = {
  total_requests: 15,
  total_quota: 465000,
  total_tokens: 1800,
  cached_tokens: 250,
  cache_hit_rate: 0.25,
  avg_ttft: 115,
  avg_elapsed: 700,
  avg_rpm: 2,
  avg_tpm: 100,
};

// 按端点分发成功信封的默认实现。
function respondByEndpoint(url) {
  if (String(url).includes('/api/user/dashboard/aggregate')) {
    return Promise.resolve({
      data: { success: true, message: '', data: aggregateRows },
    });
  }
  return Promise.resolve({
    data: { success: true, message: '', data: summaryData },
  });
}

function mount(element) {
  const container = document.createElement('div');
  document.body.appendChild(container);
  const root = createRoot(container);
  act(() => {
    root.render(element);
  });
  return { container, root };
}

function unmount({ container, root }) {
  act(() => root.unmount());
  container.remove();
}

function findButton(container, label) {
  return Array.from(container.querySelectorAll('button')).find((button) =>
    button.textContent.includes(label)
  );
}

// React 受控输入通过 value tracker 去重，直接赋值 .value 会被忽略；
// 必须经原生 setter 写入后再派发 input 事件，onChange 才会触发。
function setInputValue(input, value) {
  const setter = Object.getOwnPropertyDescriptor(
    window.HTMLInputElement.prototype,
    'value'
  ).set;
  setter.call(input, value);
  input.dispatchEvent(new Event('input', { bubbles: true }));
}

// jsdom 的 MouseEvent 不会从 clientX/clientY 推导 pageX/pageY，而 recharts 用
// pageX/pageY 计算激活点；这里显式补上，并向图表容器派发 mousemove 以触发 tooltip。
function hoverChart(container, { x, y }) {
  const wrapper = container.querySelector('.recharts-wrapper');
  const event = new MouseEvent('mousemove', {
    bubbles: true,
    clientX: x,
    clientY: y,
  });
  Object.defineProperty(event, 'pageX', { value: x });
  Object.defineProperty(event, 'pageY', { value: y });
  wrapper.dispatchEvent(event);
}

// 记录每次 API.get 的 URL，便于断言路径与查询参数。
function requestedUrls() {
  return API.get.mock.calls.map(([url]) => url);
}

beforeEach(() => {
  API.get.mockReset();
  API.get.mockImplementation(respondByEndpoint);
  isAdmin.mockReset();
  isAdmin.mockReturnValue(false);
  // 逐用例复位 i18n 模式，避免语言切换用例污染按 key 断言的既有用例。
  mockI18n.language = 'keys';
  // 偏好持久化用例会写入 localStorage；逐用例清空，避免跨用例串扰。
  window.localStorage.clear();
});

// 选择 semantic-ui Dropdown（非原生 select）中的 Top-N 选项：展开后点选菜单项。
async function selectTopN(container, value) {
  const dropdown = container.querySelector('.dimension-topn-dropdown');
  if (!dropdown) throw new Error('top-N dropdown not found');
  await act(async () => {
    dropdown.dispatchEvent(new MouseEvent('mousedown', { bubbles: true }));
    await new Promise((resolve) => setTimeout(resolve, 0));
  });
  const items = Array.from(dropdown.querySelectorAll('.item'));
  const target = items.find((item) => item.textContent.trim() === String(value));
  if (!target) throw new Error(`top-N option not found: ${value}`);
  await act(async () => {
    target.dispatchEvent(new MouseEvent('click', { bubbles: true }));
    await new Promise((resolve) => setTimeout(resolve, 0));
  });
}

// 读取当前 localStorage 中解码后的偏好对象。
function storedPreferences() {
  return JSON.parse(window.localStorage.getItem(PREFERENCES_STORAGE_KEY));
}

describe('Dashboard structure and shared filters', () => {
  it('renders the filter row, presets, granularities, and the overview section', async () => {
    let mounted;
    await act(async () => {
      mounted = mount(<Dashboard />);
    });

    const { container } = mounted;
    // 筛选器文案经 i18n key 渲染（测试将 t mock 为键透传，故断言键本身）。
    expect(container.textContent).toContain('dashboard.filters.start');
    expect(container.textContent).toContain('dashboard.filters.end');
    expect(container.textContent).toContain('dashboard.filters.query');

    // 1/7/14/29 天预设 + 自定义，粒度 hour/day/week。
    ['1', '7', '14', '29', 'custom'].forEach((preset) => {
      expect(
        container.querySelector(`[data-preset="${preset}"]`)
      ).not.toBeNull();
    });
    ['hour', 'day', 'week'].forEach((granularity) => {
      expect(
        container.querySelector(`[data-granularity="${granularity}"]`)
      ).not.toBeNull();
    });

    // 默认分区为概览：KPI 卡片条 + 三条趋势线。
    expect(
      container.querySelector('[data-active-section]').dataset.activeSection
    ).toBe('overview');
    expect(container.textContent).toContain('dashboard.summary.total_requests');
    expect(container.textContent).toContain('dashboard.summary.cache_hit_rate');
    expect(container.textContent).toContain('dashboard.charts.requests.title');
    expect(container.textContent).toContain('dashboard.charts.quota.title');
    expect(container.textContent).toContain('dashboard.charts.tokens.title');

    unmount(mounted);
  });

  it('shows the dimension chart in the model section', async () => {
    let mounted;
    await act(async () => {
      mounted = mount(<Dashboard />);
    });

    const { container } = mounted;
    await act(async () => {
      container
        .querySelector('[data-section="model"]')
        .dispatchEvent(new MouseEvent('click', { bubbles: true }));
    });

    // 维度图三视图切换按钮。
    expect(container.textContent).toContain('dashboard.views.trend');
    expect(container.textContent).toContain('dashboard.views.proportion');
    expect(container.textContent).toContain('dashboard.views.top');
    expect(
      container.querySelector('[data-active-section]').dataset.activeSection
    ).toBe('model');

    unmount(mounted);
  });

  it('owns the request: the page issues one aggregate and one summary request on mount', async () => {
    let mounted;
    await act(async () => {
      mounted = mount(<Dashboard />);
    });

    const urls = requestedUrls();
    expect(urls).toHaveLength(2);
    expect(
      urls.some((url) => url.startsWith('/api/user/dashboard/aggregate?'))
    ).toBe(true);
    expect(
      urls.some((url) => url.startsWith('/api/user/dashboard/summary?'))
    ).toBe(true);

    unmount(mounted);
  });

  it('hides the administrator username input for a common user', async () => {
    let mounted;
    await act(async () => {
      mounted = mount(<Dashboard />);
    });

    expect(mounted.container.querySelector('input[type="text"]')).toBeNull();

    unmount(mounted);
  });

  it('shows the administrator username input for an administrator', async () => {
    isAdmin.mockReturnValue(true);
    let mounted;
    await act(async () => {
      mounted = mount(<Dashboard />);
    });

    expect(
      mounted.container.querySelector('input[type="text"]')
    ).not.toBeNull();

    unmount(mounted);
  });

  it('preserves preset, custom range, and granularity across section changes', async () => {
    let mounted;
    await act(async () => {
      mounted = mount(<Dashboard />);
    });

    const { container } = mounted;
    const click = async (selector) => {
      await act(async () => {
        container
          .querySelector(selector)
          .dispatchEvent(new MouseEvent('click', { bubbles: true }));
      });
    };

    // 选择 14 天预设与 hour 粒度。
    await click('[data-preset="14"]');
    await click('[data-granularity="hour"]');

    // 切到自定义并填写起止日期。
    await click('[data-preset="custom"]');
    const dateInputs = container.querySelectorAll('input[type="date"]');
    expect(dateInputs.length).toBe(2);
    await act(async () => {
      setInputValue(dateInputs[0], '2026-09-01');
    });
    await act(async () => {
      setInputValue(dateInputs[1], '2026-09-10');
    });

    const isActive = (selector) =>
      container.querySelector(selector).classList.contains('active');

    expect(isActive('[data-preset="custom"]')).toBe(true);
    expect(isActive('[data-granularity="hour"]')).toBe(true);

    // 切换到模型分析分区，筛选状态必须保持。
    await click('[data-section="model"]');
    expect(
      container.querySelector('[data-active-section]').dataset.activeSection
    ).toBe('model');
    expect(isActive('[data-preset="custom"]')).toBe(true);
    expect(isActive('[data-granularity="hour"]')).toBe(true);
    const afterSwitch = container.querySelectorAll('input[type="date"]');
    expect(afterSwitch[0].value).toBe('2026-09-01');
    expect(afterSwitch[1].value).toBe('2026-09-10');

    // 切回概览，筛选状态同样保持。
    await click('[data-section="overview"]');
    expect(isActive('[data-preset="custom"]')).toBe(true);
    expect(isActive('[data-granularity="hour"]')).toBe(true);

    unmount(mounted);
  });

  it('applies the selected preset range to both dashboard queries', async () => {
    let mounted;
    await act(async () => {
      mounted = mount(<Dashboard />);
    });

    const { container } = mounted;
    await act(async () => {
      container
        .querySelector('[data-preset="7"]')
        .dispatchEvent(new MouseEvent('click', { bubbles: true }));
    });
    await act(async () => {
      findButton(container, 'dashboard.filters.query').dispatchEvent(
        new MouseEvent('click', { bubbles: true })
      );
    });

    const urls = requestedUrls();
    const aggregateUrl = urls.find((url) => url.includes('/aggregate?'));
    const summaryUrl = urls.find((url) => url.includes('/summary?'));
    [aggregateUrl, summaryUrl].forEach((url) => {
      expect(url).toContain('start_timestamp=');
      expect(url).toContain('end_timestamp=');
    });
    expect(aggregateUrl).toContain('granularity=day');
    expect(summaryUrl).toContain('granularity=day');

    unmount(mounted);
  });

  it('never emits a username scope for a common user', async () => {
    let mounted;
    await act(async () => {
      mounted = mount(<Dashboard />);
    });

    const { container } = mounted;
    // 普通用户不渲染用户名输入框。
    expect(container.querySelector('input[type="text"]')).toBeNull();

    // 切换分区后再次查询，仍不得带上 username 参数。
    await act(async () => {
      container
        .querySelector('[data-section="model"]')
        .dispatchEvent(new MouseEvent('click', { bubbles: true }));
    });
    await act(async () => {
      findButton(container, 'dashboard.filters.query').dispatchEvent(
        new MouseEvent('click', { bubbles: true })
      );
    });

    requestedUrls().forEach((url) => {
      expect(url).not.toContain('username=');
    });

    unmount(mounted);
  });

  it('sends the administrator username scope when provided', async () => {
    isAdmin.mockReturnValue(true);
    let mounted;
    await act(async () => {
      mounted = mount(<Dashboard />);
    });

    const { container } = mounted;
    const input = container.querySelector('input[type="text"]');
    await act(async () => {
      setInputValue(input, 'alice');
    });
    await act(async () => {
      findButton(container, 'dashboard.filters.query').dispatchEvent(
        new MouseEvent('click', { bubbles: true })
      );
    });

    // 取最近一次查询（用户名设置后触发）发出的 URL，而非挂载时的初始请求。
    const urls = requestedUrls();
    const aggregateUrl = urls
      .filter((url) => url.includes('/aggregate?'))
      .pop();
    const summaryUrl = urls.filter((url) => url.includes('/summary?')).pop();
    expect(aggregateUrl).toContain('username=alice');
    expect(summaryUrl).toContain('username=alice');

    unmount(mounted);
  });
});

describe('Dashboard role-aware tabs', () => {
  // 角色化 Tab 的五分区定义：概览/模型分析/令牌分析对所有登录用户可见，
  // 渠道分析/用户分析仅管理员可见。菜单项上带 data-section 便于断言。
  const paneKeys = (container) =>
    Array.from(container.querySelectorAll('[data-section]')).map(
      (item) => item.dataset.section
    );

  const clickSection = async (container, key) => {
    await act(async () => {
      container
        .querySelector(`[data-section="${key}"]`)
        .dispatchEvent(new MouseEvent('click', { bubbles: true }));
    });
  };

  it('shows exactly three panes and hides restricted panes for a common user', async () => {
    let mounted;
    await act(async () => {
      mounted = mount(<Dashboard />);
    });

    const { container } = mounted;
    // 普通用户仅可见概览 / 模型分析 / 令牌分析。
    expect(paneKeys(container)).toEqual(['overview', 'model', 'token']);
    // 分区名经 i18n key 渲染（测试将 t mock 为键透传）。
    expect(container.textContent).toContain('dashboard.panes.overview');
    expect(container.textContent).toContain('dashboard.panes.model');
    expect(container.textContent).toContain('dashboard.panes.token');
    // 渠道分析与用户分析对普通用户完全缺席。
    expect(container.querySelector('[data-section="channel"]')).toBeNull();
    expect(container.querySelector('[data-section="user"]')).toBeNull();
    expect(container.textContent).not.toContain('dashboard.panes.channel');
    expect(container.textContent).not.toContain('dashboard.panes.user');

    unmount(mounted);
  });

  it('shows all five panes for an administrator', async () => {
    isAdmin.mockReturnValue(true);
    let mounted;
    await act(async () => {
      mounted = mount(<Dashboard />);
    });

    const { container } = mounted;
    expect(paneKeys(container)).toEqual([
      'overview',
      'model',
      'token',
      'channel',
      'user',
    ]);
    ['overview', 'model', 'token', 'channel', 'user'].forEach((key) => {
      expect(container.textContent).toContain(`dashboard.panes.${key}`);
    });

    unmount(mounted);
  });

  it('renders the token dimension chart in the token pane', async () => {
    let mounted;
    await act(async () => {
      mounted = mount(<Dashboard />);
    });

    const { container } = mounted;
    await clickSection(container, 'token');

    expect(
      container.querySelector('[data-active-section]').dataset.activeSection
    ).toBe('token');
    // DimensionChart 以当前维度渲染标题与三视图切换控件。
    expect(container.textContent).toContain('dashboard.dimensions.token');
    expect(container.textContent).toContain('dashboard.views.trend');

    unmount(mounted);
  });

  it('does not request a new dimension for overview/model and refetches only the changed dimension', async () => {
    let mounted;
    await act(async () => {
      mounted = mount(<Dashboard />);
    });

    const { container } = mounted;
    // 挂载时恰好一次 aggregate + 一次 summary。
    expect(API.get.mock.calls.length).toBe(2);

    // 概览与模型分析同属 model 维度：切分区不得产生任何请求。
    await clickSection(container, 'model');
    expect(API.get.mock.calls.length).toBe(2);

    // 切到令牌分析：维度变化必须重新拉取，且只新增一次 aggregate，不重复 summary。
    await clickSection(container, 'token');
    const urls = requestedUrls();
    const aggregateUrls = urls.filter((url) => url.includes('/aggregate?'));
    const summaryUrls = urls.filter((url) => url.includes('/summary?'));
    expect(aggregateUrls).toHaveLength(2);
    expect(summaryUrls).toHaveLength(1);
    expect(aggregateUrls.pop()).toContain('dimension=token');

    // 切回概览（model 维度）：再次触发一次 aggregate，summary 仍不重复。
    await clickSection(container, 'overview');
    const afterBack = requestedUrls();
    expect(afterBack.filter((url) => url.includes('/aggregate?'))).toHaveLength(
      3
    );
    expect(afterBack.filter((url) => url.includes('/summary?'))).toHaveLength(1);

    unmount(mounted);
  });

  it('preserves filters when switching to a different-dimension pane', async () => {
    let mounted;
    await act(async () => {
      mounted = mount(<Dashboard />);
    });

    const { container } = mounted;
    await clickSection(container, 'overview');
    await act(async () => {
      container
        .querySelector('[data-preset="14"]')
        .dispatchEvent(new MouseEvent('click', { bubbles: true }));
    });
    await act(async () => {
      container
        .querySelector('[data-granularity="week"]')
        .dispatchEvent(new MouseEvent('click', { bubbles: true }));
    });

    const isActive = (selector) =>
      container.querySelector(selector).classList.contains('active');

    await clickSection(container, 'token');

    expect(isActive('[data-preset="14"]')).toBe(true);
    expect(isActive('[data-granularity="week"]')).toBe(true);
    // 新维度的请求携带保留后的筛选值。
    const lastAggregate = requestedUrls()
      .filter((url) => url.includes('/aggregate?'))
      .pop();
    expect(lastAggregate).toContain('granularity=week');

    unmount(mounted);
  });
});

describe('Dashboard transport and empty states', () => {
  it('targets the aggregate and summary endpoints with dimension and granularity', async () => {
    let mounted;
    await act(async () => {
      mounted = mount(<Dashboard />);
    });

    const aggregateUrl = requestedUrls().find((url) =>
      url.includes('/api/user/dashboard/aggregate?')
    );
    const summaryUrl = requestedUrls().find((url) =>
      url.includes('/api/user/dashboard/summary?')
    );

    expect(aggregateUrl).toContain('dimension=model');
    expect(aggregateUrl).toContain('granularity=day');
    expect(summaryUrl).not.toContain('dimension=');
    expect(summaryUrl).toContain('granularity=day');

    unmount(mounted);
  });

  it('renders the empty state, not an error, when the aggregate succeeds with no rows', async () => {
    API.get.mockImplementation((url) =>
      String(url).includes('/aggregate')
        ? Promise.resolve({
            data: { success: true, message: '', data: [] },
          })
        : Promise.resolve({
            data: { success: true, message: '', data: summaryData },
          })
    );

    let mounted;
    await act(async () => {
      mounted = mount(<Dashboard />);
    });

    const { container } = mounted;
    expect(container.textContent).toContain('dashboard.empty');
    expect(container.textContent).not.toContain('dashboard.error');

    unmount(mounted);
  });

  it('renders the error state, distinct from empty, when a request rejects', async () => {
    const consoleError = jest
      .spyOn(console, 'error')
      .mockImplementation(() => {});
    API.get.mockImplementation((url) =>
      String(url).includes('/aggregate')
        ? Promise.reject(new Error('network down'))
        : Promise.resolve({
            data: { success: true, message: '', data: summaryData },
          })
    );

    let mounted;
    await act(async () => {
      mounted = mount(<Dashboard />);
    });

    const { container } = mounted;
    expect(container.textContent).toContain('dashboard.error');
    expect(container.textContent).not.toContain('dashboard.empty');

    consoleError.mockRestore();
    unmount(mounted);
  });

  it('treats a success=false envelope as a failure, not an empty success', async () => {
    const consoleError = jest
      .spyOn(console, 'error')
      .mockImplementation(() => {});
    API.get.mockImplementation((url) =>
      String(url).includes('/aggregate')
        ? Promise.resolve({
            data: { success: false, message: 'rejected', data: null },
          })
        : Promise.resolve({
            data: { success: true, message: '', data: summaryData },
          })
    );

    let mounted;
    await act(async () => {
      mounted = mount(<Dashboard />);
    });

    const { container } = mounted;
    expect(container.textContent).toContain('dashboard.error');
    expect(container.textContent).not.toContain('dashboard.empty');

    consoleError.mockRestore();
    unmount(mounted);
  });

  it('fetchDashboardAggregate normalizes empty data to an empty array', async () => {
    API.get.mockResolvedValueOnce({
      data: { success: true, message: '', data: null },
    });

    await expect(
      fetchDashboardAggregate({
        dimension: 'model',
        granularity: 'day',
        startTimestamp: 1,
        endTimestamp: 2,
      })
    ).resolves.toEqual([]);
  });

  it('fetchDashboardSummary returns the unwrapped summary object', async () => {
    API.get.mockResolvedValueOnce({
      data: { success: true, message: '', data: summaryData },
    });

    await expect(
      fetchDashboardSummary({
        granularity: 'day',
        startTimestamp: 1,
        endTimestamp: 2,
      })
    ).resolves.toEqual(summaryData);
  });

  it('rejects on transport failure so callers can distinguish it from empty success', async () => {
    API.get.mockRejectedValueOnce(new Error('boom'));

    await expect(
      fetchDashboardAggregate({
        dimension: 'model',
        granularity: 'day',
        startTimestamp: 1,
        endTimestamp: 2,
      })
    ).rejects.toThrow('boom');
  });

  it('omits username for a common user even when a value is present', async () => {
    isAdmin.mockReturnValue(false);
    API.get.mockResolvedValueOnce({
      data: { success: true, message: '', data: [] },
    });

    await fetchDashboardAggregate({
      dimension: 'model',
      granularity: 'day',
      startTimestamp: 1,
      endTimestamp: 2,
      username: 'alice',
    });

    expect(API.get.mock.calls[0][0]).not.toContain('username=');
  });

  it('sends trimmed username only for an administrator with a non-empty value', async () => {
    isAdmin.mockReturnValue(true);
    API.get.mockResolvedValueOnce({
      data: { success: true, message: '', data: [] },
    });

    await fetchDashboardSummary({
      granularity: 'day',
      startTimestamp: 1,
      endTimestamp: 2,
      username: '  alice  ',
    });
    expect(API.get.mock.calls[0][0]).toContain('username=alice');

    API.get.mockResolvedValueOnce({
      data: { success: true, message: '', data: [] },
    });
    await fetchDashboardSummary({
      granularity: 'day',
      startTimestamp: 1,
      endTimestamp: 2,
      username: '   ',
    });
    expect(API.get.mock.calls[1][0]).not.toContain('username=');
  });
});

describe('SummaryCards metrics', () => {
  it('formats every populated KPI and keeps numeric zero as 0', () => {
    const { container } = mount(
      <SummaryCards
        summary={{
          total_requests: 1234,
          total_quota: 1500000,
          total_tokens: 6000,
          cached_tokens: 250,
          cache_hit_rate: 0.25,
          avg_ttft: 150,
          avg_elapsed: 900,
          avg_rpm: 2,
          avg_tpm: 100,
        }}
      />
    );

    const text = container.textContent;
    expect(text).toContain('1,234');
    expect(text).toContain('1.5M');
    expect(text).toContain('6,000');
    expect(text).toContain('250');
    expect(text).toContain('25.00%');
    expect(text).toContain('150.0 ms');
    expect(text).toContain('900.0 ms');
    expect(text).toContain('2.00');
    expect(text).toContain('100.00');
  });

  it('shows -- for a null summary and for unavailable latency samples', () => {
    const nullMount = mount(<SummaryCards summary={null} />);
    const nullText = nullMount.container.textContent;
    // 九项全部不可用 → 九次占位符。
    expect((nullText.match(/--/g) || []).length).toBe(9);

    const partial = mount(
      <SummaryCards
        summary={{
          total_requests: 0,
          total_quota: 0,
          total_tokens: 0,
          cached_tokens: 0,
          cache_hit_rate: 0,
        }}
      />
    );
    const partialText = partial.container.textContent;
    // 数值零仍展示为 0，而非占位符。
    expect(partialText).toContain('0');
    expect(partialText).toContain('0.00%');
    // 缺失的延迟/RPM/TPM 显示 --。
    expect((partialText.match(/--/g) || []).length).toBe(4);
  });

  it('shows -- for non-finite samples while keeping numeric zero', () => {
    const { container } = mount(
      <SummaryCards
        summary={{
          total_requests: NaN,
          total_quota: Infinity,
          total_tokens: -Infinity,
          cached_tokens: 0,
          cache_hit_rate: NaN,
          avg_ttft: 0,
          avg_elapsed: NaN,
          avg_rpm: Infinity,
          avg_tpm: 0,
        }}
      />
    );
    const text = container.textContent;
    // 非有限样本（NaN/±Infinity）一律视为不可用：total_requests/quota/tokens/
    // cache_hit_rate/avg_elapsed/avg_rpm 共 6 项占位。
    expect((text.match(/--/g) || []).length).toBe(6);
    // 数值零（cached_tokens/avg_ttft/avg_tpm）仍是有效样本。
    expect(text).toContain('0.0 ms');
    expect(text).toContain('0.00');
  });
});

describe('DimensionChart views and immutability', () => {
  it('renders trend, proportion, and top views from the same rows', async () => {
    let mounted;
    await act(async () => {
      mounted = mount(
        <DimensionChart
          dimension='model'
          rows={aggregateRows}
          view='trend'
          topN={10}
          onViewChange={() => {}}
          onTopNChange={() => {}}
        />
      );
    });

    const { container } = mounted;
    // 趋势视图：每个维度键一条折线，当前视图标记为 trend。
    expect(container.querySelectorAll('.recharts-line').length).toBe(2);
    expect(container.querySelector('[data-view]').dataset.view).toBe('trend');

    await act(async () => {
      mounted.root.render(
        <DimensionChart
          dimension='model'
          rows={aggregateRows}
          view='proportion'
          topN={10}
          onViewChange={() => {}}
          onTopNChange={() => {}}
        />
      );
    });
    // 占比视图：单个饼图，视图标记为 proportion。
    expect(container.querySelectorAll('.recharts-pie').length).toBe(1);
    expect(container.querySelectorAll('.recharts-line').length).toBe(0);
    expect(container.querySelector('[data-view]').dataset.view).toBe('proportion');

    await act(async () => {
      mounted.root.render(
        <DimensionChart
          dimension='model'
          rows={aggregateRows}
          view='top'
          topN={10}
          onViewChange={() => {}}
          onTopNChange={() => {}}
        />
      );
    });
    // Top 视图：柱状图，视图标记为 top。
    expect(container.querySelectorAll('.recharts-bar').length).toBe(1);
    expect(container.querySelectorAll('.recharts-pie').length).toBe(0);
    expect(container.querySelector('[data-view]').dataset.view).toBe('top');

    unmount(mounted);
  });

  it('renders the top view as a horizontal bar chart with the category axis on Y', async () => {
    let mounted;
    await act(async () => {
      mounted = mount(
        <DimensionChart
          dimension='model'
          rows={aggregateRows}
          view='top'
          topN={10}
          onViewChange={() => {}}
          onTopNChange={() => {}}
        />
      );
    });

    const { container } = mounted;
    const tickText = (selector) =>
      Array.from(
        container.querySelectorAll(
          `${selector} .recharts-cartesian-axis-tick-value`
        )
      ).map((node) => node.textContent);

    // 横向柱状图：维度键落在 Y 分类轴，数值刻度落在 X 轴。
    expect(tickText('.recharts-yAxis').sort()).toEqual(
      ['deepseek-v4', 'gpt-4o'].sort()
    );
    tickText('.recharts-xAxis').forEach((tick) => {
      expect(Number(tick)).not.toBeNaN();
    });

    // 降序 Top-N：两条数据各渲染一个柱条。
    expect(container.querySelectorAll('.recharts-bar-rectangle').length).toBe(2);

    unmount(mounted);
  });

  it('limits the top view to topN descending bars', async () => {
    let mounted;
    await act(async () => {
      mounted = mount(
        <DimensionChart
          dimension='model'
          rows={aggregateRows}
          view='top'
          topN={1}
          onViewChange={() => {}}
          onTopNChange={() => {}}
        />
      );
    });

    const { container } = mounted;
    // 仅保留 token 总量最高的 gpt-4o（1300 > 500）。
    const yTicks = Array.from(
      container.querySelectorAll(
        '.recharts-yAxis .recharts-cartesian-axis-tick-value'
      )
    ).map((node) => node.textContent);
    expect(yTicks).toEqual(['gpt-4o']);
    expect(container.querySelectorAll('.recharts-bar-rectangle').length).toBe(1);

    unmount(mounted);
  });

  it('renders the empty marker when there are no rows', () => {
    const { container } = mount(
      <DimensionChart
        dimension='model'
        rows={[]}
        view='trend'
        topN={10}
        onViewChange={() => {}}
        onTopNChange={() => {}}
      />
    );
    expect(container.textContent).toContain('dashboard.empty');
  });

  it('switching the view issues no additional request', async () => {
    let mounted;
    await act(async () => {
      mounted = mount(<Dashboard />);
    });

    const { container } = mounted;
    const initialCalls = API.get.mock.calls.length;
    expect(initialCalls).toBe(2);

    // 维度图位于模型分析分区，先切分区（切换分区不得发起请求）。
    await act(async () => {
      container
        .querySelector('[data-section="model"]')
        .dispatchEvent(new MouseEvent('click', { bubbles: true }));
    });
    expect(API.get.mock.calls.length).toBe(initialCalls);

    const proportionButton = findButton(
      container,
      'dashboard.views.proportion'
    );
    expect(proportionButton).toBeTruthy();
    await act(async () => {
      proportionButton.dispatchEvent(
        new MouseEvent('click', { bubbles: true })
      );
    });

    // 视图切换是纯客户端变换，父页面不得重新发起请求。
    expect(API.get.mock.calls.length).toBe(initialCalls);

    // 继续切到 Top 视图同样零请求。
    const topButton = findButton(container, 'dashboard.views.top');
    await act(async () => {
      topButton.dispatchEvent(new MouseEvent('click', { bubbles: true }));
    });
    expect(API.get.mock.calls.length).toBe(initialCalls);

    unmount(mounted);
  });

  it('does not mutate the input rows in any transform', () => {
    const input = [
      {
        bucket: '2026-09-23 15:00',
        key: 'b',
        requests: 1,
        quota: 1,
        prompt_tokens: 2,
        completion_tokens: 3,
      },
      {
        bucket: '2026-09-23 14:00',
        key: 'a',
        requests: 1,
        quota: 1,
        prompt_tokens: 4,
        completion_tokens: 5,
      },
      // 与 X 轴字段/原型键撞名的维度键也不得被转换函数改写。
      {
        bucket: '2026-09-23 14:00',
        key: 'bucket',
        requests: 2,
        quota: 2,
        prompt_tokens: 6,
        completion_tokens: 0,
      },
      {
        bucket: '2026-09-23 14:00',
        key: '__proto__',
        requests: 3,
        quota: 3,
        prompt_tokens: 7,
        completion_tokens: 0,
      },
    ];
    const snapshot = JSON.parse(JSON.stringify(input));

    buildDimensionTrendData(input);
    buildDimensionProportionData(input);
    buildDimensionTopData(input, 1);

    expect(input).toEqual(snapshot);
  });

  it('builds trend points sorted by bucket using total tokens', () => {
    const { keys, points } = buildDimensionTrendData(aggregateRows);
    expect(keys).toEqual(['deepseek-v4', 'gpt-4o']);
    expect(points.map((point) => point.bucket)).toEqual([
      '2026-09-23 14:00',
      '2026-09-23 15:00',
    ]);
    // total tokens = prompt + completion（不含 cached），真值经 Symbol Map 读取。
    expect(points[0][DIMENSION_SERIES_VALUES].get('gpt-4o')).toBe(1300);
    expect(points[0][DIMENSION_SERIES_VALUES].get('deepseek-v4')).toBe(0);
    expect(points[1][DIMENSION_SERIES_VALUES].get('deepseek-v4')).toBe(500);
  });

  it('keeps trend labels and series when a dimension key collides with bucket or __proto__', () => {
    const collisionRows = [
      {
        bucket: '2026-09-23 14:00',
        key: 'bucket',
        prompt_tokens: 10,
        completion_tokens: 5,
      },
      {
        bucket: '2026-09-23 14:00',
        key: '__proto__',
        prompt_tokens: 20,
        completion_tokens: 0,
      },
      {
        bucket: '2026-09-23 15:00',
        key: '__proto__',
        prompt_tokens: 1,
        completion_tokens: 1,
      },
    ];

    const { keys, points } = buildDimensionTrendData(collisionRows);
    expect(keys).toEqual(['__proto__', 'bucket']);

    // X 轴标签必须保持为 bucket 值，不得被同名维度键覆盖。
    expect(points.map((point) => point.bucket)).toEqual([
      '2026-09-23 14:00',
      '2026-09-23 15:00',
    ]);

    // 两个序列的真值都必须完整保留。
    expect(points[0][DIMENSION_SERIES_VALUES].get('bucket')).toBe(15);
    expect(points[0][DIMENSION_SERIES_VALUES].get('__proto__')).toBe(20);
    expect(points[1][DIMENSION_SERIES_VALUES].get('__proto__')).toBe(2);
    expect(points[1][DIMENSION_SERIES_VALUES].get('bucket')).toBe(0);

    // 撞名序列在图表中仍以独立折线渲染，不丢序列、不抛错。
    const { container } = mount(
      <DimensionChart
        dimension='model'
        rows={collisionRows}
        view='trend'
        topN={10}
        onViewChange={() => {}}
        onTopNChange={() => {}}
      />
    );
    expect(container.querySelectorAll('.recharts-line').length).toBe(2);
  });

  it('builds descending proportion totals and limits top data to topN', () => {
    const proportion = buildDimensionProportionData(aggregateRows);
    expect(proportion[0]).toEqual({ key: 'gpt-4o', value: 1300 });
    expect(proportion[1]).toEqual({ key: 'deepseek-v4', value: 500 });

    expect(buildDimensionTopData(aggregateRows, 1)).toEqual([proportion[0]]);
  });

  it('shows request and quota context alongside tokens in the tooltip', () => {
    const { container } = mount(
      <DimensionTooltip
        active
        label='2026-09-23 14:00'
        labelKey='dashboard.statistics.tooltip.date'
        payload={[{ name: 'gpt-4o', value: 1300, color: '#4318FF' }]}
        resolveKey={(entry) => entry.name}
        resolveContext={() => ({ requests: 12, quota: 345000, tokens: 1300 })}
      />
    );

    const text = container.textContent;
    // 通用度量：total tokens 值。
    expect(text).toContain('1,300');
    // 契约要求 tooltip 仍可从聚合行呈现 request 与 quota 上下文。
    expect(text).toContain('dashboard.units.tokens');
    expect(text).toContain('dashboard.units.requests');
    expect(text).toContain('12');
    expect(text).toContain('dashboard.units.quota');
    expect(text).toContain('345.0k');
    // 趋势视图表头按日期语义呈现。
    expect(text).toContain('dashboard.statistics.tooltip.date: 2026-09-23 14:00');
  });

  it('omits the tooltip header when the view has no label semantics', () => {
    const { container } = mount(
      <DimensionTooltip
        active
        label='gpt-4o'
        payload={[{ name: 'gpt-4o', value: 1300, color: '#4318FF' }]}
        resolveKey={(entry) => entry.name}
      />
    );

    // 占比视图无 activeLabel 语义：不传 labelKey 时不渲染表头，避免张冠李戴。
    expect(container.querySelector('.dimension-tooltip-label')).toBeNull();
  });

  it('labels the top view tooltip header by dimension key instead of date', async () => {
    let mounted;
    await act(async () => {
      mounted = mount(
        <DimensionChart
          dimension='model'
          rows={aggregateRows}
          view='top'
          topN={10}
          onViewChange={() => {}}
          onTopNChange={() => {}}
        />
      );
    });

    const { container } = mounted;
    await act(async () => {
      hoverChart(container, { x: 300, y: 60 });
    });

    const tooltip = container.querySelector('.dimension-tooltip');
    expect(tooltip).not.toBeNull();
    // Top 视图的 activeLabel 是维度键（gpt-4o 为最高值，居首行）：
    // 表头必须按维度语义呈现，绝不能标成「日期」。
    expect(tooltip.textContent).toContain(
      'dashboard.statistics.tooltip.dimension: gpt-4o'
    );
    expect(tooltip.textContent).not.toContain(
      'dashboard.statistics.tooltip.date'
    );

    unmount(mounted);
  });

  it('labels the trend view tooltip header by date bucket', async () => {
    let mounted;
    await act(async () => {
      mounted = mount(
        <DimensionChart
          dimension='model'
          rows={aggregateRows}
          view='trend'
          topN={10}
          onViewChange={() => {}}
          onTopNChange={() => {}}
        />
      );
    });

    const { container } = mounted;
    await act(async () => {
      hoverChart(container, { x: 300, y: 150 });
    });

    const tooltip = container.querySelector('.dimension-tooltip');
    expect(tooltip).not.toBeNull();
    // 趋势视图的 activeLabel 是时间 bucket：表头按日期语义呈现。
    expect(tooltip.textContent).toContain('dashboard.statistics.tooltip.date: ');
    expect(tooltip.textContent).not.toContain(
      'dashboard.statistics.tooltip.dimension'
    );

    unmount(mounted);
  });
});

describe('DimensionChart views and preferences', () => {
  const clickSection = async (container, key) => {
    await act(async () => {
      container
        .querySelector(`[data-section="${key}"]`)
        .dispatchEvent(new MouseEvent('click', { bubbles: true }));
    });
  };

  it('loadDashboardChartPreferences returns contract defaults for absent storage', () => {
    expect(window.localStorage.getItem(PREFERENCES_STORAGE_KEY)).toBeNull();
    expect(loadDashboardChartPreferences()).toEqual(DEFAULT_PREFERENCES);
  });

  it('saveDashboardChartPreference writes the single contract key without discarding other dimensions', () => {
    saveDashboardChartPreference('model', { view: 'top', topN: 50 });

    // 唯一存储 key：dashboard_chart_preferences。
    expect(window.localStorage.getItem(PREFERENCES_STORAGE_KEY)).not.toBeNull();
    expect(storedPreferences()).toEqual({
      ...DEFAULT_PREFERENCES,
      model: { view: 'top', topN: 50 },
    });

    // 更新另一维度不得丢弃既有维度偏好。
    saveDashboardChartPreference('token', { view: 'proportion', topN: 5 });
    expect(storedPreferences()).toEqual({
      ...DEFAULT_PREFERENCES,
      model: { view: 'top', topN: 50 },
      token: { view: 'proportion', topN: 5 },
    });
  });

  it('persists the view change for the active dimension only', async () => {
    let mounted;
    await act(async () => {
      mounted = mount(<Dashboard />);
    });

    const { container } = mounted;
    await clickSection(container, 'model');
    const proportionButton = findButton(container, 'dashboard.views.proportion');
    await act(async () => {
      proportionButton.dispatchEvent(new MouseEvent('click', { bubbles: true }));
    });

    expect(storedPreferences().model).toEqual({ view: 'proportion', topN: 10 });
    // 其他维度保持契约默认，未被本次变更污染。
    expect(storedPreferences().token).toEqual({ view: 'trend', topN: 10 });

    unmount(mounted);
  });

  it('persists the Top-N change for the active dimension only', async () => {
    let mounted;
    await act(async () => {
      mounted = mount(<Dashboard />);
    });

    const { container } = mounted;
    await clickSection(container, 'model');
    await selectTopN(container, 20);

    expect(storedPreferences().model).toEqual({ view: 'trend', topN: 20 });
    expect(storedPreferences().channel).toEqual({ view: 'trend', topN: 10 });

    unmount(mounted);
  });

  it('restores per-dimension view and Top-N preferences after remount', async () => {
    let mounted;
    await act(async () => {
      mounted = mount(<Dashboard />);
    });

    // 模型维度：切占比视图 + Top-N 20。
    await clickSection(mounted.container, 'model');
    await act(async () => {
      findButton(mounted.container, 'dashboard.views.proportion').dispatchEvent(
        new MouseEvent('click', { bubbles: true })
      );
    });
    await selectTopN(mounted.container, 20);
    unmount(mounted);

    // 重新挂载：偏好应从 localStorage 恢复。
    let remounted;
    await act(async () => {
      remounted = mount(<Dashboard />);
    });
    await clickSection(remounted.container, 'model');

    expect(
      remounted.container.querySelector('[data-view]').dataset.view
    ).toBe('proportion');
    expect(
      remounted.container.querySelector('.dimension-topn-dropdown .divider.text')
        .textContent
    ).toBe('20');

    unmount(remounted);
  });

  it('keeps preferences independent per dimension across remount', async () => {
    let mounted;
    await act(async () => {
      mounted = mount(<Dashboard />);
    });

    // 模型维度切 Top 视图；令牌维度保持默认趋势视图。
    await clickSection(mounted.container, 'model');
    await act(async () => {
      findButton(mounted.container, 'dashboard.views.top').dispatchEvent(
        new MouseEvent('click', { bubbles: true })
      );
    });
    unmount(mounted);

    let remounted;
    await act(async () => {
      remounted = mount(<Dashboard />);
    });

    // 模型维度恢复为 top。
    await clickSection(remounted.container, 'model');
    expect(
      remounted.container.querySelector('[data-view]').dataset.view
    ).toBe('top');

    // 令牌维度仍是契约默认 trend，未被模型维度影响。
    await clickSection(remounted.container, 'token');
    expect(
      remounted.container.querySelector('[data-view]').dataset.view
    ).toBe('trend');

    unmount(remounted);
  });

  it('degrades malformed stored content to defaults without blocking render', async () => {
    // 畸形 JSON。
    window.localStorage.setItem(PREFERENCES_STORAGE_KEY, '{not valid json');
    expect(loadDashboardChartPreferences()).toEqual(DEFAULT_PREFERENCES);

    // 合法 JSON 但字段非法：视图与 Top-N 均回落默认。
    window.localStorage.setItem(
      PREFERENCES_STORAGE_KEY,
      JSON.stringify({ model: { view: 'pie', topN: 3 }, token: 'oops' })
    );
    expect(loadDashboardChartPreferences()).toEqual(DEFAULT_PREFERENCES);

    // 页面照常渲染，不因存储畸形而崩溃。
    let mounted;
    await act(async () => {
      mounted = mount(<Dashboard />);
    });
    expect(mounted.container.textContent).toContain(
      'dashboard.summary.total_requests'
    );
    unmount(mounted);
  });

  it('degrades to defaults when localStorage access throws', () => {
    const getItem = jest
      .spyOn(window.localStorage.__proto__, 'getItem')
      .mockImplementation(() => {
        throw new Error('storage disabled');
      });

    expect(loadDashboardChartPreferences()).toEqual(DEFAULT_PREFERENCES);

    // 写入同样被吞掉：不抛出、不阻断渲染。
    const setItem = jest
      .spyOn(window.localStorage.__proto__, 'setItem')
      .mockImplementation(() => {
        throw new Error('storage disabled');
      });
    expect(() =>
      saveDashboardChartPreference('model', { view: 'top', topN: 5 })
    ).not.toThrow();

    getItem.mockRestore();
    setItem.mockRestore();
  });

  it('ignores an unknown dimension on save', () => {
    saveDashboardChartPreference('unknown', { view: 'top', topN: 5 });
    expect(window.localStorage.getItem(PREFERENCES_STORAGE_KEY)).toBeNull();
  });
});

describe('TrendCharts overview', () => {
  it('renders requests, quota, and tokens as three separate trends', () => {
    const { container } = mount(
      <TrendCharts rows={aggregateRows} granularity='day' />
    );

    expect(container.querySelectorAll('.recharts-line').length).toBe(3);
    expect(container.textContent).toContain('dashboard.charts.requests.title');
    expect(container.textContent).toContain('dashboard.charts.quota.title');
    expect(container.textContent).toContain('dashboard.charts.tokens.title');
  });

  it('uses backend bucket labels verbatim for hour granularity', () => {
    const { container } = mount(
      <TrendCharts rows={aggregateRows} granularity='hour' />
    );

    expect(container.textContent).toContain('2026-09-23 14:00');
    expect(container.textContent).toContain('2026-09-23 15:00');
  });

  it('uses backend bucket labels verbatim for day and week granularity', () => {
    const dayRows = [
      { ...aggregateRows[0], bucket: '2026-09-23' },
      { ...aggregateRows[1], bucket: '2026-09-24' },
    ];
    const dayMount = mount(<TrendCharts rows={dayRows} granularity='day' />);
    expect(dayMount.container.textContent).toContain('2026-09-23');
    expect(dayMount.container.textContent).toContain('2026-09-24');
    expect(
      dayMount.container.querySelector('[data-granularity="day"]')
    ).not.toBeNull();

    // week 桶标签形如 YYYY-MM-DD（周一），必须原样呈现而非按天重新解析。
    const weekRows = [
      { ...aggregateRows[0], bucket: '2026-09-21' },
      { ...aggregateRows[1], bucket: '2026-09-28' },
    ];
    const weekMount = mount(<TrendCharts rows={weekRows} granularity='week' />);
    expect(weekMount.container.textContent).toContain('2026-09-21');
    expect(weekMount.container.textContent).toContain('2026-09-28');
    expect(
      weekMount.container.querySelector('[data-granularity="week"]')
    ).not.toBeNull();
  });

  it('builds per-bucket totals without mutating rows', () => {
    const input = [...aggregateRows];
    const snapshot = JSON.parse(JSON.stringify(input));

    const points = buildOverviewTrendData(input);
    expect(points).toEqual([
      { bucket: '2026-09-23 14:00', requests: 12, quota: 345000, tokens: 1300 },
      { bucket: '2026-09-23 15:00', requests: 3, quota: 120000, tokens: 500 },
    ]);
    expect(input).toEqual(snapshot);
  });
});

describe('Dashboard translations', () => {
  // 收集对象的所有叶子 key 路径（如 dashboard.summary.total_requests）。
  const leafPaths = (value, prefix = '') => {
    if (value === null || typeof value !== 'object' || Array.isArray(value)) {
      return [prefix];
    }
    return Object.entries(value).flatMap(([key, child]) =>
      leafPaths(child, prefix ? `${prefix}.${key}` : key)
    );
  };

  // 5.1 契约要求的 key 路径：分区名、筛选器、KPI 标签、图表类型、Top-N、空状态、错误、单位标签。
  const REQUIRED_PATHS = [
    'dashboard.panes.overview',
    'dashboard.panes.model',
    'dashboard.panes.token',
    'dashboard.panes.channel',
    'dashboard.panes.user',
    'dashboard.filters.range',
    'dashboard.filters.presets.1',
    'dashboard.filters.presets.7',
    'dashboard.filters.presets.14',
    'dashboard.filters.presets.29',
    'dashboard.filters.presets.custom',
    'dashboard.filters.start',
    'dashboard.filters.end',
    'dashboard.filters.granularity',
    'dashboard.filters.granularities.hour',
    'dashboard.filters.granularities.day',
    'dashboard.filters.granularities.week',
    'dashboard.filters.username',
    'dashboard.filters.query',
    'dashboard.summary.total_requests',
    'dashboard.summary.total_quota',
    'dashboard.summary.total_tokens',
    'dashboard.summary.cached_tokens',
    'dashboard.summary.cache_hit_rate',
    'dashboard.summary.avg_ttft',
    'dashboard.summary.avg_elapsed',
    'dashboard.summary.avg_rpm',
    'dashboard.summary.avg_tpm',
    'dashboard.dimensions.model',
    'dashboard.dimensions.channel',
    'dashboard.dimensions.token',
    'dashboard.dimensions.user',
    'dashboard.views.trend',
    'dashboard.views.proportion',
    'dashboard.views.top',
    'dashboard.topN',
    'dashboard.empty',
    'dashboard.error',
    'dashboard.units.requests',
    'dashboard.units.quota',
    'dashboard.units.tokens',
    'dashboard.units.milliseconds',
    'dashboard.units.rpm',
    'dashboard.units.tpm',
    'dashboard.statistics.tooltip.dimension',
  ];

  // 既有 dashboard key 不得在本次扩展中丢失。
  const EXISTING_PATHS = [
    'dashboard.charts.requests.title',
    'dashboard.charts.requests.tooltip',
    'dashboard.charts.quota.title',
    'dashboard.charts.quota.tooltip',
    'dashboard.charts.tokens.title',
    'dashboard.charts.tokens.tooltip',
    'dashboard.statistics.title',
    'dashboard.statistics.tooltip.date',
    'dashboard.statistics.tooltip.value',
  ];

  it('provides every required dashboard key in both languages', () => {
    const zhPaths = new Set(leafPaths(zhTranslation.dashboard, 'dashboard'));
    const enPaths = new Set(leafPaths(enTranslation.dashboard, 'dashboard'));

    REQUIRED_PATHS.forEach((path) => {
      expect(zhPaths.has(path)).toBe(true);
      expect(enPaths.has(path)).toBe(true);
    });
  });

  it('keeps Chinese and English dashboard key paths one-to-one', () => {
    const zhPaths = leafPaths(zhTranslation.dashboard, 'dashboard').sort();
    const enPaths = leafPaths(enTranslation.dashboard, 'dashboard').sort();

    // 无缺失、无多余：两侧 key 集合完全一致。
    expect(zhPaths).toEqual(enPaths);
  });

  it('preserves every pre-existing dashboard key', () => {
    const zhPaths = new Set(leafPaths(zhTranslation.dashboard, 'dashboard'));
    const enPaths = new Set(leafPaths(enTranslation.dashboard, 'dashboard'));

    EXISTING_PATHS.forEach((path) => {
      expect(zhPaths.has(path)).toBe(true);
      expect(enPaths.has(path)).toBe(true);
    });
  });

  it('re-renders navigation and filter labels when the language changes', async () => {
    // 接线验证：主导航与筛选器文案必须经 t() 解析，随语言切换而变化。
    // 先以英文资源渲染，再切到中文，断言同一组件实例上的文案随之改变。
    isAdmin.mockReturnValue(true);
    mockI18n.language = 'en';

    let mounted;
    await act(async () => {
      mounted = mount(<Dashboard />);
    });

    const { container, root } = mounted;
    expect(container.textContent).toContain('Overview');
    expect(container.textContent).toContain('Model Analysis');
    expect(container.textContent).toContain('Token Analysis');
    expect(container.textContent).toContain('Channel Analysis');
    expect(container.textContent).toContain('User Analysis');
    expect(container.textContent).toContain('Time Range');
    expect(container.textContent).toContain('Granularity');
    expect(container.textContent).toContain('Query');
    expect(container.textContent).toContain('7 days');
    expect(container.textContent).toContain('Custom');
    // 英文 locale 下不得再出现硬编码中文文案。
    expect(container.textContent).not.toContain('概览');
    expect(container.textContent).not.toContain('查询');

    await act(async () => {
      await mockI18n.changeLanguage('zh');
      root.render(<Dashboard />);
    });

    expect(container.textContent).toContain('概览');
    expect(container.textContent).toContain('模型分析');
    expect(container.textContent).toContain('令牌分析');
    expect(container.textContent).toContain('渠道分析');
    expect(container.textContent).toContain('用户分析');
    expect(container.textContent).toContain('时间范围');
    expect(container.textContent).toContain('粒度');
    expect(container.textContent).toContain('查询');
    expect(container.textContent).toContain('7天');
    expect(container.textContent).toContain('自定义');
    expect(container.textContent).not.toContain('Overview');

    unmount(mounted);
  });
});
