// Dashboard 图表偏好持久化边界：把「每个维度各自的图表类型与 Top-N」写入 localStorage，
// 供页面重新挂载后恢复。本模块只负责读写与校验，不持有 React 状态、不发起请求。
//
// 契约：
// - 唯一 key：dashboard_chart_preferences，value 为 JSON 编码的 DashboardChartPreferences。
// - 每个维度（model/token/channel/user）各持 { view, topN }。
// - 默认：每维度 { view: 'trend', topN: 10 }。
// - 任何读写异常（JSON 畸形、localStorage 不可用、字段非法）都必须降级为契约默认值，
//   绝不阻止 Dashboard 渲染。

/** @typedef {'model'|'channel'|'token'|'user'} DashboardDimension */
/** @typedef {'trend'|'proportion'|'top'} DashboardChartView */
/** @typedef {5|10|20|50} DashboardTopN */

/**
 * @typedef {Object} DashboardChartPreference
 * @property {DashboardChartView} view
 * @property {DashboardTopN} topN
 */

/**
 * @typedef {Object} DashboardChartPreferences
 * @property {DashboardChartPreference} model
 * @property {DashboardChartPreference} token
 * @property {DashboardChartPreference} channel
 * @property {DashboardChartPreference} user
 */

const STORAGE_KEY = 'dashboard_chart_preferences';

// 闭集：维度、视图、Top-N 的合法取值；不在集合内的字段一律回落默认值。
const DIMENSIONS = ['model', 'token', 'channel', 'user'];
const VIEWS = ['trend', 'proportion', 'top'];
const TOP_N_OPTIONS = [5, 10, 20, 50];
const DEFAULT_VIEW = 'trend';
const DEFAULT_TOP_N = 10;

/** 构造一份全新的契约默认偏好（每个维度独立副本，避免调用方之间互相污染）。 */
function createDefaultPreferences() {
  const preferences = {};
  DIMENSIONS.forEach((dimension) => {
    preferences[dimension] = { view: DEFAULT_VIEW, topN: DEFAULT_TOP_N };
  });
  return preferences;
}

/** 把任意输入规范化为合法偏好：缺失或不在闭集内的字段回落默认值。 */
function normalizePreference(value) {
  const source = value && typeof value === 'object' ? value : {};
  return {
    view: VIEWS.includes(source.view) ? source.view : DEFAULT_VIEW,
    topN: TOP_N_OPTIONS.includes(source.topN) ? source.topN : DEFAULT_TOP_N,
  };
}

/** 把任意输入规范化为完整的 DashboardChartPreferences；未知维度被丢弃。 */
function normalizePreferences(value) {
  const source = value && typeof value === 'object' ? value : {};
  const preferences = {};
  DIMENSIONS.forEach((dimension) => {
    preferences[dimension] = normalizePreference(source[dimension]);
  });
  return preferences;
}

/** 安全读取原始存储值；localStorage 不可用（含无 window 环境）时返回 null。 */
function readRawStorage() {
  try {
    if (typeof window === 'undefined' || !window.localStorage) return null;
    return window.localStorage.getItem(STORAGE_KEY);
  } catch (error) {
    return null;
  }
}

/**
 * 读取并校验逐维度图表偏好；存储缺失或内容畸形一律回落契约默认值。
 * @returns {DashboardChartPreferences}
 */
export function loadDashboardChartPreferences() {
  const raw = readRawStorage();
  if (!raw) return createDefaultPreferences();
  try {
    return normalizePreferences(JSON.parse(raw));
  } catch (error) {
    return createDefaultPreferences();
  }
}

/**
 * 持久化单个维度的图表偏好，且不丢弃其他维度的既有偏好。
 * 维度非法或写入失败时静默返回，绝不阻断 Dashboard 渲染。
 * @param {DashboardDimension} dimension
 * @param {DashboardChartPreference} preference
 */
export function saveDashboardChartPreference(dimension, preference) {
  if (!DIMENSIONS.includes(dimension)) return;
  try {
    if (typeof window === 'undefined' || !window.localStorage) return;
    const preferences = loadDashboardChartPreferences();
    preferences[dimension] = normalizePreference(preference);
    window.localStorage.setItem(STORAGE_KEY, JSON.stringify(preferences));
  } catch (error) {
    // 持久化失败不得阻止 Dashboard 渲染。
  }
}
