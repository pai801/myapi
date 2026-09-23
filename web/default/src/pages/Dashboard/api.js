import { API, isAdmin } from '../../helpers';

// Dashboard 传输边界：页面（index.js）持有筛选状态与数据，本模块只负责把已校验的
// 页面状态值拼成查询串、走既有 Axios 边界（helpers/api.js 的 API 实例）发请求，
// 并只解包「成功」信封。传输/API 失败一律 reject，从而与「成功但为空」可区分——
// 页面据此渲染错误态而非空态。

const AGGREGATE_PATH = '/api/user/dashboard/aggregate';
const SUMMARY_PATH = '/api/user/dashboard/summary';

// username 仅当当前用户是管理员且 trim 后非空时发送；普通用户即便 UI 残留该值也永不携带，
// 避免越权扩大数据范围。
function appendUsernameScope(params, username) {
  if (isAdmin() && typeof username === 'string' && username.trim() !== '') {
    params.push(`username=${encodeURIComponent(username.trim())}`);
  }
}

function buildRequestUrl(path, params) {
  return params.length > 0 ? `${path}?${params.join('&')}` : path;
}

// 只接受 success=true 的信封；其余（success=false、畸形响应）一律视为失败。
async function requestDashboardData(path, params) {
  const response = await API.get(buildRequestUrl(path, params));
  const { success, data } = response.data || {};
  if (!success) {
    throw new Error('dashboard request rejected');
  }
  return data;
}

/**
 * 拉取扁平的 dashboard 聚合行。query 均为已校验的页面状态值。
 * @param {{dimension: string, granularity: string, startTimestamp: number, endTimestamp: number, username?: string}} query
 * @returns {Promise<Array>} 空数据规范化为 []
 */
export async function fetchDashboardAggregate(query) {
  const { dimension, granularity, startTimestamp, endTimestamp, username } =
    query || {};
  const params = [
    `dimension=${encodeURIComponent(dimension)}`,
    `granularity=${encodeURIComponent(granularity)}`,
    `start_timestamp=${startTimestamp}`,
    `end_timestamp=${endTimestamp}`,
  ];
  appendUsernameScope(params, username);

  const data = await requestDashboardData(AGGREGATE_PATH, params);
  return Array.isArray(data) ? data : [];
}

/**
 * 拉取 dashboard KPI 汇总对象。query 均为已校验的页面状态值。
 * @param {{granularity: string, startTimestamp: number, endTimestamp: number, username?: string}} query
 * @returns {Promise<Object|null>}
 */
export async function fetchDashboardSummary(query) {
  const { granularity, startTimestamp, endTimestamp, username } = query || {};
  const params = [
    `granularity=${encodeURIComponent(granularity)}`,
    `start_timestamp=${startTimestamp}`,
    `end_timestamp=${endTimestamp}`,
  ];
  appendUsernameScope(params, username);

  return requestDashboardData(SUMMARY_PATH, params);
}
