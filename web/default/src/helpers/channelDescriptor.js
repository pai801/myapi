import { API } from './api';
import { CHANNEL_OPTIONS } from '../constants';

// 渠道能力清单（ChannelDescriptor）前端模块（PRD §5.12 层1 / 决策 D7）。
//
// 后端 GET /api/channel/descriptors 下发渠道元数据：默认构建返回空列表，扩展渠道由扩展方
// 注入。前端不硬编码任何渠道类型 —— 下拉 / 标签 / 面板全部据此清单渲染。
//
// 缓存策略沿用 helpers/utils.js 的 channelModels：模块级缓存 + 显式 load。
// 纯函数（buildChannelOptions / descriptorToOption / findDescriptor …）显式接收清单，
// 便于组件用 state 驱动重渲染；命令式逻辑（提交 / 加载渠道）走 getChannelDescriptor 读缓存。

let descriptors = [];
let byType = null;

// pickI18n 从 i18n map 取当前语言文案，未命中回退到 fallback。
// 语言码可能带区域后缀（如 zh-CN），退化为前缀匹配。
export function pickI18n(map, fallback, lang) {
  if (map && typeof map === 'object') {
    if (lang && map[lang]) return map[lang];
    const base = (lang || '').split('-')[0];
    if (base && map[base]) return map[base];
  }
  return fallback || '';
}

// loadChannelDescriptors 拉取并缓存渠道清单。失败时归空（不阻塞页面），返回当前缓存。
export async function loadChannelDescriptors() {
  try {
    const res = await API.get('/api/channel/descriptors');
    const { success, data } = res.data || {};
    descriptors = success && Array.isArray(data) ? data : [];
  } catch (e) {
    // 错误提示已由 api 拦截器统一弹出，这里仅降级为空清单。
    descriptors = [];
  }
  byType = null;
  return descriptors;
}

// getChannelDescriptors 返回已缓存的清单（未加载时为空数组）。
export function getChannelDescriptors() {
  return Array.isArray(descriptors) ? descriptors : [];
}

// getChannelDescriptor 按渠道类型从缓存取清单；未命中返回 undefined。
export function getChannelDescriptor(type) {
  if (byType === null) {
    byType = new Map();
    getChannelDescriptors().forEach((d) => {
      byType.set(Number(d.channel_type), d);
    });
  }
  return byType.get(Number(type));
}

// findDescriptor 从给定清单里按渠道类型查找（纯函数，供组件 state 驱动）。
export function findDescriptor(list, type) {
  if (!Array.isArray(list)) return undefined;
  return list.find((d) => Number(d.channel_type) === Number(type));
}

// descriptorToOption 把清单转成 semantic-ui 下拉项（与 CHANNEL_OPTIONS 同形）。
export function descriptorToOption(descriptor, lang) {
  return {
    key: descriptor.id,
    text: pickI18n(descriptor.name_i18n, descriptor.name, lang),
    value: descriptor.channel_type,
    color: descriptor.color || 'blue',
  };
}

// buildChannelOptions 合并内置渠道常量与后端下发的清单（按 channel_type 去重）。
// 默认构建清单为空 → 仅内置渠道；扩展渠道由后端追加。
export function buildChannelOptions(list, lang) {
  const options = [...CHANNEL_OPTIONS];
  const known = new Set(options.map((o) => o.value));
  (Array.isArray(list) ? list : []).forEach((d) => {
    const type = Number(d.channel_type);
    if (!known.has(type)) {
      options.push(descriptorToOption(d, lang));
      known.add(type);
    }
  });
  return options;
}

// oauth-* 面板渲染器：由 descriptor 的 OAuth 元数据驱动（PRD §5.12 层2）。
const OAUTH_PANELS = new Set(['oauth-device-code', 'oauth-authorization-code']);

// 已实现的面板渲染器；其余（未知类型）一律回退 manual。
const IMPLEMENTED_PANELS = new Set(['manual', ...OAUTH_PANELS]);

// descriptorOAuth 返回清单声明的 OAuth 元数据（须同时具备 start / poll 路径才可用），
// 否则返回 null。供 oauth 渲染器与 resolvePanelType 共用。
export function descriptorOAuth(descriptor) {
  const oauth = descriptor && descriptor.panel && descriptor.panel.oauth;
  if (!oauth || !oauth.start_path || !oauth.poll_path) return null;
  return oauth;
}

// resolvePanelType 解析实际应使用的面板渲染器类型。
// 未实现的类型回退 manual（密钥框必须保持可手写，PRD §5.12 层2）。
// oauth-* 类型额外要求 OAuth 元数据完整（start/poll 路径齐备），否则也回退 manual ——
// 避免渲染出一个无法发起登录的空面板。
export function resolvePanelType(descriptor) {
  const t = descriptor && descriptor.panel_type;
  if (!IMPLEMENTED_PANELS.has(t)) return 'manual';
  if (OAUTH_PANELS.has(t) && !descriptorOAuth(descriptor)) return 'manual';
  return t;
}

// descriptorKeyPrompt 返回清单声明的手写密钥框提示文案（未声明时为空串）。
export function descriptorKeyPrompt(descriptor, lang) {
  if (!descriptor) return '';
  const panel = descriptor.panel || {};
  return pickI18n(panel.key_prompt_i18n, panel.key_prompt, lang);
}
