// Dashboard 图表共享配置：维度图（DimensionChart）与概览趋势图（TrendCharts）
// 共用的度量口径与 recharts 视觉常量，避免同一份 total tokens 口径与轴/tooltip
// 样式在两个组件中各写一份。仅承载无状态常量与纯函数，不含任何请求或状态。

/** 通用度量：total tokens = prompt_tokens + completion_tokens（不含 cached）。 */
export function totalTokens(row) {
  const prompt = Number(row?.prompt_tokens) || 0;
  const completion = Number(row?.completion_tokens) || 0;
  return prompt + completion;
}

/** recharts 轴刻度样式，与既有 Dashboard 视觉一致。 */
export const AXIS_TICK = { fontSize: 12, fill: '#A3AED0' };

/** recharts tooltip 容器样式，与既有 Dashboard 视觉一致。 */
export const TOOLTIP_STYLE = {
  background: '#fff',
  border: 'none',
  borderRadius: '4px',
  boxShadow: '0 2px 8px rgba(0,0,0,0.1)',
};

/** 数值千分位格式化；空值原样返回，供 tooltip 与图例复用。 */
export function formatCount(value) {
  return value === null || value === undefined
    ? value
    : Number(value).toLocaleString('en-US');
}
