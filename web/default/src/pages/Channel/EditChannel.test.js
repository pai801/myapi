import React from 'react';
import { createRoot } from 'react-dom/client';
import { act } from 'react-dom/test-utils';

// 本测试直接用 react-dom/client + test-utils 的 act（项目未安装 @testing-library），
// 因此 testing-library 的 act 规则在此属误报。
/* eslint-disable testing-library/no-unnecessary-act */

// React 18 需要显式声明处于 act 环境，否则每次 act() 都会打印未包裹警告。
global.IS_REACT_ACT_ENVIRONMENT = true;

// EditChannel 的配置所有权契约测试：同一 harness 同时守护两条边界。
//
// 1) descriptor 请求头键：渠道 config JSON 里 custom headers 的持久化键名由后端下发的 descriptor
//    声明（capabilities.custom_headers_key），前端据此渲染编辑器并在提交时写入该键名，绝不硬编码
//    config 的持久化键名；descriptor 一变，行为必须变。
// 2) 自由扩展配置：自定义配置编辑器只拥有系统键与 descriptor 请求头键之外的扩展键——载入只回显
//    扩展键，提交以当前扩展对象整体替换旧扩展键（清空即删除），系统键与请求头键不得被其覆盖或删除。
//
// 说明：i18n 被 mock 成 t:(k)=>k，故页面文本即 i18n 键本身，便于用键名断言。
// CRA 的 jest 配置开启了 resetMocks，工厂里的实现会被逐用例清空，
// 因此所有 mockImplementation 一律在 beforeEach 里重新装配。

// i18n 的 t 用可变 mock 承载：既保持 t:(k)=>k 的既有语义，又能断言冲突键确实传给了 t。
let mockT;

// navigate 用可变 mock 承载：断言保存成功后才跳转回渠道列表。
let mockNavigate;

jest.mock('react-i18next', () => ({
  useTranslation: () => ({ t: mockT, i18n: { language: 'zh' } }),
}));

// 走「加载已有渠道」路径：useParams 返回 id，页面进入 isEdit 分支。
jest.mock('react-router-dom', () => ({
  useParams: () => ({ id: '1' }),
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
  showError,
  showSuccess,
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
const CUSTOM_CONFIG_TITLE_KEY = 'channel.edit.custom_config.title';
const INVALID_JSON_ERROR_KEY = 'channel.edit.custom_config.invalid_json';
const NOT_OBJECT_ERROR_KEY = 'channel.edit.custom_config.not_object';
const CONFLICT_ERROR_KEY = 'channel.edit.custom_config.conflict';
const SUBMIT_KEY = 'channel.edit.buttons.submit';
const UPDATE_SUCCESS_KEY = 'channel.edit.messages.update_success';
// 一个不该被写入的硬编码键名（中性占位：真实键名由清单下发，本常量只用于反证硬编码路径）。
const HARDCODED_KEY = 'hardcoded_custom_headers';

// 与实现契约一致的 9 个系统键（由既有专属表单拥有，扩展编辑器必须排除）。
const SYSTEM_CONFIG_KEYS = [
  'region',
  'sk',
  'ak',
  'user_id',
  'api_version',
  'library_id',
  'plugin',
  'vertex_ai_project_id',
  'vertex_ai_adc',
];

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
    mockT = jest.fn((k) => k);
    mockNavigate = jest.fn();
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

  function findCustomConfigTextArea() {
    return container.querySelector('textarea[name="custom_config"]');
  }

  // 卸载当前页面并换新容器/根，供同一用例内覆盖多组输入（组件状态需重置）。
  async function remountPage() {
    act(() => root.unmount());
    container.remove();
    container = document.createElement('div');
    document.body.appendChild(container);
    root = createRoot(container);
  }

  // 受控文本域必须走原生 value setter + input 事件，React 才能观察到这次变更。
  async function changeCustomConfigText(value) {
    const textarea = findCustomConfigTextArea();
    if (!textarea) throw new Error('custom config textarea not found');
    await act(async () => {
      const valueSetter = Object.getOwnPropertyDescriptor(
        window.HTMLTextAreaElement.prototype,
        'value'
      ).set;
      valueSetter.call(textarea, value);
      textarea.dispatchEvent(new Event('input', { bubbles: true }));
    });
  }

  async function clickSubmit() {
    const submitButton = findSubmitButton();
    expect(submitButton).toBeTruthy();
    await act(async () => {
      submitButton.dispatchEvent(new MouseEvent('click', { bubbles: true }));
      await new Promise((resolve) => setTimeout(resolve, 0));
    });
  }

  // 渠道类型下拉是 semantic-ui Dropdown（非原生 select），需点选菜单项来驱动 onChange。
  async function changeChannelType(label) {
    const dropdown = container.querySelector('div[name="type"]');
    if (!dropdown) throw new Error('channel type dropdown not found');
    const items = Array.from(dropdown.querySelectorAll('.item'));
    const target = items.find((item) => item.textContent.trim() === label);
    if (!target) throw new Error(`channel type option not found: ${label}`);
    await act(async () => {
      target.dispatchEvent(new MouseEvent('click', { bubbles: true }));
      await new Promise((resolve) => setTimeout(resolve, 0));
    });
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

  it('renders custom config editor for every channel type without descriptor capability', async () => {
    // 输入一：完全没有 descriptor（清单缺失）。
    mockDescriptor = null;
    mockChannelData = channelResponse('{}');

    await renderPage();

    expect(container.textContent).toContain(CUSTOM_CONFIG_TITLE_KEY);
    expect(findCustomConfigTextArea()).toBeTruthy();

    // 输入二：descriptor 存在但不支持自定义请求头。
    await remountPage();
    mockDescriptor = descriptorWith({
      supports_custom_headers: false,
      custom_headers_key: KEY,
    });
    mockChannelData = channelResponse('{}');

    await renderPage();

    expect(container.textContent).toContain(CUSTOM_CONFIG_TITLE_KEY);
    expect(findCustomConfigTextArea()).toBeTruthy();
  });

  it('loads only extension keys and pretty-prints them', async () => {
    const extensionConfig = {
      custom_flag: { nested: { a: 1 }, list: [1, 2], num: 3, bool: true },
    };
    mockDescriptor = descriptorWith({
      supports_custom_headers: true,
      custom_headers_key: KEY,
    });
    mockChannelData = channelResponse(
      JSON.stringify({
        region: 'us-east-1',
        [KEY]: { Authorization: 'Bearer secret' },
        ...extensionConfig,
      })
    );

    await renderPage();

    const textarea = findCustomConfigTextArea();
    expect(textarea).toBeTruthy();
    // 只有扩展键进入编辑器，且按两空格缩进格式化。
    expect(textarea.value).toBe(JSON.stringify(extensionConfig, null, 2));
    expect(textarea.value).toContain('\n  "custom_flag"');
    expect(textarea.value).toContain('\n    "nested"');
    // 系统键与 descriptor 请求头键不得出现在编辑器文本里。
    expect(textarea.value).not.toContain('region');
    expect(textarea.value).not.toContain(KEY);

    // 覆盖非法存量 config：编辑器留空，既有失败态继续阻止提交。
    await remountPage();
    mockDescriptor = descriptorWith({
      supports_custom_headers: true,
      custom_headers_key: KEY,
    });
    mockChannelData = channelResponse('{not valid json');

    await renderPage();

    expect(findCustomConfigTextArea().value).toBe('');

    await clickSubmit();

    expect(API.put).not.toHaveBeenCalled();
    expect(API.post).not.toHaveBeenCalled();
  });

  it('submits merged system and extension keys with JSON types preserved and compact JSON', async () => {
    mockDescriptor = null;
    mockChannelData = channelResponse(JSON.stringify({ region: 'us-east-1' }));

    await renderPage();

    const extensionConfig = {
      custom_obj: { nested: { deep: 'v' } },
      custom_arr: [1, 'two', false],
      custom_num: 42,
      custom_bool: true,
    };
    await changeCustomConfigText(JSON.stringify(extensionConfig));

    await clickSubmit();

    expect(API.put).toHaveBeenCalledTimes(1);
    const payload = API.put.mock.calls[0][1];
    const parsedConfig = JSON.parse(payload.config);

    // 系统键与扩展键并存，系统键值未被编辑器影响。
    expect(parsedConfig.region).toBe('us-east-1');
    expect(Object.keys(parsedConfig).sort()).toEqual(
      ['region', ...Object.keys(extensionConfig)].sort()
    );
    // 扩展值的 JSON 类型保持（嵌套对象 / 数组 / 数字 / 布尔）。
    expect(parsedConfig.custom_obj).toEqual({ nested: { deep: 'v' } });
    expect(parsedConfig.custom_arr).toEqual([1, 'two', false]);
    expect(parsedConfig.custom_num).toBe(42);
    expect(typeof parsedConfig.custom_num).toBe('number');
    expect(parsedConfig.custom_bool).toBe(true);
    expect(typeof parsedConfig.custom_bool).toBe('boolean');

    // 紧凑序列化：payload 与无缩进 stringify 逐字一致，且不含格式化换行。
    expect(payload.config).toBe(JSON.stringify(parsedConfig));
    expect(payload.config).not.toContain('\n');
  });

  it('blocks submission for invalid JSON and non-object top-level values', async () => {
    const cases = [
      { text: '{not valid json', errorKey: INVALID_JSON_ERROR_KEY },
      { text: '[1, 2, 3]', errorKey: NOT_OBJECT_ERROR_KEY },
      { text: '"just a string"', errorKey: NOT_OBJECT_ERROR_KEY },
      { text: '42', errorKey: NOT_OBJECT_ERROR_KEY },
      { text: 'true', errorKey: NOT_OBJECT_ERROR_KEY },
      { text: 'null', errorKey: NOT_OBJECT_ERROR_KEY },
    ];

    for (const { text, errorKey } of cases) {
      await remountPage();
      mockDescriptor = descriptorWith({
        supports_custom_headers: true,
        custom_headers_key: KEY,
      });
      mockChannelData = channelResponse('{}');

      await renderPage();

      showError.mockClear();
      await changeCustomConfigText(text);
      await clickSubmit();

      expect(showError).toHaveBeenCalledWith(errorKey);
      expect(API.put).not.toHaveBeenCalled();
      expect(API.post).not.toHaveBeenCalled();
    }
  });

  it('blocks submission when a custom key conflicts with an owned key', async () => {
    // 9 个系统键全部作为冲突来源，外加 descriptor 下发的请求头键。
    const ownedKeys = [...SYSTEM_CONFIG_KEYS, KEY];

    for (const ownedKey of ownedKeys) {
      await remountPage();
      mockDescriptor = descriptorWith({
        supports_custom_headers: true,
        custom_headers_key: KEY,
      });
      mockChannelData = channelResponse(
        JSON.stringify({
          region: 'us-east-1',
          [KEY]: { Authorization: 'Bearer secret' },
        })
      );

      await renderPage();

      showError.mockClear();
      mockT.mockClear();
      await changeCustomConfigText(JSON.stringify({ [ownedKey]: 'evil' }));
      await clickSubmit();

      // 冲突键被显式传给 t，错误可见且没有任何提交请求发出（原系统/请求头值未被覆盖）。
      expect(mockT).toHaveBeenCalledWith(CONFLICT_ERROR_KEY, { key: ownedKey });
      expect(showError).toHaveBeenCalledWith(CONFLICT_ERROR_KEY);
      expect(API.put).not.toHaveBeenCalled();
      expect(API.post).not.toHaveBeenCalled();
    }
  });

  it('clearing custom config removes old extension keys and preserves owned keys', async () => {
    mockDescriptor = descriptorWith({
      supports_custom_headers: true,
      custom_headers_key: KEY,
    });
    mockChannelData = channelResponse(
      JSON.stringify({
        region: 'us-east-1',
        [KEY]: { Authorization: 'Bearer secret' },
        old_ext: { x: 1 },
        legacy_flag: 'stale',
      })
    );

    await renderPage();

    const textarea = findCustomConfigTextArea();
    expect(textarea.value).toContain('old_ext');
    expect(textarea.value).toContain('legacy_flag');

    showError.mockClear();
    await changeCustomConfigText('');
    expect(findCustomConfigTextArea().value).toBe('');

    await clickSubmit();

    expect(API.put).toHaveBeenCalledTimes(1);
    const payload = API.put.mock.calls[0][1];
    const parsedConfig = JSON.parse(payload.config);

    // 旧扩展键整体消失，系统键与请求头键和值原样保留。
    expect(parsedConfig).not.toHaveProperty('old_ext');
    expect(parsedConfig).not.toHaveProperty('legacy_flag');
    expect(parsedConfig.region).toBe('us-east-1');
    expect(parsedConfig[KEY]).toEqual({ Authorization: 'Bearer secret' });
    expect(Object.keys(parsedConfig).sort()).toEqual(['region', KEY].sort());
    // 空白文本不得被当作错误。
    expect(showError).not.toHaveBeenCalled();
  });

  it('preserves the excluded headers key when the channel type is switched before submit', async () => {
    // 载入时 descriptor 声明 54 的请求头键为 x_key：该键被排除出编辑器（用户看不到）。
    const desc54 = descriptorWith({
      supports_custom_headers: true,
      custom_headers_key: KEY,
    });
    mockDescriptor = desc54;
    // 用户把类型改到 55（无 descriptor / 无 custom_headers_key），提交时必须仍保留载入时排除过的键。
    findDescriptor.mockImplementation((list, type) =>
      Number(type) === 54 ? desc54 : undefined
    );
    getChannelDescriptor.mockImplementation(() => desc54);
    buildChannelOptions.mockImplementation(() => [
      { key: 54, text: 'TYPE_54', value: 54 },
      { key: 55, text: 'TYPE_55', value: 55 },
    ]);
    mockChannelData = channelResponse(
      JSON.stringify({ region: 'us-east-1', [KEY]: { Authorization: 'Bearer secret' } })
    );

    await renderPage();

    // 编辑器文本必须不含 x_key（载入即被排除，用户不可见）。
    expect(findCustomConfigTextArea().value).not.toContain(KEY);

    await changeChannelType('TYPE_55');

    showError.mockClear();
    await clickSubmit();

    expect(API.put).toHaveBeenCalledTimes(1);
    const payload = API.put.mock.calls[0][1];
    const parsedConfig = JSON.parse(payload.config);

    // 原类型专属请求头键必须原样保留，不能被当作扩展键静默删除。
    expect(parsedConfig).toHaveProperty(KEY);
    expect(parsedConfig[KEY]).toEqual({ Authorization: 'Bearer secret' });
    expect(parsedConfig.region).toBe('us-east-1');
    expect(showError).not.toHaveBeenCalled();
  });

  it('still merges extension keys and preserves excluded/system keys without over-retaining', async () => {
    mockDescriptor = descriptorWith({
      supports_custom_headers: true,
      custom_headers_key: KEY,
    });
    mockChannelData = channelResponse(
      JSON.stringify({
        region: 'us-east-1',
        [KEY]: { Authorization: 'Bearer secret' },
        old_ext: { x: 1 },
      })
    );

    await renderPage();

    const extensionConfig = { new_ext: { deep: true }, new_num: 7 };
    await changeCustomConfigText(JSON.stringify(extensionConfig));

    showError.mockClear();
    await clickSubmit();

    expect(API.put).toHaveBeenCalledTimes(1);
    const payload = API.put.mock.calls[0][1];
    const parsedConfig = JSON.parse(payload.config);

    // 旧扩展键被编辑器接管（整体替换），新扩展键正确合并。
    expect(parsedConfig).not.toHaveProperty('old_ext');
    expect(parsedConfig.new_ext).toEqual({ deep: true });
    expect(parsedConfig.new_num).toBe(7);
    // 排除过的请求头键与系统键未被编辑器接管、未被删除。
    expect(parsedConfig[KEY]).toEqual({ Authorization: 'Bearer secret' });
    expect(parsedConfig.region).toBe('us-east-1');
    // 键集合恰好等于「系统键 + 请求头键 + 新扩展键」，无过度保留。
    expect(Object.keys(parsedConfig).sort()).toEqual(
      ['region', KEY, 'new_ext', 'new_num'].sort()
    );
    expect(showError).not.toHaveBeenCalled();
  });

  it('round-trips a literal __proto__ key without polluting prototypes', async () => {
    // 承载方案：__proto__ 作为自有扩展键可完整往返（载入回显 → 提交落库），且不触发原型 setter。
    mockDescriptor = descriptorWith({
      supports_custom_headers: true,
      custom_headers_key: KEY,
    });
    // 必须用字面 JSON 字符串构造：对象字面量的 __proto__ 会设置原型而非产生自有键。
    mockChannelData = channelResponse('{"__proto__":{"polluted":true}}');

    await renderPage();

    // 载入路径：该键必须进入编辑器文本（不得静默丢失），且全局原型未被污染。
    const textarea = findCustomConfigTextArea();
    expect(textarea.value).toContain('__proto__');
    expect({}.polluted).toBeUndefined();

    showError.mockClear();
    await clickSubmit();

    expect(API.put).toHaveBeenCalledTimes(1);
    const payload = API.put.mock.calls[0][1];
    // 提交路径：键必须以自有属性落库并可被 JSON 序列化（原值往返）。
    expect(payload.config).toContain('__proto__');
    const parsedConfig = JSON.parse(payload.config);
    expect(Object.prototype.hasOwnProperty.call(parsedConfig, '__proto__')).toBe(true);
    expect(parsedConfig['__proto__']).toEqual({ polluted: true });
    // 无原型污染：全局 Object.prototype 未被写入。
    expect({}.polluted).toBeUndefined();
    expect(showError).not.toHaveBeenCalled();
  });

  it('navigates to /channel after a successful edit save', async () => {
    mockDescriptor = null;
    mockChannelData = channelResponse('{}');

    await renderPage();

    expect(mockNavigate).not.toHaveBeenCalled();
    await clickSubmit();

    expect(API.put).toHaveBeenCalledTimes(1);
    expect(showSuccess).toHaveBeenCalledWith(UPDATE_SUCCESS_KEY);
    expect(mockNavigate).toHaveBeenCalledTimes(1);
    expect(mockNavigate).toHaveBeenCalledWith('/channel');
  });

  it('does NOT navigate when the edit save returns success:false', async () => {
    mockDescriptor = null;
    mockChannelData = channelResponse('{}');
    API.put.mockResolvedValue({
      data: { success: false, message: 'save-failed' },
    });

    await renderPage();

    await clickSubmit();

    expect(API.put).toHaveBeenCalledTimes(1);
    expect(showError).toHaveBeenCalledWith('save-failed');
    expect(mockNavigate).not.toHaveBeenCalled();
  });
});
